package process

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func sleepWakeConfig(t *testing.T, enabled bool) config.ModelConfig {
	t.Helper()
	cmd, port := simpleResponderCmd(t, "-silent", "-respond hello")
	return config.ModelConfig{
		Cmd:                cmd,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		SleepWake: config.SleepWakeConfig{
			Enabled:            enabled,
			SleepEndpoint:      "/sleep",
			WakeEndpoint:       "/wake_up",
			IsSleepingEndpoint: "/is_sleeping",
			SleepVerifyTimeout: 5,
			WakeVerifyTimeout:  5,
		},
	}
}

func waitForState(t *testing.T, p *ProcessCommand, want ProcessState) {
	t.Helper()
	deadline := time.Now().Add(testStartTimeout)
	for time.Now().Before(deadline) {
		if p.State() == want {
			return
		}
		time.Sleep(testPollInterval)
	}
	t.Fatalf("timed out waiting for state %s, got %s", want, p.State())
}

// TestProcessCommand_SleepWake exercises the full sleep → wake → serve cycle on
// a sleep/wake-enabled model. The subprocess stays alive across the cycle.
func TestProcessCommand_SleepWake(t *testing.T) {
	skipIfNoSimpleResponder(t)

	p := newProcessCommand(t, sleepWakeConfig(t, true))
	t.Cleanup(func() { p.Stop(testStopTimeout) })

	runErr := runAsync(t, p)
	if got := p.State(); got != StateReady {
		t.Fatalf("after Run: expected %s, got %s", StateReady, got)
	}

	// Sleep frees VRAM but keeps the process alive.
	if err := p.Sleep(testStopTimeout); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if got := p.State(); got != StateSleeping {
		t.Fatalf("after Sleep: expected %s, got %s", StateSleeping, got)
	}

	// Run is still parked (process never terminated).
	select {
	case err := <-runErr:
		t.Fatalf("Run returned while sleeping: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Wake restores readiness.
	if err := p.Wake(testStartTimeout); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if got := p.State(); got != StateReady {
		t.Fatalf("after Wake: expected %s, got %s", StateReady, got)
	}

	// The process serves traffic again.
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/test", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("after Wake: expected 200, got %d", rr.Code)
	}
}

// TestProcessCommand_SleepStop verifies a sleeping process can be fully stopped
// (the "unload a sleeping model" path), terminating the subprocess.
func TestProcessCommand_SleepStop(t *testing.T) {
	skipIfNoSimpleResponder(t)

	p := newProcessCommand(t, sleepWakeConfig(t, true))
	runErr := runAsync(t, p)

	if err := p.Sleep(testStopTimeout); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	waitForState(t, p, StateSleeping)

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := p.State(); got != StateStopped {
		t.Fatalf("after Stop: expected %s, got %s", StateStopped, got)
	}

	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Fatal("Run did not return after stopping a sleeping process")
	}
}

// TestProcessCommand_SleepFallsBackToStop verifies that calling Sleep on a model
// without sleep/wake support frees VRAM by fully stopping it instead.
func TestProcessCommand_SleepFallsBackToStop(t *testing.T) {
	skipIfNoSimpleResponder(t)

	p := newProcessCommand(t, sleepWakeConfig(t, false))
	runErr := runAsync(t, p)

	if err := p.Sleep(testStopTimeout); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if got := p.State(); got != StateStopped {
		t.Fatalf("Sleep without support: expected %s, got %s", StateStopped, got)
	}

	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Fatal("Run did not return after sleep fell back to stop")
	}
}
