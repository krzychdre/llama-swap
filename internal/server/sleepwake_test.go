package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

func TestServer_APISleepModel(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m1": {SleepWake: config.SleepWakeConfig{Enabled: true}},
	}}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/sleep/m1", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := local.sleepCalls.Load(); got != 1 {
		t.Errorf("sleepCalls=%d want 1", got)
	}
	if local.sleepModel != "m1" {
		t.Errorf("sleepModel=%q want m1", local.sleepModel)
	}
}

func TestServer_APISleepModel_NotSupported(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/sleep/m1", nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%q", w.Code, w.Body.String())
	}
	if got := local.sleepCalls.Load(); got != 0 {
		t.Errorf("sleepCalls=%d want 0", got)
	}
}

func TestServer_APIWakeModel_Sleeping(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	local.running = map[string]process.ProcessState{"m1": process.StateSleeping}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m1": {SleepWake: config.SleepWakeConfig{Enabled: true}},
	}}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/wake/m1", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	// wakeModel dispatches a background load request through the local router.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if local.serveCalls.Load() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("wake did not dispatch a load request, serveCalls=%d want 1", local.serveCalls.Load())
}

func TestServer_APIWakeModel_NotSleeping(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	local.running = map[string]process.ProcessState{"m1": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m1": {SleepWake: config.SleepWakeConfig{Enabled: true}},
	}}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/wake/m1", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	// A non-sleeping model is a no-op: no load request is dispatched.
	time.Sleep(100 * time.Millisecond)
	if got := local.serveCalls.Load(); got != 0 {
		t.Errorf("serveCalls=%d want 0 (wake on non-sleeping model must be a no-op)", got)
	}
}
