package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/metricsdb"
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/tidwall/gjson"
)

// getMetrics returns all stored entries oldest-first, mirroring the pre-DB
// ring-buffer accessor the tests were written against.
func (mp *metricsMonitor) getMetrics() []ActivityLogEntry {
	page, err := mp.list(metricsdb.ListFilter{Limit: 500})
	if err != nil {
		panic(err)
	}
	slices.Reverse(page.Entries)
	return page.Entries
}

func TestServer_ParseMetrics_ChatCompletions(t *testing.T) {
	body := `{"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 12 || entry.Tokens.OutputTokens != 7 || entry.Tokens.CachedTokens != 4 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ParseMetrics_Timings(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":100.0,"predicted_per_second":40.0,"prompt_ms":200,"predicted_ms":1250,"cache_n":8}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 20 || entry.Tokens.OutputTokens != 50 || entry.Tokens.CachedTokens != 8 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.TokensPerSecond != 40.0 || entry.Tokens.PromptPerSecond != 100.0 {
		t.Fatalf("rates = %+v", entry.Tokens)
	}
	if entry.DurationMs != 1450 {
		t.Fatalf("DurationMs = %d, want 1450", entry.DurationMs)
	}
}

func TestServer_ProcessStreamingResponse(t *testing.T) {
	body := []byte("data: {\"choices\":[{}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":33}}\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), time.Time{}, time.Time{}, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 15 || entry.Tokens.OutputTokens != 33 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_NoData(t *testing.T) {
	if _, err := processStreamingResponse("m", time.Now(), time.Time{}, time.Time{}, []byte("data: [DONE]\n\n")); err == nil {
		t.Fatal("expected error for stream with no usage data")
	}
}

// Streaming backends without llama.cpp timings (e.g. vllm) get generation rates
// derived from the proxy's own stream write timing.
func TestServer_ProcessStreamingResponse_DerivedRates(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":11}}\n\n" +
		"data: [DONE]\n\n")

	start := time.Now()
	firstToken := start.Add(500 * time.Millisecond) // TTFT: 100 prompt tokens / 0.5s = 200 t/s
	lastToken := firstToken.Add(1 * time.Second)    // gen: (11-1) tokens / 1s = 10 t/s

	entry, err := processStreamingResponse("m", start, firstToken, lastToken, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 100 || entry.Tokens.OutputTokens != 11 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.TokensPerSecond != 10.0 {
		t.Fatalf("TokensPerSecond = %v, want 10", entry.Tokens.TokensPerSecond)
	}
	if entry.Tokens.PromptPerSecond != 200.0 {
		t.Fatalf("PromptPerSecond = %v, want 200", entry.Tokens.PromptPerSecond)
	}
}

// OpenAI-style usage counts cache hits inside prompt_tokens, so the derived
// prompt speed must only count the tokens actually prefilled — otherwise a
// mostly-cached prompt reports a wildly inflated speed (vllm prefix caching).
func TestServer_ProcessStreamingResponse_DerivedRates_ExcludesCachedTokens(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":1100,\"completion_tokens\":11,\"prompt_tokens_details\":{\"cached_tokens\":1000}}}\n\n" +
		"data: [DONE]\n\n")

	start := time.Now()
	firstToken := start.Add(500 * time.Millisecond) // prefill: (1100-1000) tokens / 0.5s = 200 t/s
	lastToken := firstToken.Add(1 * time.Second)

	entry, err := processStreamingResponse("m", start, firstToken, lastToken, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 1100 || entry.Tokens.CachedTokens != 1000 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.PromptPerSecond != 200.0 {
		t.Fatalf("PromptPerSecond = %v, want 200 (cached tokens excluded)", entry.Tokens.PromptPerSecond)
	}
}

// A fully cached prompt has no prefill to measure: speed stays unknown rather
// than reporting a nonsense number.
func TestServer_ProcessStreamingResponse_DerivedRates_FullyCachedPrompt(t *testing.T) {
	body := []byte("data: {\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":1000}}}\n\n" +
		"data: [DONE]\n\n")

	start := time.Now()
	firstToken := start.Add(100 * time.Millisecond)
	lastToken := firstToken.Add(1 * time.Second)

	entry, err := processStreamingResponse("m", start, firstToken, lastToken, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.PromptPerSecond != -1 {
		t.Fatalf("PromptPerSecond = %v, want -1 (fully cached prompt)", entry.Tokens.PromptPerSecond)
	}
}

// Anthropic-style usage reports cache reads in a separate bucket that
// input_tokens already excludes — no subtraction must happen there.
func TestServer_ProcessStreamingResponse_DerivedRates_AnthropicCacheSeparate(t *testing.T) {
	body := []byte("data: {\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":11,\"cache_read_input_tokens\":1000}}}\n\n")

	start := time.Now()
	firstToken := start.Add(500 * time.Millisecond) // prefill: 100 tokens / 0.5s = 200 t/s
	lastToken := firstToken.Add(1 * time.Second)

	entry, err := processStreamingResponse("m", start, firstToken, lastToken, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 100 || entry.Tokens.CachedTokens != 1000 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.PromptPerSecond != 200.0 {
		t.Fatalf("PromptPerSecond = %v, want 200 (no double subtraction)", entry.Tokens.PromptPerSecond)
	}
}

// An upstream that buffers the whole SSE response before sending it delivers a
// single write, so firstToken == lastToken and per-token stream timing is
// unusable. Generation throughput must still be derived from the wall clock
// instead of being reported as unknown. Uses the Anthropic /v1/messages shape
// (usage split across message_start's message.usage and message_delta's usage).
func TestServer_ProcessStreamingResponse_WallClockFallback(t *testing.T) {
	body := []byte("event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":36472,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"},\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":36472,\"output_tokens\":108}}\n\n")

	start := time.Now().Add(-1 * time.Second)
	// Single batched write: firstToken == lastToken, so stream timing is unusable.
	writeAt := time.Now()

	entry, err := processStreamingResponse("m", start, writeAt, writeAt, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 36472 || entry.Tokens.OutputTokens != 108 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	// Throughput derived from wall clock (~108 tokens over ~1s) must be positive,
	// not the -1 "unknown" sentinel.
	if entry.Tokens.TokensPerSecond <= 0 {
		t.Fatalf("TokensPerSecond = %v, want > 0 (wall-clock fallback)", entry.Tokens.TokensPerSecond)
	}
	// Prompt speed cannot be separated from generation here, so it stays unknown.
	if entry.Tokens.PromptPerSecond != -1 {
		t.Fatalf("PromptPerSecond = %v, want -1 (unknown)", entry.Tokens.PromptPerSecond)
	}
}

func TestServer_EnsureStreamUsage(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMod bool
	}{
		{"streaming injects", `{"stream":true,"messages":[]}`, true},
		{"non-streaming untouched", `{"stream":false}`, false},
		{"no stream field untouched", `{"messages":[]}`, false},
		{"already set untouched", `{"stream":true,"stream_options":{"include_usage":false}}`, false},
		{"invalid json untouched", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, mod := ensureStreamUsage([]byte(tc.body))
			if mod != tc.wantMod {
				t.Fatalf("modified = %v, want %v", mod, tc.wantMod)
			}
			if mod && !gjson.GetBytes(out, "stream_options.include_usage").Bool() {
				t.Fatalf("include_usage not set: %s", out)
			}
		})
	}
}

func TestMetricsMonitor_RecordMetadata(t *testing.T) {
	mm := newMetricsMonitor(nil, nil, 10, 0)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"usage":{}}`))
	r = r.WithContext(shared.SetContext(r.Context(), shared.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"client": "web", "trace": "abc"},
	}))

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))

	mm.record("m", r, copier, 0, nil, nil)

	entries := mm.getMetrics()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Metadata["client"] != "web" {
		t.Errorf("client = %q, want web", entries[0].Metadata["client"])
	}
	if entries[0].Metadata["trace"] != "abc" {
		t.Errorf("trace = %q, want abc", entries[0].Metadata["trace"])
	}
}

