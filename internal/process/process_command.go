package process

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

var ErrStartAborted = fmt.Errorf("aborted")

// cmdWaitDelay is the upper bound the runtime will wait for child I/O to
// drain after the process exits before force-closing the stdout/stderr
// pipes. Required so that cmd.Wait() returns even when a forked grandchild
// inherits and holds the pipes open (e.g. a shell wrapper that backgrounds
// the real binary). killProcess sends the stop signal directly (not via the
// cmd context), so this delay is measured from process exit rather than from
// the stop request, and stays independent of the caller's graceful timeout.
const cmdWaitDelay = 10 * time.Second

// parentCancelGraceTimeout is the graceful timeout used when the process is
// torn down because parentCtx was cancelled (final router teardown or app
// shutdown). In the normal flow the process has already been stopped via
// Stop() by this point, so killProcess is a no-op kill; the short grace just
// bounds the rare case where a process is still alive when its context is cut.
const parentCancelGraceTimeout = time.Second

type runReq struct {
	timeout time.Duration
	respond chan error
}

type stopReq struct {
	timeout time.Duration
	respond chan error
}

type waitReadyReq struct {
	respond chan error
}

type sleepReq struct {
	timeout time.Duration
	respond chan error
}

type wakeReq struct {
	timeout time.Duration
	respond chan error
}

type startResult struct {
	cmd       *exec.Cmd
	cmdDone   chan struct{}
	cancel    context.CancelFunc
	handlerFn http.HandlerFunc
	err       error
}

type ProcessCommand struct {
	id        string
	config    config.ModelConfig
	parentCtx context.Context

	processLogger *logmon.Monitor
	proxyLogger   *logmon.Monitor

	// waitDelay is assigned to cmd.WaitDelay when starting the upstream
	// process. Defaults to cmdWaitDelay; tests override it to keep the
	// pipe-close backstop from dominating their runtime.
	waitDelay time.Duration

	runCh       chan runReq
	stopCh      chan stopReq
	waitReadyCh chan waitReadyReq
	sleepCh     chan sleepReq
	wakeCh      chan wakeReq

	// current ProcessState. Written only by run(); read by State() via atomic load.
	state atomic.Value

	// stores the active reverse-proxy handler when the process is running.
	// Written only by run(); read by ServeHTTP via atomic load.
	handler atomic.Pointer[http.HandlerFunc]

	lastUse  atomic.Int64 // unix nano timestamp of last ServeHTTP completion
	inflight atomic.Int64 // current in-flight ServeHTTP calls
}

var _ Process = (*ProcessCommand)(nil)

func New(
	parentCtx context.Context,
	id string,
	conf config.ModelConfig,
	processLogger *logmon.Monitor,
	proxyLogger *logmon.Monitor,
) (*ProcessCommand, error) {
	p := &ProcessCommand{
		id:            id,
		config:        conf,
		parentCtx:     parentCtx,
		processLogger: processLogger,
		proxyLogger:   proxyLogger,

		runCh:       make(chan runReq),
		stopCh:      make(chan stopReq),
		waitReadyCh: make(chan waitReadyReq),
		sleepCh:     make(chan sleepReq),
		wakeCh:      make(chan wakeReq),
		waitDelay:   cmdWaitDelay,
	}
	p.state.Store(StateStopped)

	go p.run()
	return p, nil
}

func (p *ProcessCommand) Logger() *logmon.Monitor { return p.processLogger }

