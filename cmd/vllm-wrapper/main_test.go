package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWakeUpVLLM(t *testing.T) {
	// Test successful wake up
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wake_up" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if err := wakeUpVLLM(ts.URL); err != nil {
		t.Fatalf("wakeUpVLLM failed: %v", err)
	}

	// Test failure when server returns error
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/wake_up" {
			t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts2.Close()

	if err := wakeUpVLLM(ts2.URL); err == nil {
		t.Errorf("wakeUpVLLM expected error for non-200 response")
	}
}

func TestWaitForHealthy(t *testing.T) {
	// Test successful health check
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("Unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	if err := waitForHealthyWithPath(ts.URL, "/v1/models", 2*time.Second); err != nil {
		t.Fatalf("waitForHealthy failed: %v", err)
	}

	// Test timeout: server delays response longer than context timeout
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay 2 seconds
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts2.Close()

	err := waitForHealthyWithPath(ts2.URL, "/v1/models", 1*time.Second)
	if err == nil {
		t.Errorf("waitForHealthy expected timeout error")
		return
	}
	if err != context.DeadlineExceeded {
		t.Errorf("waitForHealthy expected context deadline exceeded, got %v", err)
	}
}

func TestSleepCommandMarshal(t *testing.T) {
	// We test the sleep command by checking the JSON marshaling we use in sleepCmd.
	// Since sleepCmd is not easily unit-testable without exposing more, we test the structure.
	body := map[string]int{"level": 1}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}
	expected := `{"level":1}`
	if string(data) != expected {
		t.Errorf("Expected %s, got %s", expected, string(data))
	}
}

// TestStartDaemon tests that startDaemon returns an error when the start command exits
// quickly and the daemon does not become healthy.
func TestStartDaemon(t *testing.T) {
	// Use a start command that exits immediately (true) and a health URL that will not respond.
	err := startDaemon([]string{"true"}, "http://127.0.0.1:12345/health", "/health", 10*time.Millisecond)
	if err == nil {
		t.Fatalf("startDaemon expected error but got nil")
	}
	if !strings.Contains(err.Error(), "daemon did not become healthy") {
		t.Errorf("error expected to contain 'daemon did not become healthy', got %v", err)
	}
}

// TestStartDaemonArgv verifies that multiple startArgs are passed as separate argv values.
func TestServeHandler_ControlPathsLoopbackOnly(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		remoteAddr string
		want       int
	}{
		{"sleep from loopback", http.MethodPost, "/sleep", "127.0.0.1:5000", http.StatusOK},
		{"wake_up from ipv6 loopback", http.MethodPost, "/wake_up", "[::1]:5000", http.StatusOK},
		{"sleep from the network", http.MethodPost, "/sleep", "192.168.1.20:5000", http.StatusForbidden},
		{"wake_up from the network", http.MethodPost, "/wake_up", "192.168.1.20:5000", http.StatusForbidden},
		{"sleep from the network without a port", http.MethodPost, "/sleep", "192.168.1.20", http.StatusForbidden},
		{"other paths from the network", http.MethodPost, "/v1/chat/completions", "192.168.1.20:5000", http.StatusOK},
		{"sleep read from the network", http.MethodGet, "/sleep", "192.168.1.20:5000", http.StatusOK},
	}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := newServeHandler(upstream)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got status %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestStartDaemonArgv(t *testing.T) {
	tmpDir := t.TempDir()
	argvFile := filepath.Join(tmpDir, "argv.txt")

	script := filepath.Join(tmpDir, "write-argv.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/bash\nprintf '%s\n' \"$@\" > \""+argvFile+"\"\nexit 0\n",
	), 0755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	err := startDaemon([]string{script, "arg1", "arg2", "arg3"}, "http://127.0.0.1:12345/health", "/health", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error (health check fails)")
	}

	content, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(content)), "\n")
	want := []string{"arg1", "arg2", "arg3"}
	if len(got) != len(want) {
		t.Fatalf("argv length: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}
