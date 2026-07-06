package server

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/cache"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/metricsdb"
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/tidwall/gjson"
)

// TokenMetrics and ActivityLogEntry are canonically defined in metricsdb;
// aliased here so server code keeps its established names.
type TokenMetrics = metricsdb.TokenMetrics
type ActivityLogEntry = metricsdb.Entry

// ActivityLogEvent carries a single activity log entry to event subscribers.
type ActivityLogEvent struct {
	Metrics ActivityLogEntry
}

func (e ActivityLogEvent) Type() uint32 {
	return shared.ActivityLogEventID
}

// metricsMonitor parses upstream responses for token statistics, persists
// activity entries to the metrics store, and (when captures are enabled)
// stores zstd+CBOR-compressed request/response captures in a sized cache.
type metricsMonitor struct {
	store      *metricsdb.Store
	ownedStore bool // store opened here rather than supplied; closed by close()
	logger     *logmon.Monitor

	enableCaptures bool
	captureCache   *cache.Cache // zstd-compressed CBOR of ReqRespCapture
}

// newMetricsMonitor creates a metricsMonitor recording into store. A nil
// store gets an owned in-memory store retaining up to maxMetrics entries.
// captureBufferMB is the capture buffer size in megabytes; 0 disables captures.
func newMetricsMonitor(logger *logmon.Monitor, store *metricsdb.Store, maxMetrics int, captureBufferMB int) *metricsMonitor {
	ownedStore := false
	if store == nil {
		var err error
		store, err = metricsdb.Open(metricsdb.MemoryPath, metricsdb.Options{MemoryRowCap: maxMetrics, Logger: logger})
		if err != nil {
			// An in-memory open cannot fail short of sqlite itself being broken.
			panic(fmt.Sprintf("opening in-memory metrics store: %v", err))
		}
		ownedStore = true
	}
	mm := &metricsMonitor{
		store:          store,
		ownedStore:     ownedStore,
		logger:         logger,
		enableCaptures: captureBufferMB > 0,
	}
	if captureBufferMB > 0 {
		mm.captureCache = cache.New(captureBufferMB * 1024 * 1024)
	}
	return mm
}

// queueMetrics persists a metric and returns its database-assigned ID, or -1
// when the insert failed.
func (mp *metricsMonitor) queueMetrics(metric ActivityLogEntry) int {
	id, err := mp.store.Insert(metric)
	if err != nil && mp.logger != nil {
		mp.logger.Warnf("metrics: failed to store activity entry: %v", err)
	}
	return id
}

// emitMetric publishes an ActivityLogEvent for the given metric.
func (mp *metricsMonitor) emitMetric(metric ActivityLogEntry) {
	event.Emit(ActivityLogEvent{Metrics: metric})
}

// metricsPage is the paginated /api/metrics response payload.
type metricsPage struct {
	Entries []ActivityLogEntry `json:"entries"`
	Total   int                `json:"total"`
	HasMore bool               `json:"has_more"`
}

// list returns a page of stored metrics, newest first, annotating HasCapture
// from the in-memory capture cache.
func (mp *metricsMonitor) list(f metricsdb.ListFilter) (metricsPage, error) {
	entries, total, hasMore, err := mp.store.List(f)
	if err != nil {
		return metricsPage{}, err
	}
	if entries == nil {
		entries = []ActivityLogEntry{}
	}
	if mp.captureCache != nil {
		for i := range entries {
			entries[i].HasCapture = mp.captureCache.Has(entries[i].ID)
		}
	}
	return metricsPage{Entries: entries, Total: total, HasMore: hasMore}, nil
}

// summary returns SQL-computed aggregates over the filtered range.
func (mp *metricsMonitor) summary(model string, from, to time.Time) (metricsdb.Summary, error) {
	return mp.store.Summary(model, from, to)
}

// close releases the owned in-memory store, if any. Externally supplied
// stores outlive the server (they are owned by main and survive reloads).
func (mp *metricsMonitor) close() {
	if mp.ownedStore {
		mp.store.Close()
	}
}

