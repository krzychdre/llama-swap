package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestServer_APILockModel(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"m1"}, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	post := func(path string) int {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		return w.Code
	}
	statusLocked := func() bool {
		return s.modelStatus()[0].Locked
	}

	if code := post("/api/models/lock/m1"); code != http.StatusOK {
		t.Fatalf("lock status=%d want 200", code)
	}
	if !s.locks.Locked("m1") || !statusLocked() {
		t.Errorf("m1 not locked after lock: Locked=%v status=%v", s.locks.Locked("m1"), statusLocked())
	}

	if code := post("/api/models/unlock/m1"); code != http.StatusOK {
		t.Fatalf("unlock status=%d want 200", code)
	}
	if s.locks.Locked("m1") || statusLocked() {
		t.Errorf("m1 still locked after unlock: Locked=%v status=%v", s.locks.Locked("m1"), statusLocked())
	}

	if code := post("/api/models/lock/missing"); code != http.StatusNotFound {
		t.Errorf("lock of unknown model status=%d want 404", code)
	}
}
