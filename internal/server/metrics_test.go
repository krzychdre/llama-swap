package server

import (
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

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
