package server

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

func TestServer_ParseMetrics_ChatCompletions(t *testing.T) {
	body := `{"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 12 || entry.Tokens.OutputTokens != 7 || entry.Tokens.CachedTokens != 4 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ActivitySourceStripsClientPort(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{name: "IPv4", remoteAddr: "192.168.1.10:54321", want: "ip:192.168.1.10"},
		{name: "IPv6", remoteAddr: "[2001:db8::10]:54321", want: "ip:2001:db8::10"},
		{name: "missing port", remoteAddr: "localhost", want: "ip:localhost"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = test.remoteAddr
			if got := activitySource(r); got != test.want {
				t.Fatalf("activitySource() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestServer_ActivitySourceForwardedHeaders(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		setup      func(*http.Request)
		want       string
	}{
		{"x-forwarded-for", "127.0.0.1:54321", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.1")
		}, "xff:203.0.113.10"},
		{"x-real-ip", "127.0.0.1:54321", func(r *http.Request) {
			r.Header.Set("X-Real-IP", "203.0.113.20")
		}, "xff:203.0.113.20"},
		{"x-forwarded-for takes priority over x-real-ip", "127.0.0.1:54321", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "203.0.113.10")
			r.Header.Set("X-Real-IP", "203.0.113.20")
		}, "xff:203.0.113.10"},
		{"x-forwarded-for whitespace-only first entry falls back to x-real-ip", "127.0.0.1:54321", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "   , 10.0.0.1")
			r.Header.Set("X-Real-IP", "203.0.113.20")
		}, "xff:203.0.113.20"},
		{"x-forwarded-for empty first entry falls back to remote addr", "127.0.0.1:54321", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", ",10.0.0.1")
		}, "ip:127.0.0.1"},
		{"headers from a remote client are ignored", "192.168.1.10:54321", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "203.0.113.10")
		}, "ip:192.168.1.10"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = test.remoteAddr
			test.setup(r)
			if got := activitySource(r); got != test.want {
				t.Fatalf("activitySource() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestServer_ParseMetrics_Timings(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":100.0,"predicted_per_second":40.0,"prompt_ms":200,"predicted_ms":1250,"cache_n":8}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
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

func TestServer_ParseMetrics_TabbyAPI(t *testing.T) {
	body := `{"id":"chatcmpl-131b9995cd0c40c99e2ce958083ee406","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":83,"prompt_time":2.36,"prompt_tokens_per_sec":35.17,"completion_tokens":192,"completion_time":11.22,"completion_tokens_per_sec":17.11,"total_tokens":275,"total_time":13.63},"model":"SC_6.00bpw_H6_V6"}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 83 || entry.Tokens.OutputTokens != 192 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, 35.17; math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 17.11; math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
	if entry.DurationMs != 13630 {
		t.Errorf("DurationMs = %d, want 13630", entry.DurationMs)
	}
}

func TestServer_ParseMetrics_OMLX(t *testing.T) {
	tests := []struct {
		name         string
		rates        string
		extra        string
		wantPrompt   float64
		wantGenerate float64
	}{
		{"reported", `,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 63.74, 66.38},
		{"zero", `,"prompt_tokens_per_second":0,"generation_tokens_per_second":0`, "", 0, 0},
		{"null", `,"prompt_tokens_per_second":null,"generation_tokens_per_second":null`, "", -1, -1},
		{"missing", "", "", -1, -1},
		{"prompt only", `,"prompt_tokens_per_second":63.74`, "", 63.74, -1},
		{"generation only", `,"generation_tokens_per_second":66.38`, "", -1, 66.38},
		{"non-numeric", `,"prompt_tokens_per_second":"unknown","generation_tokens_per_second":false`, "", -1, -1},
		{"TabbyAPI precedence", `,"prompt_tokens_per_sec":35.17,"completion_tokens_per_sec":17.11,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 35.17, 17.11},
		{"TabbyAPI zero precedence", `,"prompt_tokens_per_sec":0,"completion_tokens_per_sec":0,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 0, 0},
		{"TabbyAPI string precedence", `,"prompt_tokens_per_sec":"35.17","completion_tokens_per_sec":"17.11","prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 35.17, 17.11},
		{"TabbyAPI null fallback", `,"prompt_tokens_per_sec":null,"completion_tokens_per_sec":null,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 63.74, 66.38},
		{"all null", `,"prompt_tokens_per_sec":null,"completion_tokens_per_sec":null,"prompt_tokens_per_second":null,"generation_tokens_per_second":null`, "", -1, -1},
		{"independent generation fallback", `,"prompt_tokens_per_sec":35.17,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 35.17, 66.38},
		{"independent prompt fallback", `,"completion_tokens_per_sec":17.11,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 63.74, 17.11},
		{"timings precedence", `,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, `,"timings":{"prompt_n":56,"predicted_n":8,"cache_n":16,"prompt_per_second":100,"predicted_per_second":40}`, 100, 40},
		{"vLLM precedence", `,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, `,"metrics":{"time_to_first_token_ms":200,"mean_itl_ms":10}`, 200, 100},
		{"vLLM over timings", `,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, `,"timings":{"prompt_n":56,"predicted_n":8,"cache_n":16,"prompt_per_second":100,"predicted_per_second":40},"metrics":{"time_to_first_token_ms":200,"mean_itl_ms":10}`, 200, 100},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"usage":{"prompt_tokens":56,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":16}` + test.rates + `}` + test.extra + `}`
			for _, streaming := range []bool{false, true} {
				name := "JSON"
				if streaming {
					name = "SSE"
				}
				t.Run(name, func(t *testing.T) {
					var entry ActivityLogEntry
					var err error
					if streaming {
						stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" + "data: " + body + "\n\ndata: [DONE]\n\n"
						entry, err = processStreamingResponse("m", time.Now(), time.Time{}, time.Time{}, []byte(stream))
					} else {
						parsed := gjson.Parse(body)
						entry, err = parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
					}
					if err != nil {
						t.Fatalf("parse metrics: %v", err)
					}
					if entry.Tokens.InputTokens != 56 || entry.Tokens.OutputTokens != 8 || entry.Tokens.CachedTokens != 16 {
						t.Fatalf("tokens = %+v", entry.Tokens)
					}
					if entry.Tokens.PromptPerSecond != test.wantPrompt || entry.Tokens.TokensPerSecond != test.wantGenerate {
						t.Fatalf("rates = %+v, want prompt %v, generation %v", entry.Tokens, test.wantPrompt, test.wantGenerate)
					}
				})
			}
		})
	}
}