// record parses a completed response body and stores/emits an activity entry.
// Successful requests store a zstd+CBOR capture (when enabled) with cf
// controlling which parts are retained. Failed (non-200) requests capture the
// request only and set ErrorMsg to a description of the failure, so the error
// can be inspected without storing unreadable raw response bytes. reqBody and
// reqHeaders are the request data buffered before dispatch.
func (mp *metricsMonitor) record(modelID string, r *http.Request, recorder *responseBodyCopier, cf captureFields, reqBody []byte, reqHeaders map[string]string) {
	tm := ActivityLogEntry{
		Timestamp:       time.Now(),
		Model:           modelID,
		ReqPath:         r.URL.Path,
		RespContentType: recorder.Header().Get("Content-Type"),
		RespStatusCode:  recorder.Status(),
		DurationMs:      int(time.Since(recorder.StartTime()).Milliseconds()),
	}

	if ctxData, ok := shared.ReadContext(r.Context()); ok && len(ctxData.Metadata) > 0 {
		tm.Metadata = make(map[string]string, len(ctxData.Metadata))
		for k, v := range ctxData.Metadata {
			tm.Metadata[k] = v
		}
	}

	queueAndEmit := func() {
		tm.ID = mp.queueMetrics(tm)
		mp.emitMetric(tm)
	}

	if recorder.Status() != http.StatusOK {
		mp.logger.Warnf("non-200 response, recording partial metrics: status=%d, path=%s", recorder.Status(), r.URL.Path)
		decoded, decErr := mp.decodeResponseBody(recorder, r.URL.Path)
		tm.ErrorMsg = failedErrorMessage(recorder.Status(), decoded, decErr)
		tm.ID = mp.queueMetrics(tm)
		// Capture the request only; the failure is surfaced via ErrorMsg
		// rather than storing the (possibly undisplayable) response body.
		tm.HasCapture = mp.storeCapture(tm.ID, r, recorder, cf&^captureRespBody, reqBody, reqHeaders, nil)
		mp.emitMetric(tm)
		return
	}

	body := recorder.body.Bytes()
	if len(body) == 0 {
		mp.logger.Warn("metrics: empty body, recording minimal metrics")
		queueAndEmit()
		return
	}

	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "" {
		decoded, err := decompressBody(body, encoding)
		if err != nil {
			mp.logger.Warnf("metrics: decompression failed: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			tm.ErrorMsg = fmt.Sprintf("response decompression failed: %v", err)
			queueAndEmit()
			return
		}
		body = decoded
	}

	if strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
		if parsed, err := processStreamingResponse(modelID, recorder.StartTime(), recorder.FirstWrite(), recorder.LastWrite(), body); err != nil {
			mp.logger.Warnf("error processing streaming response: %v, path=%s, recording minimal metrics", err, r.URL.Path)
		} else {
			tm.Tokens = parsed.Tokens
			tm.DurationMs = parsed.DurationMs
		}
	} else if gjson.ValidBytes(body) {
		parsed := gjson.ParseBytes(body)
		usage := parsed.Get("usage")
		timings := parsed.Get("timings")

		// /infill responses are arrays; timings live in the last element (#463).
		if strings.HasPrefix(r.URL.Path, "/infill") {
			if arr := parsed.Array(); len(arr) > 0 {
				timings = arr[len(arr)-1].Get("timings")
			}
		}

		if usage.Exists() || timings.Exists() {
			if parsedMetrics, err := parseMetrics(modelID, recorder.StartTime(), usage, timings); err != nil {
				mp.logger.Warnf("error parsing metrics: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			} else {
				tm.Tokens = parsedMetrics.Tokens
				tm.DurationMs = parsedMetrics.DurationMs
			}
		}
	} else {
		mp.logger.Warnf("metrics: invalid JSON in response body path=%s, recording minimal metrics", r.URL.Path)
	}

	tm.ID = mp.queueMetrics(tm)
	tm.HasCapture = mp.storeCapture(tm.ID, r, recorder, cf, reqBody, reqHeaders, body)
	mp.emitMetric(tm)
}

// storeCapture assembles a ReqRespCapture for id, honoring the captureFields
// mask, and stores it when captures are enabled. body is the response body to
// capture (already decompressed by the caller); pass nil to omit it. Returns
// true if a capture was stored.
func (mp *metricsMonitor) storeCapture(id int, r *http.Request, recorder *responseBodyCopier, cf captureFields, reqBody []byte, reqHeaders map[string]string, body []byte) bool {
	// id < 0 means the entry failed to persist; a capture would be unreachable.
	if !mp.enableCaptures || id < 0 {
		return false
	}
	capture := ReqRespCapture{
		ID:         id,
		ReqPath:    r.URL.Path,
		ReqHeaders: reqHeaders,
	}
	if cf&captureReqBody != 0 {
		capture.ReqBody = reqBody
	}
	if cf&captureRespHeaders != 0 {
		capture.RespHeaders = headerMap(recorder.Header())
		redactHeaders(capture.RespHeaders)
		delete(capture.RespHeaders, "Content-Encoding")
	}
	if cf&captureRespBody != 0 {
		capture.RespBody = body
	}
	return mp.addCapture(capture)
}

// decodeResponseBody returns the buffered response body, decompressing it when
// the upstream set a Content-Encoding we recognize. On decompression failure it
// logs a warning and returns an error so the caller can record a description
// (via ErrorMsg) instead of storing unreadable raw bytes.
func (mp *metricsMonitor) decodeResponseBody(recorder *responseBodyCopier, path string) ([]byte, error) {
	body := recorder.body.Bytes()
	if len(body) == 0 {
		return nil, nil
	}
	encoding := recorder.Header().Get("Content-Encoding")
	if encoding == "" {
		return body, nil
	}
	decoded, err := decompressBody(body, encoding)
	if err != nil {
		mp.logger.Warnf("metrics: response decompression failed: %v, path=%s", err, path)
		return nil, err
	}
	return decoded, nil
}

// errorMessagePaths lists JSON paths where a human-readable error message can
// live across OpenAI- and llama.cpp-style error responses.
var errorMessagePaths = []string{"error.message", "error", "message", "detail"}

// extractErrorMessage pulls a human-readable error string from a JSON error
// response. Returns "" if no message is found or the body is not valid JSON.
func extractErrorMessage(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	parsed := gjson.ParseBytes(body)
	for _, path := range errorMessagePaths {
		v := parsed.Get(path)
		if v.Exists() && v.Type == gjson.String {
			if s := strings.TrimSpace(v.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

// failedErrorMessage builds a human-readable description for a non-200 response.
// It prefers an error message parsed from the (decompressed) body and falls back
// to the HTTP status text. A non-nil decErr indicates the body could not be
// decoded, in which case the decode error is described instead.
func failedErrorMessage(status int, body []byte, decErr error) string {
	const maxLen = 500
	if decErr != nil {
		return fmt.Sprintf("response decode failed: %v", decErr)
	}
	if msg := extractErrorMessage(body); msg != "" {
		if len(msg) > maxLen {
			msg = msg[:maxLen] + "..."
		}
		return msg
	}
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("%d %s", status, text)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// usagePaths lists the JSON paths where a per-event usage object can live.
var usagePaths = []string{"usage", "response.usage", "message.usage"}

// extractUsageTokens reads input/output/cached token counts from a usage
// gjson.Result, handling the field-name differences across endpoints.
// cachedInInput reports whether the cached count is a subset of input
// (OpenAI-style *_tokens_details.cached_tokens) rather than a separate
// bucket (Anthropic's cache_read_input_tokens, which input_tokens excludes).
func extractUsageTokens(usage gjson.Result) (input, output, cached int64, cachedInInput, ok bool) {
	cached = -1
	if !usage.Exists() {
		return
	}

	if v := usage.Get("prompt_tokens"); v.Exists() {
		input = v.Int()
		ok = true
	} else if v := usage.Get("input_tokens"); v.Exists() {
		input = v.Int()
		ok = true
	}

	if v := usage.Get("completion_tokens"); v.Exists() {
		output = v.Int()
		ok = true
	} else if v := usage.Get("output_tokens"); v.Exists() {
		output = v.Int()
		ok = true
	}

	if v := usage.Get("cache_read_input_tokens"); v.Exists() {
		cached = v.Int()
		ok = true
	} else if v := usage.Get("input_tokens_details.cached_tokens"); v.Exists() {
		cached = v.Int()
		cachedInInput = true
		ok = true
	} else if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
		cached = v.Int()
		cachedInInput = true
		ok = true
	}
	return
}

func processStreamingResponse(modelID string, start, firstToken, lastToken time.Time, body []byte) (ActivityLogEntry, error) {
	var (
		inputTokens, outputTokens int64
		cachedTokens              int64 = -1
		cachedInInput             bool
		hasAny                    bool
		timings                   gjson.Result
	)

	prefix := []byte("data:")
	for offset := 0; offset < len(body); {
		nl := bytes.IndexByte(body[offset:], '\n')
		var line []byte
		if nl == -1 {
			line = body[offset:]
			offset = len(body)
		} else {
			line = body[offset : offset+nl]
			offset += nl + 1
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, prefix) {
			continue
		}
		data := bytes.TrimSpace(line[len(prefix):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(data) {
			continue
		}
		parsed := gjson.ParseBytes(data)

		for _, path := range usagePaths {
			u := parsed.Get(path)
			if !u.Exists() {
				continue
			}
			i, o, c, cInInput, ok := extractUsageTokens(u)
			if !ok {
				continue
			}
			hasAny = true
			if i > 0 {
				inputTokens = i
			}
			if o > 0 {
				outputTokens = o
			}
			if c >= 0 {
				cachedTokens = c
				cachedInInput = cInInput
			}
		}
		if t := parsed.Get("timings"); t.Exists() {
			timings = t
			hasAny = true
		}
	}

	if !hasAny {
		return ActivityLogEntry{}, fmt.Errorf("no valid JSON data found in stream")
	}

	return buildMetrics(modelID, start, inputTokens, outputTokens, cachedTokens, cachedInInput, timings, firstToken, lastToken), nil
}

func parseMetrics(modelID string, start time.Time, usage, timings gjson.Result) (ActivityLogEntry, error) {
	input, output, cached, cachedInInput, _ := extractUsageTokens(usage)
	// Non-streaming responses have no per-token stream timing, so rates are only
	// available when the backend supplies llama.cpp timings.
	return buildMetrics(modelID, start, input, output, cached, cachedInInput, timings, time.Time{}, time.Time{}), nil
}

// buildMetrics composes an ActivityLogEntry from accumulated token counts and
// optional llama-server timings (which override input/output and provide rates).
// For streaming backends that omit timings (e.g. vllm), firstToken/lastToken
// bracket the generated stream so rates can be derived from the proxy's own
// timing; they are zero for non-streaming responses. cachedInInput indicates
// that cachedTokens is counted inside inputTokens (OpenAI-style usage).
func buildMetrics(modelID string, start time.Time, inputTokens, outputTokens, cachedTokens int64, cachedInInput bool, timings gjson.Result, firstToken, lastToken time.Time) ActivityLogEntry {
	wallDurationMs := int(time.Since(start).Milliseconds())
	durationMs := wallDurationMs
	tokensPerSecond := -1.0
	promptPerSecond := -1.0
	draftTokens := -1
	draftAccTokens := -1

	if timings.Exists() {
		inputTokens = timings.Get("prompt_n").Int()
		outputTokens = timings.Get("predicted_n").Int()
		promptPerSecond = timings.Get("prompt_per_second").Float()
		tokensPerSecond = timings.Get("predicted_per_second").Float()
		timingsDurationMs := int(timings.Get("prompt_ms").Float() + timings.Get("predicted_ms").Float())
		if timingsDurationMs > durationMs {
			durationMs = timingsDurationMs
		}
		if cachedValue := timings.Get("cache_n"); cachedValue.Exists() {
			cachedTokens = cachedValue.Int()
		}
		if timings.Get("draft_n").Exists() && timings.Get("draft_n_accepted").Exists() {
			draftTokens = int(timings.Get("draft_n").Int())
			draftAccTokens = int(timings.Get("draft_n_accepted").Int())
		}
	} else if !firstToken.IsZero() && lastToken.After(firstToken) {
		// Derive rates from stream timing: start..firstToken approximates prompt
		// processing (queue + prefill); firstToken..lastToken brackets the
		// generation of the remaining output tokens.
		if genSecs := lastToken.Sub(firstToken).Seconds(); genSecs > 0 && outputTokens > 1 {
			tokensPerSecond = float64(outputTokens-1) / genSecs
		}
		// Prompt speed must count only tokens actually prefilled. OpenAI-style
		// usage includes cache hits in the prompt count, so strip them before
		// dividing by TTFT; Anthropic-style reports them separately already.
		// A fully cached prompt leaves the speed unknown (-1).
		prefillTokens := inputTokens
		if cachedInInput && cachedTokens > 0 {
			prefillTokens -= cachedTokens
		}
		if ttftSecs := firstToken.Sub(start).Seconds(); ttftSecs > 0 && prefillTokens > 0 {
			promptPerSecond = float64(prefillTokens) / ttftSecs
		}
	}

	// Fallback: when neither llama.cpp timings nor usable per-token stream timing
	// are available - e.g. an upstream that buffers the whole response before
	// sending it (so firstWrite == lastWrite), or a non-streaming response -
	// approximate generation throughput from the wall-clock duration. This is
	// output tokens over the full request time, so it is a lower bound on the
	// true generation speed for prompt-heavy requests. Prompt speed is left
	// unknown because prompt and generation time cannot be separated here.
	if tokensPerSecond < 0 && outputTokens > 0 && wallDurationMs > 0 {
		tokensPerSecond = float64(outputTokens) / (float64(wallDurationMs) / 1000.0)
	}

	return ActivityLogEntry{
		Timestamp: time.Now(),
		Model:     modelID,
		Tokens: TokenMetrics{
			CachedTokens:    int(cachedTokens),
			DraftTokens:     draftTokens,
			DraftAccTokens:  draftAccTokens,
			InputTokens:     int(inputTokens),
			OutputTokens:    int(outputTokens),
			PromptPerSecond: promptPerSecond,
			TokensPerSecond: tokensPerSecond,
		},
		DurationMs: durationMs,
	}
}

// decompressBody decompresses the body based on the Content-Encoding header.
func decompressBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	case "deflate":
		reader := flate.NewReader(bytes.NewReader(body))
		defer reader.Close()
		return io.ReadAll(reader)
	default:
		return body, nil
	}
}

// filterAcceptEncoding filters Accept-Encoding to only gzip/deflate so response
// bodies remain decompressible for metrics parsing.
func filterAcceptEncoding(acceptEncoding string) string {
	if acceptEncoding == "" {
		return ""
	}

	supported := map[string]bool{"gzip": true, "deflate": true}
	var filtered []string
	for part := range strings.SplitSeq(acceptEncoding, ",") {
		encoding, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if supported[strings.ToLower(encoding)] {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}
	return strings.Join(filtered, ", ")
}

// responseBodyCopier tees the upstream response to the client while buffering
// it for metrics parsing. Status defaults to 200 until WriteHeader is called.
type responseBodyCopier struct {
	http.ResponseWriter
	body        *bytes.Buffer
	tee         io.Writer
	status      int
	wroteHeader bool
	start       time.Time
	firstWrite  time.Time
	lastWrite   time.Time
}

func newBodyCopier(w http.ResponseWriter) *responseBodyCopier {
	buf := &bytes.Buffer{}
	return &responseBodyCopier{
		ResponseWriter: w,
		body:           buf,
		tee:            io.MultiWriter(w, buf),
		status:         http.StatusOK,
		start:          time.Now(),
	}
}

func (w *responseBodyCopier) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	// On a protocol upgrade (e.g. websocket) the body is raw framed data, not a
	// metrics-parseable response, so write straight to the client without
	// buffering a copy we can't use.
	if w.status == http.StatusSwitchingProtocols {
		return w.ResponseWriter.Write(b)
	}
	// Record write timing so streaming backends without llama.cpp timings can
	// have generation rates derived from the stream itself.
	now := time.Now()
	if w.firstWrite.IsZero() {
		w.firstWrite = now
	}
	w.lastWrite = now
	return w.tee.Write(b)
}

func (w *responseBodyCopier) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

// Flush forwards to the underlying writer so streaming responses still flush.
func (w *responseBodyCopier) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer so httputil.ReverseProxy can take
// over the connection for websocket upgrades.
func (w *responseBodyCopier) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

func (w *responseBodyCopier) Status() int           { return w.status }
func (w *responseBodyCopier) StartTime() time.Time  { return w.start }
func (w *responseBodyCopier) FirstWrite() time.Time { return w.firstWrite }
func (w *responseBodyCopier) LastWrite() time.Time  { return w.lastWrite }

// ResetStreamTiming discards the write timing recorded so far, so bytes
// written before the upstream handler runs (e.g. the router's loading-state
// SSE chunks) don't masquerade as the upstream's first token and wreck the
// derived token rates. The next Write stamps a fresh firstWrite.
func (w *responseBodyCopier) ResetStreamTiming() {
	w.firstWrite = time.Time{}
	w.lastWrite = time.Time{}
}
