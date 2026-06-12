package process

import (
	"context"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

type ProcessState string

const (
	StateStopped  ProcessState = ProcessState("stopped")
	StateStarting ProcessState = ProcessState("starting")
	StateReady    ProcessState = ProcessState("ready")
	StateStopping ProcessState = ProcessState("stopping")

	// Sleep/wake states for models that support freeing VRAM without exiting
	// (e.g. vLLM). The subprocess stays alive across all three; only its VRAM
	// is released. StateGoingToSleep is observable while the (potentially slow)
	// sleep POST is in flight — the model still occupies VRAM during it, so the
	// scheduler must keep counting it until it reaches StateSleeping.
	StateGoingToSleep ProcessState = ProcessState("going-to-sleep")
	StateSleeping     ProcessState = ProcessState("sleeping")
	StateWaking       ProcessState = ProcessState("waking")

	// process is shutdown and will not be restarted
	StateShutdown ProcessState = ProcessState("shutdown")
)

type Process interface {
	// Run starts the process blocks until the process is terminated.
	// The timeout parameter controls how long to wait for the process to get
	// to a ready state to process traffic
	Run(timeout time.Duration) error

	// WaitReady blocks until the process is ready to serve requests
	// or the context is cancelled. It returns nil when the process is ready
	WaitReady(context.Context) error

	// Stop blocks until the process has terminated. It returns nil when
	// the process terminated as expected (exit 0)
	Stop(timeout time.Duration) error

	// Sleep frees the upstream's VRAM while keeping the subprocess alive, so a
	// later Wake is fast. It blocks until the model is sleeping (or, if sleep
	// is unsupported or fails, until the process has been stopped so its VRAM
	// is freed either way). Used by the router to evict a model when it
	// supports sleep/wake instead of fully stopping it.
	Sleep(timeout time.Duration) error

	// Wake restores a sleeping model to a ready state. It blocks until the
	// model is ready, or returns an error (and stops the process) if waking
	// fails. Calling Wake on a process that is not sleeping is a no-op.
	Wake(timeout time.Duration) error

	// State returns the current state of the process
	// Note: this is a snapshot of the state at the time of the call
	// and may change at any time after the call returns.
	State() ProcessState

	// ServeHTTP forwards requests to the underlying process
	// Calling it when the process is not ready will result in a
	// 503 response with a body indicating it is a llama-swap-error
	ServeHTTP(http.ResponseWriter, *http.Request)

	// Logger returns the monitor that captures this process's stdout/stderr.
	Logger() *logmon.Monitor
}
