package server

import (
	"net/http"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// ModelLocks records which local models are locked: a request for another
// model that would evict a locked one is rejected instead of swapping it out.
// Like PauseState it is owned by main and shared by every Server a hot config
// reload creates, and held in memory only.
type ModelLocks struct {
	mu     sync.Mutex
	locked map[string]bool
}

func NewModelLocks() *ModelLocks {
	return &ModelLocks{locked: make(map[string]bool)}
}

// Locked reports whether modelID is locked. It is installed on the local
// router as its lock check.
func (l *ModelLocks) Locked(modelID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locked[modelID]
}

// set updates the lock of modelID and reports whether it changed.
func (l *ModelLocks) set(modelID string, locked bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locked[modelID] == locked {
		return false
	}
	if locked {
		l.locked[modelID] = true
	} else {
		delete(l.locked, modelID)
	}
	return true
}

func (s *Server) handleAPILockModel(w http.ResponseWriter, r *http.Request) {
	s.setModelLocked(w, r, true)
}

func (s *Server) handleAPIUnlockModel(w http.ResponseWriter, r *http.Request) {
	s.setModelLocked(w, r, false)
}

func (s *Server) setModelLocked(w http.ResponseWriter, r *http.Request, locked bool) {
	realName, ok := s.resolveLocalModel(w, r)
	if !ok {
		return
	}
	if s.locks.set(realName, locked) {
		if locked {
			s.logs.ProxyLogs.Infof("model %s locked", realName)
		} else {
			s.logs.ProxyLogs.Infof("model %s unlocked", realName)
		}
		event.Emit(swaputil.ModelLockChangedEvent{ModelID: realName, Locked: locked})
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
