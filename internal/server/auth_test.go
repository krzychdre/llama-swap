package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestServer_SameOriginMiddleware(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// proxied cases model the documented nginx setup: it keeps Host as its own
	// address and reports the public host only in X-Forwarded-Host.
	cases := []struct {
		name       string
		method     string
		host       string
		origin     string
		remoteAddr string
		forwarded  string
		want       int
	}{
		{"post without Origin", http.MethodPost, "example.com", "", "203.0.113.9:4000", "", http.StatusOK},
		{"post from the same host", http.MethodPost, "example.com", "http://example.com", "203.0.113.9:4000", "", http.StatusOK},
		{"post from another host", http.MethodPost, "example.com", "http://evil.example", "203.0.113.9:4000", "", http.StatusForbidden},
		{"put from another host", http.MethodPut, "example.com", "https://evil.example", "203.0.113.9:4000", "", http.StatusForbidden},
		{"delete from another host", http.MethodDelete, "example.com", "http://evil.example", "203.0.113.9:4000", "", http.StatusForbidden},
		{"get from another host is not state changing", http.MethodGet, "example.com", "http://evil.example", "203.0.113.9:4000", "", http.StatusOK},
		{
			"post from the public host through a loopback proxy",
			http.MethodPost, "127.0.0.1:8080", "https://real-host", "127.0.0.1:4000", "real-host", http.StatusOK,
		},
		{
			"post from the public host through a remote proxy",
			http.MethodPost, "127.0.0.1:8080", "https://real-host", "203.0.113.9:4000", "real-host", http.StatusForbidden,
		},
		{
			"post from a foreign origin next to a forwarded host",
			http.MethodPost, "127.0.0.1:8080", "https://evil.example", "127.0.0.1:4000", "real-host", http.StatusForbidden,
		},
		{
			"post from a loopback proxy that forwards no host",
			http.MethodPost, "127.0.0.1:8080", "https://real-host", "127.0.0.1:4000", "", http.StatusForbidden,
		},
		{
			"post with a malformed Origin",
			http.MethodPost, "example.com", "http://example.com:bad port", "127.0.0.1:4000", "", http.StatusForbidden,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "/api/models/unload", nil)
			r.Host = c.host
			r.RemoteAddr = c.remoteAddr
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.forwarded != "" {
				r.Header.Set("X-Forwarded-Host", c.forwarded)
			}
			w := httptest.NewRecorder()
			sameOriginMiddleware(final).ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("status = %d, want %d", w.Code, c.want)
			}
		})
	}
}

func TestServer_RequestContextMiddleware(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"llama3": {},
		},
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := CreateRequestContextMiddleware(cfg)

	t.Run("known model passes through", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama3"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("missing model returns 404", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

func TestServer_AuthMiddleware(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("no keys configured passes through", func(t *testing.T) {
		mw := CreateAuthMiddleware(config.Config{})
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	cfg := config.Config{RequiredAPIKeys: []string{"secret"}}

	t.Run("valid key", func(t *testing.T) {
		mw := CreateAuthMiddleware(cfg)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		mw := CreateAuthMiddleware(cfg)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Error("missing WWW-Authenticate header")
		}
	})
}