// run is the single-writer goroutine that owns all mutable lifecycle state
// (current ProcessState, the running *exec.Cmd, the active reverse-proxy
// handler, and the list of WaitReady subscribers). Every public method
// (Run / Stop / State / WaitReady) is a thin client that sends a request on
// one of the channels below and waits for a response — this funnels concurrent
// callers through a single serialization point so the state machine never
// observes a race.
func (p *ProcessCommand) run() {
	// Mutable state — only read/written from this goroutine. ServeHTTP reads
	// p.handler concurrently, which is why handler is an atomic.Pointer.
	// p.state mirrors `state` so State() can observe transitions; setState
	// writes both.
	state := StateStopped
	setState := func(s ProcessState) {
		old := state
		state = s
		p.state.Store(s)
		if old != s {
			event.Emit(shared.ProcessStateChangeEvent{
				ProcessName: p.id,
				OldState:    string(old),
				NewState:    string(s),
			})
		}
	}
	var (
		cmd          *exec.Cmd
		cmdDone      <-chan struct{}
		cmdCancel    context.CancelFunc
		readyWaiters []waitReadyReq
		// runResp parks the in-flight Run caller's response channel. The
		// interface contract is that Run blocks until the process is
		// terminated, so we hold this until Stop, parentCtx, or an
		// upstream exit unblocks it via respondRun.
		runResp chan<- error
	)

	// notifyWaiters wakes every blocked WaitReady caller with the given result.
	// Used on transitions out of StateStarting (ready, failed, aborted, or
	// shutdown) — anything that resolves the "is it ready yet?" question.
	notifyWaiters := func(err error) {
		for _, w := range readyWaiters {
			select {
			case w.respond <- err:
			default:
			}
		}
		readyWaiters = nil
	}

	// respondRun delivers the final Run result, if a Run caller is parked.
	respondRun := func(err error) {
		if runResp != nil {
			runResp <- err
			runResp = nil
		}
	}

	for {
		select {
		// Shutdown: parent context cancelled. Tear down any running process,
		// wake any pending WaitReady callers with an error, then exit the
		// goroutine permanently. Subsequent public-method calls will fail
		// because parentCtx.Done() unblocks their send-side selects.
		case <-p.parentCtx.Done():
			// Mark shutdown before killProcess so concurrent State() readers
			// stop treating this process as ready while the (possibly slow)
			// teardown is in progress.
			setState(StateShutdown)
			if cmd != nil {
				p.handler.Store(nil)
				p.killProcess(cmd, cmdCancel, cmdDone, parentCancelGraceTimeout)
				cmd = nil
				cmdDone = nil
				cmdCancel = nil
			}
			notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
			respondRun(fmt.Errorf("[%s] shutdown", p.id))
			return

		// Upstream exited on its own (not via Stop). Drop handler state,
		// transition to Stopped, and unblock the parked Run caller.
		// cmdDone is nil while no process is running, so this case is
		// dormant outside of StateReady.
		case <-cmdDone:
			if cmdCancel != nil {
				cmdCancel()
			}
			cmd = nil
			cmdDone = nil
			cmdCancel = nil
			p.handler.Store(nil)
			setState(StateStopped)
			respondRun(fmt.Errorf("[%s] upstream exited unexpectedly", p.id))

		// WaitReady: if we're already in a terminal-for-this-question state,
		// respond immediately; otherwise queue the caller and let a future
		// state transition wake them via notifyWaiters.
		case req := <-p.waitReadyCh:
			switch state {
			case StateReady:
				req.respond <- nil
			case StateShutdown:
				req.respond <- fmt.Errorf("[%s] shutdown", p.id)
			default:
				readyWaiters = append(readyWaiters, req)
			}

		// Run: start the upstream process. Only valid from StateStopped.
		// doStart can take a long time (health-check polling), so it runs in
		// a separate goroutine and we wait on resultCh. While waiting we also
		// listen for an incoming Stop — that's how callers cancel an in-flight
		// start.
		case req := <-p.runCh:
			if state != StateStopped {
				req.respond <- fmt.Errorf("[%s] could not be started in %s state", p.id, state)
				continue
			}
			setState(StateStarting)

			startCtx, cancelStart := context.WithCancel(context.Background())
			resultCh := make(chan startResult, 1)
			go func() {
				resultCh <- p.doStart(startCtx, req.timeout)
			}()

			// pendingStop holds a Stop request that arrived mid-start, so we
			// can respond to it AFTER we've finished tearing the start down.
			var pendingStop *stopReq
			select {
			// doStart finished on its own — either successfully (latch
			// cmd/handler and move to Ready) or with an error (back to
			// Stopped). Either way wake WaitReady subscribers and reply
			// to the Run caller.
			case res := <-resultCh:
				if res.err == nil {
					cmd = res.cmd
					cmdDone = res.cmdDone
					cmdCancel = res.cancel
					fn := res.handlerFn
					p.handler.Store(&fn)
					setState(StateReady)
					notifyWaiters(nil)
					// Park the Run response — Run blocks until the process
					// terminates, so we only fire this when Stop, parentCtx,
					// or the upstream exit takes the process down.
					runResp = req.respond

					// Start TTL goroutine if configured — self-terminates
					// when state leaves StateReady.
					p.startTTLLoop()
				} else {
					setState(StateStopped)
					notifyWaiters(res.err)
					req.respond <- res.err
				}

			// Stop arrived while doStart was still running. Cancel the
			// start context to abort it, then wait for doStart to return.
			// If doStart had already crossed the finish line before
			// cancellation took effect, it returns a live cmd that we
			// must kill ourselves. The Run caller gets ErrAbort; the Stop
			// caller is parked in pendingStop and answered below.
			case stop := <-p.stopCh:
				cancelStart()
				res := <-resultCh
				if res.cmd != nil {
					p.killProcess(res.cmd, res.cancel, res.cmdDone, stop.timeout)
				}
				setState(StateStopped)
				notifyWaiters(ErrStartAborted)
				req.respond <- ErrStartAborted
				pendingStop = &stop

			// Parent context cancelled (e.g. config reload) while doStart
			// was still running. Stop() returns early when parentCtx is
			// done and never sends on stopCh, so we must handle shutdown
			// here to avoid leaving doStart running indefinitely.
			case <-p.parentCtx.Done():
				cancelStart()
				// Mark shutdown before tearing the process down: killProcess
				// may block (e.g. taskkill on Windows is slow to spawn), and
				// callers observing State() should see StateShutdown promptly
				// rather than a stale StateStarting.
				setState(StateShutdown)
				res := <-resultCh
				if res.cmd != nil {
					p.killProcess(res.cmd, res.cancel, res.cmdDone, parentCancelGraceTimeout)
				}
				notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
				respondRun(fmt.Errorf("[%s] shutdown", p.id))
				return
			}
			// cancelStart is idempotent; calling it again here ensures the
			// context is released even on the success path (govet leak check).
			cancelStart()
			if pendingStop != nil {
				pendingStop.respond <- nil
			}

		// Stop: tear down a running process.
		case stop := <-p.stopCh:
			if cmd != nil {
				setState(StateStopping)
				p.killProcess(cmd, cmdCancel, cmdDone, stop.timeout)
				cmd = nil
				cmdDone = nil
				cmdCancel = nil
				p.handler.Store(nil)
			}
			// Stop is a no-op (and not an error) when already Stopped — this
			// is what makes it idempotent for callers that don't track state.
			setState(StateStopped)
			respondRun(nil)
			stop.respond <- nil

		// Sleep: free the upstream's VRAM without killing the subprocess. Only
		// valid from StateReady; from any other state there is nothing to do
		// (already sleeping/stopped, or a transient the caller can treat as
		// "VRAM not occupied"), so we just acknowledge. The cmd/cmdDone/cmdCancel
		// trio is intentionally retained while sleeping so the Stop and cmdDone
		// cases above keep working — that is what lets a sleeping model be
		// stopped (unloaded) or noticed if its subprocess dies.
		case sleep := <-p.sleepCh:
			if state != StateReady {
				sleep.respond <- nil
				continue
			}
			if !p.config.SleepWake.Enabled {
				// No sleep support: fall back to a full stop so the eviction
				// caller still frees VRAM.
				setState(StateStopping)
				p.killProcess(cmd, cmdCancel, cmdDone, sleep.timeout)
				cmd, cmdDone, cmdCancel = nil, nil, nil
				p.handler.Store(nil)
				setState(StateStopped)
				respondRun(nil)
				sleep.respond <- nil
				continue
			}

			// Visible during the (possibly ~10s) sleep POST; the model still
			// holds VRAM here, so the scheduler keeps counting it.
			setState(StateGoingToSleep)
			sleepCtx, cancelSleep := context.WithCancel(context.Background())
			sleepResultCh := make(chan error, 1)
			go func() { sleepResultCh <- p.doSleep(sleepCtx) }()

			select {
			case err := <-sleepResultCh:
				cancelSleep()
				if err != nil {
					// Sleep failed — fall back to a full stop so VRAM is freed.
					p.proxyLogger.Errorf("<%s> sleep failed, stopping to free VRAM: %v", p.id, err)
					setState(StateStopping)
					p.killProcess(cmd, cmdCancel, cmdDone, sleep.timeout)
					cmd, cmdDone, cmdCancel = nil, nil, nil
					p.handler.Store(nil)
					setState(StateStopped)
					respondRun(nil)
				} else {
					setState(StateSleeping)
					p.proxyLogger.Infof("<%s> Model is now sleeping", p.id)
				}
				sleep.respond <- nil

			case stop := <-p.stopCh:
				// Explicit stop arrived mid-sleep: abort the sleep and tear down.
				cancelSleep()
				<-sleepResultCh
				setState(StateStopping)
				p.killProcess(cmd, cmdCancel, cmdDone, stop.timeout)
				cmd, cmdDone, cmdCancel = nil, nil, nil
				p.handler.Store(nil)
				setState(StateStopped)
				respondRun(nil)
				sleep.respond <- nil
				stop.respond <- nil

			case <-p.parentCtx.Done():
				cancelSleep()
				setState(StateShutdown)
				<-sleepResultCh
				if cmd != nil {
					p.handler.Store(nil)
					p.killProcess(cmd, cmdCancel, cmdDone, parentCancelGraceTimeout)
					cmd, cmdDone, cmdCancel = nil, nil, nil
				}
				notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
				respondRun(fmt.Errorf("[%s] shutdown", p.id))
				sleep.respond <- fmt.Errorf("[%s] shutdown", p.id)
				return
			}

		// Wake: restore a sleeping model to ready. Only valid from StateSleeping;
		// anything else is a no-op (already awake, or never slept). The original
		// Run caller stays parked across the whole sleep/wake cycle since the
		// subprocess never terminated.
		case wake := <-p.wakeCh:
			if state != StateSleeping {
				wake.respond <- nil
				continue
			}

			setState(StateWaking)
			wakeCtx, cancelWake := context.WithCancel(context.Background())
			wakeResultCh := make(chan error, 1)
			go func() { wakeResultCh <- p.doWake(wakeCtx, wake.timeout) }()

			select {
			case err := <-wakeResultCh:
				cancelWake()
				if err != nil {
					p.proxyLogger.Errorf("<%s> wake failed, stopping: %v", p.id, err)
					setState(StateStopping)
					p.killProcess(cmd, cmdCancel, cmdDone, wake.timeout)
					cmd, cmdDone, cmdCancel = nil, nil, nil
					p.handler.Store(nil)
					setState(StateStopped)
					respondRun(fmt.Errorf("[%s] wake failed: %w", p.id, err))
					notifyWaiters(err)
				} else {
					setState(StateReady)
					notifyWaiters(nil)
					p.startTTLLoop()
					p.proxyLogger.Infof("<%s> Model is now awake", p.id)
				}
				wake.respond <- err

			case stop := <-p.stopCh:
				cancelWake()
				<-wakeResultCh
				setState(StateStopping)
				p.killProcess(cmd, cmdCancel, cmdDone, stop.timeout)
				cmd, cmdDone, cmdCancel = nil, nil, nil
				p.handler.Store(nil)
				setState(StateStopped)
				respondRun(nil)
				notifyWaiters(ErrStartAborted)
				wake.respond <- ErrStartAborted
				stop.respond <- nil

			case <-p.parentCtx.Done():
				cancelWake()
				setState(StateShutdown)
				<-wakeResultCh
				if cmd != nil {
					p.handler.Store(nil)
					p.killProcess(cmd, cmdCancel, cmdDone, parentCancelGraceTimeout)
					cmd, cmdDone, cmdCancel = nil, nil, nil
				}
				notifyWaiters(fmt.Errorf("[%s] shutdown", p.id))
				respondRun(fmt.Errorf("[%s] shutdown", p.id))
				wake.respond <- fmt.Errorf("[%s] shutdown", p.id)
				return
			}
		}
	}
}

