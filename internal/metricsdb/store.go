// Package metricsdb persists per-request activity metrics in SQLite (pure Go
// driver, no CGO) and serves paginated and aggregated queries over them.
package metricsdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	_ "modernc.org/sqlite"
)

const (
	// MemoryPath opens a non-persistent in-memory database.
	MemoryPath = ":memory:"

	pruneInterval = time.Hour
)

var schema = []string{
	`PRAGMA journal_mode=WAL`,
	`PRAGMA busy_timeout=5000`,
	`PRAGMA synchronous=NORMAL`,
	`CREATE TABLE IF NOT EXISTS activity (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp         INTEGER NOT NULL,
		model             TEXT    NOT NULL DEFAULT '',
		req_path          TEXT    NOT NULL DEFAULT '',
		resp_content_type TEXT    NOT NULL DEFAULT '',
		resp_status_code  INTEGER NOT NULL DEFAULT 0,
		cached_tokens     INTEGER NOT NULL DEFAULT -1,
		draft_tokens      INTEGER NOT NULL DEFAULT -1,
		draft_acc_tokens  INTEGER NOT NULL DEFAULT -1,
		input_tokens      INTEGER NOT NULL DEFAULT 0,
		output_tokens     INTEGER NOT NULL DEFAULT 0,
		prompt_per_second REAL    NOT NULL DEFAULT -1,
		tokens_per_second REAL    NOT NULL DEFAULT -1,
		duration_ms       INTEGER NOT NULL DEFAULT 0,
		error_msg         TEXT    NOT NULL DEFAULT '',
		metadata          TEXT    NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_activity_timestamp ON activity(timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_activity_model_timestamp ON activity(model, timestamp)`,
	`PRAGMA user_version = 1`,
}

// Options configures a Store. RetentionDays bounds entry age in file mode
// (0 = keep forever). MemoryRowCap bounds entry count in memory mode.
type Options struct {
	RetentionDays int
	MemoryRowCap  int
	Logger        *logmon.Monitor
}

// Store wraps a single-connection SQLite database holding the activity log.
// It is safe for concurrent use; database/sql serializes access to the one
// connection.
type Store struct {
	db        *sql.DB
	logger    *logmon.Monitor
	memMode   bool
	memRowCap int

	mu            sync.Mutex
	retentionDays int
	stopCancel    context.CancelFunc
}

// Open opens (creating if needed) the metrics database at path. An empty path
// or MemoryPath yields a non-persistent in-memory store capped at
// opts.MemoryRowCap rows.
func Open(path string, opts Options) (*Store, error) {
	memMode := path == "" || path == MemoryPath
	dsn := path
	if memMode {
		dsn = MemoryPath
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening metrics db: %w", err)
	}
	// A single connection avoids SQLITE_BUSY between writers and keeps the
	// :memory: database (which lives per-connection) stable.
	db.SetMaxOpenConns(1)

	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("initializing metrics db schema: %w", err)
		}
	}

	memRowCap := opts.MemoryRowCap
	if memRowCap <= 0 {
		memRowCap = 1000
	}
	return &Store{
		db:            db,
		logger:        opts.Logger,
		memMode:       memMode,
		memRowCap:     memRowCap,
		retentionDays: opts.RetentionDays,
	}, nil
}

// InMemory reports whether the store is non-persistent.
func (s *Store) InMemory() bool {
	return s.memMode
}