func TestMetricsMonitor_RecordFailedRequestCapture(t *testing.T) {
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), nil, 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqHeaders := map[string]string{"content-type": "application/json"}

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte(`{"error":{"message":"model unavailable"}}`))

	reqBody := []byte(`{"model":"m","messages":[]}`)
	mm.record("m", r, copier, captureAll, reqBody, reqHeaders)

	entries := mm.getMetrics()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.RespStatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", entry.RespStatusCode, http.StatusBadGateway)
	}
	if entry.ErrorMsg != "model unavailable" {
		t.Errorf("error_msg = %q, want extracted message", entry.ErrorMsg)
	}
	if !entry.HasCapture {
		t.Fatal("failed request should capture the request so it can be inspected")
	}

	got := mm.getCaptureByID(entry.ID)
	if got == nil {
		t.Fatal("capture not found")
	}
	if string(got.ReqBody) != `{"model":"m","messages":[]}` {
		t.Errorf("req body = %q", got.ReqBody)
	}
	if len(got.RespBody) != 0 {
		t.Errorf("resp body stored for failed request (len=%d); want none", len(got.RespBody))
	}
	if got.RespHeaders["Content-Type"] != "application/json" {
		t.Errorf("resp Content-Type = %q", got.RespHeaders["Content-Type"])
	}
}

func TestMetricsMonitor_RecordFailedRequestStatusFallback(t *testing.T) {
	// Non-JSON error body: ErrorMsg falls back to the HTTP status text.
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), nil, 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte("<html>upstream down</html>"))

	mm.record("m", r, copier, captureAll, nil, nil)

	entries := mm.getMetrics()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg != "502 Bad Gateway" {
		t.Errorf("error_msg = %q, want status text", entries[0].ErrorMsg)
	}
}

