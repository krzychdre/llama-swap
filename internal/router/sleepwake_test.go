package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// TestGroup_SwapSleepsSleepWakeSibling verifies that evicting a sleep/wake
// enabled model during a swap puts it to sleep (freeing VRAM while keeping the
// subprocess alive) instead of fully stopping it.
func TestGroup_SwapSleepsSleepWakeSibling(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()

	b := newFakeProcess("b")
	b.autoReady = true

	conf := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {SleepWake: config.SleepWakeConfig{Enabled: true}},
			"b": {},
		},
		Routing: groupRouting(map[string]config.GroupConfig{
			"g": {Swap: true, Exclusive: true, Members: []string{"a", "b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.sleepCalls.Load(); got != 1 {
		t.Errorf("a.sleepCalls=%d want 1 (sleep/wake model should be slept on eviction)", got)
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (should sleep, not stop)", got)
	}
	if got := a.State(); got != process.StateSleeping {
		t.Errorf("a.State=%s want %s", got, process.StateSleeping)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

// TestGroup_UnloadStopsSleepWakeModel verifies that an explicit unload fully
// stops a sleep/wake model rather than sleeping it — unload means release the
// process, not just its VRAM.
func TestGroup_UnloadStopsSleepWakeModel(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0) // park a Run goroutine so Stop has something to release

	conf := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {SleepWake: config.SleepWakeConfig{Enabled: true}},
		},
		Routing: groupRouting(map[string]config.GroupConfig{
			"g": {Swap: true, Exclusive: true, Members: []string{"a"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a})

	g.Unload(2 * time.Second)

	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1 (unload must stop, not sleep)", got)
	}
	if got := a.sleepCalls.Load(); got != 0 {
		t.Errorf("a.sleepCalls=%d want 0 (unload must not sleep)", got)
	}
}