// Insert stores an entry and returns its database-assigned ID.
func (s *Store) Insert(e Entry) (int, error) {
	metaJSON := ""
	if len(e.Metadata) > 0 {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return -1, fmt.Errorf("marshaling metadata: %w", err)
		}
		metaJSON = string(b)
	}

	res, err := s.db.Exec(`INSERT INTO activity (
		timestamp, model, req_path, resp_content_type, resp_status_code,
		cached_tokens, draft_tokens, draft_acc_tokens, input_tokens, output_tokens,
		prompt_per_second, tokens_per_second, duration_ms, error_msg, metadata
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Timestamp.UnixMilli(), e.Model, e.ReqPath, e.RespContentType, e.RespStatusCode,
		e.Tokens.CachedTokens, e.Tokens.DraftTokens, e.Tokens.DraftAccTokens,
		e.Tokens.InputTokens, e.Tokens.OutputTokens,
		e.Tokens.PromptPerSecond, e.Tokens.TokensPerSecond,
		e.DurationMs, e.ErrorMsg, metaJSON)
	if err != nil {
		return -1, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return -1, err
	}

	// Memory mode has no retention pruning; bound growth by row count instead.
	// AUTOINCREMENT ids are monotonic, so everything at or below id-cap is old.
	if s.memMode {
		if _, err := s.db.Exec(`DELETE FROM activity WHERE id <= ?`, id-int64(s.memRowCap)); err != nil {
			s.warnf("metricsdb: pruning memory rows: %v", err)
		}
	}
	return int(id), nil
}

// List returns entries matching f, newest first. total counts all entries
// matching the model/date filters (ignoring the pagination cursor) and
// hasMore reports whether older entries exist beyond the returned page.
func (s *Store) List(f ListFilter) (entries []Entry, total int, hasMore bool, err error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	where, args := buildWhere(f.Model, f.From, f.To)

	if err = s.db.QueryRow(`SELECT COUNT(*) FROM activity WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, false, err
	}

	pageWhere, pageArgs := where, args
	if f.BeforeID > 0 {
		pageWhere += " AND id < ?"
		pageArgs = append(append([]any{}, args...), f.BeforeID)
	}

	rows, err := s.db.Query(`SELECT
		id, timestamp, model, req_path, resp_content_type, resp_status_code,
		cached_tokens, draft_tokens, draft_acc_tokens, input_tokens, output_tokens,
		prompt_per_second, tokens_per_second, duration_ms, error_msg, metadata
	FROM activity WHERE `+pageWhere+` ORDER BY id DESC LIMIT ?`,
		append(pageArgs, limit+1)...)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()

	entries = make([]Entry, 0, limit)
	for rows.Next() {
		var e Entry
		var ts int64
		var metaJSON string
		if err := rows.Scan(&e.ID, &ts, &e.Model, &e.ReqPath, &e.RespContentType, &e.RespStatusCode,
			&e.Tokens.CachedTokens, &e.Tokens.DraftTokens, &e.Tokens.DraftAccTokens,
			&e.Tokens.InputTokens, &e.Tokens.OutputTokens,
			&e.Tokens.PromptPerSecond, &e.Tokens.TokensPerSecond,
			&e.DurationMs, &e.ErrorMsg, &metaJSON); err != nil {
			return nil, 0, false, err
		}
		e.Timestamp = time.UnixMilli(ts)
		if metaJSON != "" {
			if err := json.Unmarshal([]byte(metaJSON), &e.Metadata); err != nil {
				s.warnf("metricsdb: unmarshaling metadata for entry %d: %v", e.ID, err)
			}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}

	if len(entries) > limit {
		entries = entries[:limit]
		hasMore = true
	}
	return entries, total, hasMore, nil
}