func (p *ProcessCommand) doStart(startCtx context.Context, healthCheckTimeout time.Duration) startResult {
	if p.config.Proxy == "" {
		return startResult{err: fmt.Errorf("upstream proxy missing")}
	}

	args, err := p.config.SanitizedCommand()
	if err != nil {
		return startResult{err: fmt.Errorf("unable to get sanitized command: %w", err)}
	}

	proxyURL, err := url.Parse(p.config.Proxy)
	if err != nil {
		return startResult{err: fmt.Errorf("invalid proxy URL %q: %w", p.config.Proxy, err)}
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)
	reverseProxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(p.config.Timeouts.Connect) * time.Second,
			KeepAlive: time.Duration(p.config.Timeouts.KeepAlive) * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   time.Duration(p.config.Timeouts.TLSHandshake) * time.Second,
		ResponseHeaderTimeout: time.Duration(p.config.Timeouts.ResponseHeader) * time.Second,
		ExpectContinueTimeout: time.Duration(p.config.Timeouts.ExpectContinue) * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       time.Duration(p.config.Timeouts.IdleConn) * time.Second,
	}
	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header.Set("X-Accel-Buffering", "no")
		}
		return nil
	}
	// httputil.ReverseProxy panics with http.ErrAbortHandler when the upstream
	// disconnects after response headers have been sent. Recover here so the
	// streaming termination is treated as a normal client/upstream disconnect.
	// see: https://github.com/golang/go/issues/23643
	handlerFn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					p.proxyLogger.Infof("<%s> recovered from upstream disconnection during streaming", p.id)
				} else {
					p.proxyLogger.Warnf("<%s> recovered from panic: %v", p.id, rec)
				}
			}
		}()
		reverseProxy.ServeHTTP(w, r)
	})

	// cmdCtx + cmd.Cancel are wired as a safety net: if the context is ever
	// cancelled while the process is alive, cmd.Cancel sends SIGTERM / CmdStop
	// and the runtime escalates to SIGKILL after cmd.WaitDelay. In the normal
	// teardown path killProcess sends the stop signal directly instead, so
	// cmd.WaitDelay only acts as the inherited-pipe backstop measured from
	// process exit (see killProcess).
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	cmd.Stderr = p.processLogger
	cmd.Stdout = p.processLogger
	cmd.Env = append(cmd.Environ(), p.config.Env...)
	cmd.Cancel = func() error { return p.sendStopSignal(cmd) }
	cmd.WaitDelay = p.waitDelay
	setProcAttributes(cmd)

	p.proxyLogger.Debugf("<%s> Executing start command: %s, env: %s", p.id, strings.Join(args, " "), strings.Join(p.config.Env, ", "))

	cmdDone := make(chan struct{})
	if err := cmd.Start(); err != nil {
		cmdCancel()
		return startResult{err: fmt.Errorf("failed to start command '%s': %w", strings.Join(args, " "), err)}
	}

	go func() {
		waitErr := cmd.Wait()
		switch st := p.State(); {
		case waitErr == nil:
			p.proxyLogger.Debugf("<%s> process exited cleanly", p.id)
		case st == StateStopping || st == StateShutdown:
			// Expected: we force-terminated the process. A forced kill exits
			// the child with a non-zero code (e.g. taskkill /f on Windows
			// yields exit status 1), so this is not an error.
			p.proxyLogger.Debugf("<%s> process stopped by llama-swap: %v", p.id, waitErr)
		default:
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				p.proxyLogger.Debugf("<%s> process exited: code=%d, err=%v", p.id, exitErr.ExitCode(), waitErr)
			} else {
				p.proxyLogger.Debugf("<%s> process exited with error: %v", p.id, waitErr)
			}
		}
		close(cmdDone)
	}()

	abort := func(err error) startResult {
		p.killProcess(cmd, cmdCancel, cmdDone, 5*time.Second)
		return startResult{err: err}
	}
	prematureExit := func() startResult {
		cmdCancel()
		return startResult{err: fmt.Errorf("upstream command exited prematurely")}
	}

	if startCtx.Err() != nil {
		return abort(ErrStartAborted)
	}

	checkEndpoint := strings.TrimSpace(p.config.CheckEndpoint)
	if checkEndpoint == "none" {
		return startResult{cmd: cmd, cmdDone: cmdDone, cancel: cmdCancel, handlerFn: handlerFn}
	}

	// Wait 250ms for the command to start up before health checking
	select {
	case <-startCtx.Done():
		return abort(ErrStartAborted)
	case <-time.After(250 * time.Millisecond):
	}

	deadline := time.Now().Add(healthCheckTimeout)
	for {
		select {
		case <-startCtx.Done():
			return abort(ErrStartAborted)
		case <-cmdDone:
			return prematureExit()
		default:
		}

		if time.Now().After(deadline) {
			return abort(fmt.Errorf("health check timed out after %v", healthCheckTimeout))
		}

		req, _ := http.NewRequestWithContext(startCtx, "GET", p.config.CheckEndpoint, nil)
		rr := httptest.NewRecorder()
		reverseProxy.ServeHTTP(rr, req)
		resp := rr.Result()
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			p.proxyLogger.Infof("<%s> Health check passed on %s%s", p.id, p.config.Proxy, p.config.CheckEndpoint)
			break
		} else if startCtx.Err() != nil {
			return abort(ErrStartAborted)
		}

		select {
		case <-startCtx.Done():
			return abort(ErrStartAborted)
		case <-cmdDone:
			return prematureExit()
		case <-time.After(time.Second):
		}
	}

	return startResult{cmd: cmd, cmdDone: cmdDone, cancel: cmdCancel, handlerFn: handlerFn}
}