func TestServer_MetricsMiddleware_OMLX(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "JSON"
		if streaming {
			name = "SSE"
		}
		t.Run(name, func(t *testing.T) {
			mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
			cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}
			body := `{"usage":{"prompt_tokens":56,"completion_tokens":8,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38}}`
			contentType := "application/json"
			if streaming {
				contentType = "text/event-stream"
				body = "data: " + body + "\n\ndata: [DONE]\n\n"
			}
			handler := chain.New(CreateMetricsMiddleware(mm, cfg)).ThenFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				io.WriteString(w, body)
			})
			r := chatRequest("m")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != body {
				t.Fatalf("response = %d %q, want 200 %q", w.Code, w.Body.String(), body)
			}
			entries := metricsEntries(t, mm)
			if len(entries) != 1 {
				t.Fatalf("want 1 entry, got %d", len(entries))
			}
			entry := entries[0]
			if entry.Model != "m" || entry.Tokens.InputTokens != 56 || entry.Tokens.OutputTokens != 8 || entry.Tokens.PromptPerSecond != 63.74 || entry.Tokens.TokensPerSecond != 66.38 {
				t.Fatalf("recorded activity = %+v", entry)
			}
		})
	}
}

// llama-server timings and TabbyAPI usage rates never appear together, but a
// response carrying both must keep the timings-sourced rates.
func TestServer_ParseMetrics_TabbyAPINotOverwrittenByTimings(t *testing.T) {
	body := `{"usage":{"prompt_tokens":83,"completion_tokens":192,"prompt_tokens_per_sec":35.17,"completion_tokens_per_sec":17.11},"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":100.0,"predicted_per_second":40.0,"prompt_ms":200,"predicted_ms":1250}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.PromptPerSecond != 100.0 || entry.Tokens.TokensPerSecond != 40.0 {
		t.Fatalf("rates = %+v, want timings rates", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_TabbyAPI(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-131b9995cd0c40c99e2ce958083ee406\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":83,\"prompt_time\":2.36,\"prompt_tokens_per_sec\":35.17,\"completion_tokens\":192,\"completion_time\":11.22,\"completion_tokens_per_sec\":17.11,\"total_tokens\":275,\"total_time\":13.63},\"model\":\"SC_6.00bpw_H6_V6\"}\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), time.Time{}, time.Time{}, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 83 || entry.Tokens.OutputTokens != 192 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, 35.17; math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 17.11; math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
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

func TestServer_ProcessStreamingResponse_VLLMMetrics(t *testing.T) {
	body := []byte(`data: {"id":"chatcmpl-b7a832cea986aea4","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":14,"total_tokens":166,"completion_tokens":152},"metrics":{"time_to_first_token_ms":70,"mean_itl_ms":10,"tokens_per_second":24.116032676555495}}

