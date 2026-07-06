package metricsdb

import "time"

// TokenMetrics holds token usage and performance metrics.
type TokenMetrics struct {
	CachedTokens    int     `json:"cache_tokens"`
	DraftTokens     int     `json:"draft_tokens"`
	DraftAccTokens  int     `json:"draft_acc_tokens"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

// Entry is one recorded request in the activity log. JSON tags define the
// wire format served by /api/metrics and the SSE activity events.
type Entry struct {
	ID              int               `json:"id"`
	Timestamp       time.Time         `json:"timestamp"`
	Model           string            `json:"model"`
	ReqPath         string            `json:"req_path"`
	RespContentType string            `json:"resp_content_type"`
	RespStatusCode  int               `json:"resp_status_code"`
	Tokens          TokenMetrics      `json:"tokens"`
	DurationMs      int               `json:"duration_ms"`
	HasCapture      bool              `json:"has_capture"`
	ErrorMsg        string            `json:"error_msg,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// ListFilter narrows and pages a List query. Zero values mean "no filter".
// BeforeID enables keyset pagination: only entries with id < BeforeID are
// returned, newest first.
type ListFilter struct {
	Model    string
	From     time.Time
	To       time.Time
	BeforeID int
	Limit    int
}

// Histogram matches the UI HistogramData shape so it can be rendered directly.
type Histogram struct {
	Bins    []int   `json:"bins"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	BinSize float64 `json:"binSize"`
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
}

// Summary holds SQL-computed aggregates over a filtered range of entries.
// Histograms are nil when no entry in the range has a positive rate.
type Summary struct {
	Requests           int        `json:"requests"`
	Errors             int        `json:"errors"`
	InputTokens        int64      `json:"input_tokens"`
	OutputTokens       int64      `json:"output_tokens"`
	CachedTokens       int64      `json:"cached_tokens"`
	AvgPromptPerSecond float64    `json:"avg_prompt_per_second"`
	AvgTokensPerSecond float64    `json:"avg_tokens_per_second"`
	PromptHistogram    *Histogram `json:"prompt_histogram"`
	GenHistogram       *Histogram `json:"gen_histogram"`
}