// sendStopSignal runs the configured CmdStop (if any) or sends SIGTERM to
// the upstream process. Wired up as cmd.Cancel so it fires whenever the
// cmd's context is cancelled.
func (p *ProcessCommand) sendStopSignal(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		p.processLogger.Debugf("<%s> sendStopSignal() called with nil cmd or process, nothing to stop", p.id)
		return nil
	}
	pid := cmd.Process.Pid
	if p.config.CmdStop != "" {
		p.processLogger.Debugf("<%s> sendStopSignal() using CmdStop %q for pid %d", p.id, p.config.CmdStop, pid)
		stopArgs, err := config.SanitizeCommand(
			strings.ReplaceAll(p.config.CmdStop, "${PID}", fmt.Sprintf("%d", pid)),
		)
		if err == nil {
			p.processLogger.Debugf("<%s> sendStopSignal() running stop command: %s", p.id, strings.Join(stopArgs, " "))
			stopCmd := exec.Command(stopArgs[0], stopArgs[1:]...)
			stopCmd.Env = cmd.Env
			setProcAttributes(stopCmd)
			runErr := stopCmd.Run()
			if runErr != nil {
				p.processLogger.Errorf("<%s> sendStopSignal() stop command failed: %v", p.id, runErr)
			} else {
				p.processLogger.Debugf("<%s> sendStopSignal() stop command completed for pid %d", p.id, pid)
			}
			return runErr
		}
		// fall through to SIGTERM if sanitize failed
		p.processLogger.Errorf("<%s> sendStopSignal() failed to sanitize CmdStop %q: %v, falling back to terminateProcessTree", p.id, p.config.CmdStop, err)
	}
	// On Unix this SIGTERMs the whole process group so a forked grandchild
	// (e.g. a shell wrapper that backgrounds the real binary) is taken down
	// with the parent rather than orphaned.
	p.processLogger.Debugf("<%s> sendStopSignal() no CmdStop configured, calling terminateProcessTree for pid %d", p.id, pid)
	termErr := terminateProcessTree(cmd)
	if termErr != nil {
		p.processLogger.Errorf("<%s> sendStopSignal() terminateProcessTree failed for pid %d: %v", p.id, pid, termErr)
	}
	return termErr
}