data: [DONE]
`)
	entry, err := processStreamingResponse("m", time.Now(), time.Time{}, time.Time{}, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 14 || entry.Tokens.OutputTokens != 152 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.CachedTokens != -1 {
		t.Errorf("CachedTokens = %d, want -1", entry.Tokens.CachedTokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, 200.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if entry.Tokens.TokensPerSecond != 100 {
		t.Errorf("TokensPerSecond = %v, want 100", entry.Tokens.TokensPerSecond)
	}
}

func TestServer_ParseMetrics_VLLMMetrics(t *testing.T) {
	body := `{"id":"chatcmpl-abc123","object":"chat.completion","usage":{"prompt_tokens":42,"completion_tokens":128,"total_tokens":170,"prompt_tokens_details":{"cached_tokens":20}},"metrics":{"time_to_first_token_ms":85.2,"generation_time_ms":1240.5,"queue_time_ms":12.3,"mean_itl_ms":9.1,"tokens_per_second":103.2}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 42 || entry.Tokens.OutputTokens != 128 || entry.Tokens.CachedTokens != 20 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, float64(42-20)/(85.2/1000); math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 1000/9.1; math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
}

func TestServer_ParseMetrics_VLLMSpeculativeDecoding(t *testing.T) {
	body := `{"id":"chatcmpl-abc123","object":"chat.completion","usage":{"prompt_tokens":42,"completion_tokens":128},"metrics":{"mean_itl_ms":9.1,"speculative_decoding":{"mean_acceptance_length":1.7,"draft_acceptance_rate":0.7,"acceptance_histogram":[6,14],"num_spec_steps":20,"num_accepted_draft_tokens":14,"num_draft_tokens":20,"num_spec_tokens":1}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != 20 || entry.Tokens.DraftAccTokens != 14 {
		t.Fatalf("draft tokens = %+v, want 14/20", entry.Tokens)
	}
}

// A speculative_decoding object missing either counter must leave both unset so
// the acceptance rate is not computed from half the data.
func TestServer_ParseMetrics_VLLMSpeculativeDecodingPartial(t *testing.T) {
	body := `{"usage":{"prompt_tokens":42,"completion_tokens":128},"metrics":{"speculative_decoding":{"num_draft_tokens":20}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != -1 || entry.Tokens.DraftAccTokens != -1 {
		t.Fatalf("draft tokens = %+v, want -1/-1", entry.Tokens)
	}
}

// vLLM only emits the metrics object on the final streamed chunk.
func TestServer_ProcessStreamingResponse_VLLMSpeculativeDecoding(t *testing.T) {
	body := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":11742,"completion_tokens":35},"metrics":{"mean_itl_ms":17.209908293272534,"speculative_decoding":{"mean_acceptance_length":1.7,"draft_acceptance_rate":0.7,"num_spec_steps":20,"num_accepted_draft_tokens":14,"num_draft_tokens":20,"num_spec_tokens":1}}}

data: [DONE]
`)
	entry, err := processStreamingResponse("m", time.Now(), time.Time{}, time.Time{}, body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.DraftTokens != 20 || entry.Tokens.DraftAccTokens != 14 {
		t.Fatalf("draft tokens = %+v, want 14/20", entry.Tokens)
	}
}

// llama-server timings and vLLM metrics never appear together, but a response
// carrying both must not have its timings-sourced draft counts clobbered.
func TestServer_ParseMetrics_TimingsDraftTokensNotOverwritten(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"draft_n":30,"draft_n_accepted":12},"metrics":{"speculative_decoding":{}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != 30 || entry.Tokens.DraftAccTokens != 12 {
		t.Fatalf("draft tokens = %+v, want 12/30", entry.Tokens)
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
	mm := newTestMetricsMonitor(t, nil, 10, 0)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"usage":{}}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"client": "web", "trace": "abc"},
	}))

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))

	mm.record("m", r, copier, 0, nil, nil)

	entries := metricsEntries(t, mm)
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

// TestMetricsMonitor_RecordClientClosed covers #1029: a client that hangs up
// before a response is written must not be filed as a successful (empty-body)
// metric. It is recorded with the 499 sentinel and a client-cancelled
// ErrorMsg, and skips the capture the upstream-failure path would store.
func TestMetricsMonitor_RecordClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	// Nothing is ever written: this is the cold-load cancellation shape.
	copier.MarkStatus(swaputil.StatusClientClosedRequest)

	mm.record("m", r, copier, captureAll, []byte("req"), nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.RespStatusCode != swaputil.StatusClientClosedRequest {
		t.Errorf("status = %d, want %d", entry.RespStatusCode, swaputil.StatusClientClosedRequest)
	}
	if entry.ErrorMsg != "client disconnected before response" {
		t.Errorf("error_msg = %q, want client-cancelled message", entry.ErrorMsg)
	}
	if entry.HasCapture {
		t.Error("a cancelled request has no response to capture")
	}
}

// TestServer_MetricsMiddleware_ClientClosed checks the middleware derives the
// sentinel for a handler that returns without writing, and that the marker
// propagates outward to the access-log recorder so both agree.
func TestServer_MetricsMiddleware_ClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}

	proxylog := logmon.NewWriter(io.Discard)
	handler := chain.New(
		CreateRequestLogMiddleware(proxylog),
		CreateMetricsMiddleware(mm, cfg),
	).ThenFunc(func(w http.ResponseWriter, r *http.Request) {})

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.168.1.1:5000"
	cancel()

	handler.ServeHTTP(httptest.NewRecorder(), r)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].RespStatusCode != swaputil.StatusClientClosedRequest {
		t.Errorf("activity status = %d, want %d", entries[0].RespStatusCode, swaputil.StatusClientClosedRequest)
	}
	if line := string(proxylog.GetHistory()); !strings.Contains(line, "499 0") {
		t.Errorf("access log %q should report 499", line)
	}
}

// TestServer_MetricsMiddleware_ServerSideCancelIsNotClientClosed guards the
// distinction #1029's fix depends on. The inflight middleware derives a
// cancellable context that POST /api/inflight/{id}/cancel fires, so testing
// "is this request's context done?" would report an operator cancelling a
// request from the UI as a client that hung up.
func TestServer_MetricsMiddleware_ServerSideCancelIsNotClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}

	proxylog := logmon.NewWriter(io.Discard)
	// The handler stands in for a dispatch that is cancelled mid-flight and
	// answers the still-connected client, as the proxy ErrorHandler does.
	handler := chain.New(
		CreateRequestLogMiddleware(proxylog),
		CreateMetricsMiddleware(mm, cfg),
	).ThenFunc(func(w http.ResponseWriter, r *http.Request) {
		derived, cancel := context.WithCancel(r.Context())
		defer cancel()
		cancel() // the operator cancels it
		r = r.WithContext(derived)
		if swaputil.MarkClientClosed(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.168.1.1:5000"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("client got %d, want %d: a connected client must be answered", w.Code, http.StatusBadGateway)
	}
	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].RespStatusCode == swaputil.StatusClientClosedRequest {
		t.Error("a server-side cancel must not be recorded as a client disconnect")
	}
	if line := string(proxylog.GetHistory()); strings.Contains(line, "499") {
		t.Errorf("access log %q should not report 499 for a connected client", line)
	}
}

func TestMetricsMonitor_RecordFailedRequestCapture(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqHeaders := map[string]string{"content-type": "application/json"}

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte(`{"error":{"message":"model unavailable"}}`))

	reqBody := []byte(`{"model":"m","messages":[]}`)
	mm.record("m", r, copier, captureAll, reqBody, reqHeaders)

	entries := metricsEntries(t, mm)
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
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte("<html>upstream down</html>"))

	mm.record("m", r, copier, captureAll, nil, nil)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg != "502 Bad Gateway" {
		t.Errorf("error_msg = %q, want status text", entries[0].ErrorMsg)
	}
}

func TestMetricsMonitor_RecordFailedRequestCaptureDisabled(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0) // captures disabled
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusInternalServerError)
	copier.Write([]byte(`{"error":"boom"}`))

	mm.record("m", r, copier, captureAll, []byte("req"), nil)

	entries := metricsEntries(t, mm)
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
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Encoding", "gzip")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte("not-really-gzip"))

	mm.record("m", r, copier, captureAll, []byte("req"), nil)

	entries := metricsEntries(t, mm)
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
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)

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
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), timings, parsed.Get("metrics"))
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
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 100, 5)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("BINARY-AUDIO-DATA"))
	})
	handler := CreateMetricsMiddleware(mm, cfg)(inner)

	req := httptest.NewRequest(http.MethodPost, "/upstream/m1/v1/audio/speech", strings.NewReader(`{"model":"m1"}`))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	entries := metricsEntries(t, mm)
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

func TestMetricsMonitor_QueueMetricsRetention(t *testing.T) {
	tests := []struct {
		name          string
		retentionDays int
		wantModels    []string
	}{
		{name: "prunes rows past retention", retentionDays: 7, wantModels: []string{"fresh"}},
		{name: "zero keeps every row", retentionDays: 0, wantModels: []string{"fresh", "old"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mm := newTestMetricsMonitor(t, nil, 10, 0)
			mm.retentionDays = tt.retentionDays

			now := time.Now()
			for _, e := range []ActivityLogEntry{
				{Model: "old", Timestamp: now.AddDate(0, 0, -8)},
				{Model: "fresh", Timestamp: now},
			} {
				if _, ok := mm.queueMetrics(e); !ok {
					t.Fatalf("queueMetrics(%s) failed", e.Model)
				}
			}

			var got []string
			for _, e := range metricsEntries(t, mm) {
				got = append(got, e.Model)
			}
			if !slices.Equal(got, tt.wantModels) {
				t.Errorf("models = %v, want %v", got, tt.wantModels)
			}
		})
	}
}