func TestMetricsMonitor_RecordFailedRequestCaptureDisabled(t *testing.T) {
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), nil, 10, 0) // captures disabled
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusInternalServerError)
	copier.Write([]byte(`{"error":"boom"}`))

	mm.record("m", r, copier, captureAll, []byte("req"), nil)

	entries := mm.getMetrics()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].HasCapture {
		t.Fatal("captures disabled, HasCapture should be false")
	}
	// ErrorMsg is independent of whether captures are enabled.
	if entries[0].ErrorMsg != "boom" {
		t.Errorf("error_msg = %q, want boom", entries[0].ErrorMsg)
	}
	if mm.getCaptureByID(entries[0].ID) != nil {
		t.Fatal("no capture should be stored when disabled")
	}
}

func TestMetricsMonitor_RecordDecompressionFailureSetsErrorMsg(t *testing.T) {
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), nil, 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Encoding", "gzip")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte("not-really-gzip"))

	mm.record("m", r, copier, captureAll, []byte("req"), nil)

	entries := mm.getMetrics()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg == "" {
		t.Fatal("expected ErrorMsg for decompression failure")
	}
	// Raw bytes must not be stored when the body could not be decoded.
	if entries[0].HasCapture {
		t.Fatal("decompression failure should not store a capture")
	}
}

func TestMetricsMonitor_DecodeResponseBody(t *testing.T) {
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), nil, 10, 5)

	// No Content-Encoding: body returned unchanged.
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Write([]byte("plain"))
	got, err := mm.decodeResponseBody(copier, "/p")
	if err != nil || string(got) != "plain" {
		t.Fatalf("plain body = %q, err = %v", got, err)
	}

	// Bogus gzip payload: returns an error and no body (no raw bytes kept).
	w2 := httptest.NewRecorder()
	copier2 := newBodyCopier(w2)
	copier2.Header().Set("Content-Encoding", "gzip")
	copier2.Write([]byte("not-really-gzip"))
	got, err = mm.decodeResponseBody(copier2, "/p")
	if err == nil {
		t.Fatal("expected decompression error")
	}
	if got != nil {
		t.Errorf("expected nil body on failure, got %q", got)
	}
}

// ResetStreamTiming clears recorded write timing (used after loading-state
// chunks) and the next write stamps a fresh firstWrite.
func TestServer_BodyCopier_ResetStreamTiming(t *testing.T) {
	copier := newBodyCopier(httptest.NewRecorder())

	copier.Write([]byte("loading banner"))
	if copier.FirstWrite().IsZero() || copier.LastWrite().IsZero() {
		t.Fatal("write did not stamp stream timing")
	}

	copier.ResetStreamTiming()
	if !copier.FirstWrite().IsZero() || !copier.LastWrite().IsZero() {
		t.Fatal("ResetStreamTiming did not clear stream timing")
	}

	copier.Write([]byte("first upstream token"))
	if copier.FirstWrite().IsZero() {
		t.Fatal("write after reset did not stamp a fresh firstWrite")
	}
}

func TestServer_ExtractErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"openai object", `{"error":{"message":"rate limited"}}`, "rate limited"},
		{"string error", `{"error":"bad request"}`, "bad request"},
		{"message field", `{"message":"nope"}`, "nope"},
		{"detail field", `{"detail":"oops"}`, "oops"},
		{"object error ignored", `{"error":{"code":42}}`, ""},
		{"no error", `{"usage":{}}`, ""},
		{"invalid json", `not-json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("extractErrorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServer_ParseMetrics_Infill(t *testing.T) {
	// /infill responses are arrays; timings live in the last element.
	body := `[{"content":"a"},{"content":"b","timings":{"prompt_n":5,"predicted_n":9,"prompt_ms":10,"predicted_ms":20}}]`
	parsed := gjson.Parse(body)
	timings := parsed.Get("timings")
	if arr := parsed.Array(); len(arr) > 0 {
		timings = arr[len(arr)-1].Get("timings")
	}
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), timings)
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 5 || entry.Tokens.OutputTokens != 9 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

// TestServer_MetricsMiddleware_UpstreamAudioCaptureSkipsRespBody verifies that
// an /upstream/<model>/v1/audio/speech request uses the path-specific capture
// mask (headers only) rather than falling back to captureAll.
func TestServer_MetricsMiddleware_UpstreamAudioCaptureSkipsRespBody(t *testing.T) {
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), nil, 100, 5)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("BINARY-AUDIO-DATA"))
	})
	handler := CreateMetricsMiddleware(mm, cfg)(inner)

	req := httptest.NewRequest(http.MethodPost, "/upstream/m1/v1/audio/speech", strings.NewReader(`{"model":"m1"}`))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	entries := mm.getMetrics()
	if len(entries) == 0 {
		t.Fatal("no metrics recorded")
	}
	last := entries[len(entries)-1]
	if !last.HasCapture {
		t.Fatal("expected capture to be stored")
	}
	cap := mm.getCaptureByID(last.ID)
	if cap == nil {
		t.Fatal("capture not found")
	}
	if len(cap.RespBody) != 0 {
		t.Errorf("RespBody stored for /upstream audio route (len=%d); want path-specific mask to skip body", len(cap.RespBody))
	}
	if len(cap.RespHeaders) == 0 {
		t.Error("RespHeaders not stored; want captureRespHeaders mask")
	}
}