// killProcess terminates the upstream process. The flow:
//
//  1. Send the graceful stop signal (CmdStop / SIGTERM) directly — NOT by
//     cancelling cmdCtx. Cancelling the context would start cmd.WaitDelay
//     immediately, which force-kills the process WaitDelay after the signal
//     and would silently cap gracefulTimeout at WaitDelay whenever
//     gracefulTimeout is the longer of the two.
//  2. We wait up to gracefulTimeout for the process to exit on its own.
//  3. If still alive, we SIGKILL the process group directly (Unix) so any
//     forked descendant is force-terminated alongside the parent.
//  4. We wait on cmdDone. cmd.WaitDelay (set when the cmd was built) is the
//     critical backstop here: once the process exits, if a forked grandchild
//     inherited the stdout/stderr pipes and is still holding them, the runtime
//     force-closes the pipes WaitDelay after the exit and cmd.Wait() unblocks.
//     Because we never cancelled the context, that WaitDelay timer measures
//     from process exit (see os/exec awaitGoroutines), not from this call.
//     Without WaitDelay this select would hang forever (the v219 bug).
//
// cancel() is still invoked (deferred) to release the context, but only after
// the process has exited and os/exec's ctx watcher has already torn down, so it
// never re-fires cmd.Cancel.
func (p *ProcessCommand) killProcess(cmd *exec.Cmd, cancel context.CancelFunc, cmdDone <-chan struct{}, gracefulTimeout time.Duration) {
	if cancel == nil {
		return
	}
	defer cancel()

	// Deliver CmdStop / SIGTERM in a goroutine so a slow or hanging CmdStop
	// cannot block the run() goroutine; the gracefulTimeout + Process.Kill
	// path below still guarantees teardown.
	if cmd != nil {
		go func() {
			p.proxyLogger.Debugf("[%s] sending stop signal with timeout %v", p.id, gracefulTimeout)
			if err := p.sendStopSignal(cmd); err != nil {
				p.proxyLogger.Warnf("[%s] stop signal failed: %v", p.id, err)
			}
		}()
	}

	timer := time.NewTimer(gracefulTimeout)
	defer timer.Stop()

	select {
	case <-cmdDone:
		return
	case <-timer.C:
	}

	if cmd != nil {
		// SIGKILL the whole process group on Unix so any descendant that
		// ignored or outlived the graceful signal is force-terminated too.
		_ = killProcessTree(cmd)
	}
	<-cmdDone
}