// Summary computes aggregates and rate histograms over entries matching the
// filters, entirely in SQL.
func (s *Store) Summary(model string, from, to time.Time) (Summary, error) {
	where, args := buildWhere(model, from, to)

	var sum Summary
	err := s.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN resp_status_code != 200 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(MAX(cached_tokens, 0)), 0),
		COALESCE(AVG(CASE WHEN prompt_per_second > 0 THEN prompt_per_second END), 0),
		COALESCE(AVG(CASE WHEN tokens_per_second > 0 THEN tokens_per_second END), 0)
	FROM activity WHERE `+where, args...).Scan(
		&sum.Requests, &sum.Errors, &sum.InputTokens, &sum.OutputTokens,
		&sum.CachedTokens, &sum.AvgPromptPerSecond, &sum.AvgTokensPerSecond)
	if err != nil {
		return Summary{}, err
	}

	if sum.PromptHistogram, err = s.rateHistogram("prompt_per_second", where, args); err != nil {
		return Summary{}, err
	}
	if sum.GenHistogram, err = s.rateHistogram("tokens_per_second", where, args); err != nil {
		return Summary{}, err
	}
	return sum, nil
}

// rateHistogram builds a histogram over the positive values of col (a trusted
// column name) using the same binning as the UI: Sturges' rule clamped to
// 5..20 bins, with linearly interpolated p50/p95/p99. Returns nil when the
// range holds no positive values.
func (s *Store) rateHistogram(col, where string, args []any) (*Histogram, error) {
	w := where + " AND " + col + " > 0"

	var n int
	var minV, maxV sql.NullFloat64
	err := s.db.QueryRow(`SELECT COUNT(*), MIN(`+col+`), MAX(`+col+`) FROM activity WHERE `+w, args...).
		Scan(&n, &minV, &maxV)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}

	h := &Histogram{Min: minV.Float64, Max: maxV.Float64}
	for _, p := range []struct {
		pct  float64
		dest *float64
	}{{50, &h.P50}, {95, &h.P95}, {99, &h.P99}} {
		v, err := s.percentile(col, w, args, n, p.pct)
		if err != nil {
			return nil, err
		}
		*p.dest = v
	}

	if h.Min == h.Max {
		h.Bins = []int{n}
		return h, nil
	}

	binCount := int(math.Ceil(math.Log2(float64(n)))) + 1 // Sturges' rule
	binCount = min(20, max(5, binCount))
	h.BinSize = (h.Max - h.Min) / float64(binCount)
	h.Bins = make([]int, binCount)

	binArgs := append([]any{h.Min, h.BinSize, binCount - 1}, args...)
	rows, err := s.db.Query(`SELECT MIN(CAST((`+col+` - ?) / ? AS INTEGER), ?) AS bin, COUNT(*)
		FROM activity WHERE `+w+` GROUP BY bin`, binArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bin, count int
		if err := rows.Scan(&bin, &count); err != nil {
			return nil, err
		}
		if bin >= 0 && bin < binCount {
			h.Bins[bin] = count
		}
	}
	return h, rows.Err()
}

// percentile returns the p-th percentile of the n positive values of col,
// linearly interpolating between the two nearest ranks.
func (s *Store) percentile(col, where string, args []any, n int, p float64) (float64, error) {
	rank := p / 100 * float64(n-1)
	lower := int(math.Floor(rank))
	frac := rank - float64(lower)

	rows, err := s.db.Query(`SELECT `+col+` FROM activity WHERE `+where+
		` ORDER BY `+col+` LIMIT 2 OFFSET ?`, append(append([]any{}, args...), lower)...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var vals []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		vals = append(vals, v)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch {
	case len(vals) == 0:
		return 0, nil
	case frac == 0 || len(vals) == 1:
		return vals[0], nil
	default:
		return vals[0] + frac*(vals[1]-vals[0]), nil
	}
}

// Prune deletes entries older than the configured retention. It is a no-op
// when retention is 0 (keep forever) or in memory mode (row-capped on insert).
func (s *Store) Prune() (int64, error) {
	s.mu.Lock()
	retention := s.retentionDays
	s.mu.Unlock()

	if s.memMode || retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -retention).UnixMilli()
	res, err := s.db.Exec(`DELETE FROM activity WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdateRetention changes the retention window used by subsequent prunes,
// e.g. after a config reload.
func (s *Store) UpdateRetention(days int) {
	s.mu.Lock()
	s.retentionDays = days
	s.mu.Unlock()
}

// Start launches the background pruner: one prune immediately, then hourly.
// Safe to call on a memory-mode store (prunes are no-ops).
func (s *Store) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stopCancel = cancel

	go func() {
		s.runPrune()
		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runPrune()
			}
		}
	}()
}

func (s *Store) runPrune() {
	pruned, err := s.Prune()
	if err != nil {
		s.warnf("metricsdb: prune failed: %v", err)
		return
	}
	if pruned > 0 {
		s.infof("metricsdb: pruned %d entries past retention", pruned)
	}
}

// Stop halts the background pruner. Safe to call repeatedly.
func (s *Store) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCancel == nil {
		return
	}
	s.stopCancel()
	s.stopCancel = nil
}

// Close stops the pruner and closes the database.
func (s *Store) Close() error {
	s.Stop()
	return s.db.Close()
}

func buildWhere(model string, from, to time.Time) (string, []any) {
	where := "1=1"
	var args []any
	if model != "" {
		where += " AND model = ?"
		args = append(args, model)
	}
	if !from.IsZero() {
		where += " AND timestamp >= ?"
		args = append(args, from.UnixMilli())
	}
	if !to.IsZero() {
		where += " AND timestamp <= ?"
		args = append(args, to.UnixMilli())
	}
	return where, args
}

func (s *Store) infof(format string, args ...any) {
	if s.logger != nil {
		s.logger.Infof(format, args...)
	}
}

func (s *Store) warnf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warnf(format, args...)
	}
}