func (p *ProcessCommand) ID() string {
	return p.id
}

func (p *ProcessCommand) Run(timeout time.Duration) error {
	req := runReq{
		timeout: timeout,
		respond: make(chan error, 1),
	}
	select {
	case p.runCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
}

func (p *ProcessCommand) WaitReady(ctx context.Context) error {
	req := waitReadyReq{respond: make(chan error, 1)}
	select {
	case p.waitReadyCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ProcessCommand) Stop(timeout time.Duration) error {
	req := stopReq{
		timeout: timeout,
		respond: make(chan error, 1),
	}
	select {
	case p.stopCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	return <-req.respond
}

// Sleep frees the upstream's VRAM, keeping the subprocess alive for a fast
// Wake. It funnels through run() (the single state owner) and blocks until the
// model is sleeping or, on any failure / unsupported config, until it has been
// fully stopped — either way VRAM is freed. Concurrent Sleep callers serialize
// in run(); the second observes a non-Ready state and returns immediately.
func (p *ProcessCommand) Sleep(timeout time.Duration) error {
	req := sleepReq{timeout: timeout, respond: make(chan error, 1)}
	select {
	case p.sleepCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	return <-req.respond
}

// Wake restores a sleeping model to ready. It blocks until the model is ready
// or returns an error (after stopping the process) if waking fails. Calling it
// on a process that is not sleeping is a no-op.
func (p *ProcessCommand) Wake(timeout time.Duration) error {
	req := wakeReq{timeout: timeout, respond: make(chan error, 1)}
	select {
	case p.wakeCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	return <-req.respond
}

// startTTLLoop launches the idle-unload goroutine when UnloadAfter is
// configured. It self-terminates when the process leaves StateReady. On TTL
// expiry it sleeps the model when sleep/wake is enabled (freeing VRAM while
// keeping the process warm) and otherwise stops it. Called on the initial
// ready transition and again after each wake.
func (p *ProcessCommand) startTTLLoop() {
	if p.config.UnloadAfter <= 0 {
		return
	}
	ttlDuration := time.Duration(p.config.UnloadAfter) * time.Second
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if p.State() != StateReady {
				return
			}
			if p.inflight.Load() != 0 {
				continue
			}
			if time.Since(time.Unix(0, p.lastUse.Load())) > ttlDuration {
				if p.config.SleepWake.Enabled {
					p.proxyLogger.Infof("<%s> Sleeping model, TTL of %ds reached", p.id, p.config.UnloadAfter)
					p.Sleep(10 * time.Second)
				} else {
					p.proxyLogger.Infof("<%s> Unloading model, TTL of %ds reached", p.id, p.config.UnloadAfter)
					p.Stop(10 * time.Second)
				}
				return
			}
		}
	}()
}

// doSleep waits for in-flight requests to drain, POSTs to the configured sleep
// endpoint (vLLM blocks this until VRAM is actually freed), then verifies the
// model reports itself sleeping. Runs in its own goroutine launched by run();
// returns an error so the caller can fall back to a full stop.
func (p *ProcessCommand) doSleep(ctx context.Context) error {
	// Wait for in-flight requests to finish before freeing VRAM.
	for p.inflight.Load() != 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sleep aborted")
		case <-time.After(50 * time.Millisecond):
		}
	}

	sleepURL, err := url.JoinPath(p.config.Proxy, p.config.SleepWake.SleepEndpoint)
	if err != nil {
		return fmt.Errorf("failed to build sleep URL: %w", err)
	}

	p.proxyLogger.Infof("<%s> Sending sleep request to %s", p.id, sleepURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sleepURL, nil)
	if err != nil {
		return fmt.Errorf("failed to build sleep request: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sleep request failed: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sleep endpoint returned status %d", resp.StatusCode)
	}

	// Verify the model is actually sleeping as a safety net.
	verifyTimeout := time.Second * time.Duration(p.config.SleepWake.SleepVerifyTimeout)
	if !p.verifySleeping(ctx, verifyTimeout, 2*time.Second) {
		return fmt.Errorf("sleep verification failed")
	}
	return nil
}

// doWake POSTs to the configured wake endpoint and health-checks the upstream
// until it is ready (bounded by WakeVerifyTimeout). Runs in its own goroutine
// launched by run().
func (p *ProcessCommand) doWake(ctx context.Context, _ time.Duration) error {
	wakeURL, err := url.JoinPath(p.config.Proxy, p.config.SleepWake.WakeEndpoint)
	if err != nil {
		return fmt.Errorf("failed to build wake URL: %w", err)
	}

	p.proxyLogger.Infof("<%s> Sending wake request to %s", p.id, wakeURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wakeURL, nil)
	if err != nil {
		return fmt.Errorf("failed to build wake request: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("wake request failed: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wake endpoint returned status %d", resp.StatusCode)
	}

	checkEndpoint := strings.TrimSpace(p.config.CheckEndpoint)
	if checkEndpoint == "none" {
		return nil
	}

	healthURL, err := url.JoinPath(p.config.Proxy, checkEndpoint)
	if err != nil {
		return fmt.Errorf("failed to build health check URL: %w", err)
	}

	deadline := time.Now().Add(time.Second * time.Duration(p.config.SleepWake.WakeVerifyTimeout))
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wake aborted")
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wake health check timed out")
		}
		hreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		hresp, herr := client.Do(hreq)
		if herr == nil {
			status := hresp.StatusCode
			hresp.Body.Close()
			if status == http.StatusOK {
				p.proxyLogger.Infof("<%s> Wake health check passed on %s", p.id, healthURL)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wake aborted")
		case <-time.After(time.Second):
		}
	}
}

// verifySleeping polls the is_sleeping endpoint until it confirms the model is
// sleeping, the timeout elapses, or ctx is cancelled.
func (p *ProcessCommand) verifySleeping(ctx context.Context, timeout, interval time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		sleeping, err := p.isSleeping(ctx)
		if err != nil {
			p.proxyLogger.Debugf("<%s> is_sleeping check error (will retry): %v", p.id, err)
		} else if sleeping {
			p.proxyLogger.Infof("<%s> Sleep verified via is_sleeping endpoint", p.id)
			return true
		}
		if time.Now().After(deadline) {
			p.proxyLogger.Errorf("<%s> Sleep verification timed out after %v", p.id, timeout)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}

// isSleeping queries the upstream is_sleeping endpoint.
func (p *ProcessCommand) isSleeping(ctx context.Context) (bool, error) {
	isSleepingURL, err := url.JoinPath(p.config.Proxy, p.config.SleepWake.IsSleepingEndpoint)
	if err != nil {
		return false, fmt.Errorf("failed to build is_sleeping URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, isSleepingURL, nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("is_sleeping endpoint returned status %d", resp.StatusCode)
	}
	var result struct {
		IsSleeping bool `json:"is_sleeping"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("failed to decode is_sleeping response: %w", err)
	}
	return result.IsSleeping, nil
}

func (p *ProcessCommand) State() ProcessState {
	if s, ok := p.state.Load().(ProcessState); ok {
		return s
	}
	return StateStopped
}

func (p *ProcessCommand) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fn := p.handler.Load()
	if fn == nil {
		http.Error(w, fmt.Sprintf("llama-swap-error: [%s] process is not ready", p.id), http.StatusServiceUnavailable)
		return
	}
	p.inflight.Add(1)
	defer func() {
		p.lastUse.Store(time.Now().UnixNano())
		p.inflight.Add(-1)
	}()
	(*fn)(w, r)
}
