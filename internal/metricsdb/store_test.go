package metricsdb

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testEntry(model string, ts time.Time) Entry {
	return Entry{
		Timestamp:       ts,
		Model:           model,
		ReqPath:         "/v1/chat/completions",
		RespContentType: "application/json",
		RespStatusCode:  200,
		Tokens: TokenMetrics{
			CachedTokens:    -1,
			DraftTokens:     -1,
			DraftAccTokens:  -1,
			InputTokens:     100,
			OutputTokens:    50,
			PromptPerSecond: 500,
			TokensPerSecond: 25,
		},
		DurationMs: 1234,
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(MemoryPath, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store
}

func TestMetricsDB_InsertAndList(t *testing.T) {
	store := openTestStore(t)

	now := time.Now()
	e := testEntry("model1", now)
	e.ErrorMsg = "boom"
	e.Metadata = map[string]string{"user": "alice"}

	id, err := store.Insert(e)
	require.NoError(t, err)
	assert.Equal(t, 1, id)

	entries, total, hasMore, err := store.List(ListFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.False(t, hasMore)
	require.Len(t, entries, 1)

	got := entries[0]
	assert.Equal(t, 1, got.ID)
	assert.Equal(t, now.UnixMilli(), got.Timestamp.UnixMilli())
	assert.Equal(t, "model1", got.Model)
	assert.Equal(t, "/v1/chat/completions", got.ReqPath)
	assert.Equal(t, "application/json", got.RespContentType)
	assert.Equal(t, 200, got.RespStatusCode)
	assert.Equal(t, e.Tokens, got.Tokens)
	assert.Equal(t, 1234, got.DurationMs)
	assert.Equal(t, "boom", got.ErrorMsg)
	assert.Equal(t, map[string]string{"user": "alice"}, got.Metadata)
}

func TestMetricsDB_KeysetPagination(t *testing.T) {
	store := openTestStore(t)

	for i := 0; i < 25; i++ {
		_, err := store.Insert(testEntry("model1", time.Now()))
		require.NoError(t, err)
	}

	// First page: newest first (ids 25..16).
	entries, total, hasMore, err := store.List(ListFilter{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 25, total)
	assert.True(t, hasMore)
	require.Len(t, entries, 10)
	assert.Equal(t, 25, entries[0].ID)
	assert.Equal(t, 16, entries[9].ID)

	// Second page via cursor.
	entries, total, hasMore, err = store.List(ListFilter{Limit: 10, BeforeID: entries[9].ID})
	require.NoError(t, err)
	assert.Equal(t, 25, total)
	assert.True(t, hasMore)
	require.Len(t, entries, 10)
	assert.Equal(t, 15, entries[0].ID)
	assert.Equal(t, 6, entries[9].ID)

	// Last page is short and has no more.
	entries, _, hasMore, err = store.List(ListFilter{Limit: 10, BeforeID: entries[9].ID})
	require.NoError(t, err)
	assert.False(t, hasMore)
	require.Len(t, entries, 5)
	assert.Equal(t, 5, entries[0].ID)
	assert.Equal(t, 1, entries[4].ID)
}

func TestMetricsDB_DateAndModelFilter(t *testing.T) {
	store := openTestStore(t)

	base := time.Now().Add(-10 * time.Hour)
	for i := 0; i < 10; i++ {
		model := "modelA"
		if i%2 == 1 {
			model = "modelB"
		}
		_, err := store.Insert(testEntry(model, base.Add(time.Duration(i)*time.Hour)))
		require.NoError(t, err)
	}

	entries, total, _, err := store.List(ListFilter{Model: "modelA"})
	require.NoError(t, err)
	assert.Equal(t, 5, total)
	require.Len(t, entries, 5)
	for _, e := range entries {
		assert.Equal(t, "modelA", e.Model)
	}

	// Entries 5..9 fall at/after base+5h.
	from := base.Add(5 * time.Hour)
	entries, total, _, err = store.List(ListFilter{From: from})
	require.NoError(t, err)
	assert.Equal(t, 5, total)
	require.Len(t, entries, 5)
	for _, e := range entries {
		assert.False(t, e.Timestamp.Before(from.Truncate(time.Millisecond)))
	}

	// Combined: modelB within [base+5h, base+7h] -> entries 5 and 7.
	entries, total, _, err = store.List(ListFilter{Model: "modelB", From: from, To: base.Add(7 * time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, entries, 2)
}

func TestMetricsDB_Summary(t *testing.T) {
	store := openTestStore(t)

	now := time.Now()
	for i := 1; i <= 4; i++ {
		e := testEntry("model1", now)
		e.Tokens.InputTokens = 100 * i
		e.Tokens.OutputTokens = 10 * i
		e.Tokens.CachedTokens = 5
		e.Tokens.PromptPerSecond = float64(100 * i)
		e.Tokens.TokensPerSecond = float64(10 * i)
		_, err := store.Insert(e)
		require.NoError(t, err)
	}
	// A failed request with sentinel rates: excluded from averages/histograms.
	fail := testEntry("model1", now)
	fail.RespStatusCode = 500
	fail.Tokens = TokenMetrics{CachedTokens: -1, DraftTokens: -1, DraftAccTokens: -1, PromptPerSecond: -1, TokensPerSecond: -1}
	_, err := store.Insert(fail)
	require.NoError(t, err)

	sum, err := store.Summary("", time.Time{}, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, 5, sum.Requests)
	assert.Equal(t, 1, sum.Errors)
	assert.Equal(t, int64(1000), sum.InputTokens)
	assert.Equal(t, int64(100), sum.OutputTokens)
	assert.Equal(t, int64(20), sum.CachedTokens) // -1 sentinel clamped to 0
	assert.InDelta(t, 250.0, sum.AvgPromptPerSecond, 0.001)
	assert.InDelta(t, 25.0, sum.AvgTokensPerSecond, 0.001)

	require.NotNil(t, sum.GenHistogram)
	h := sum.GenHistogram
	assert.Equal(t, 10.0, h.Min)
	assert.Equal(t, 40.0, h.Max)
	assert.InDelta(t, 25.0, h.P50, 0.001)
	assert.InDelta(t, 38.5, h.P95, 0.001) // interpolated: 30 + 0.85*10
	assert.Equal(t, 4, sumInts(h.Bins))

	// Empty range: zero counts, nil histograms.
	sum, err = store.Summary("", now.Add(time.Hour), time.Time{})
	require.NoError(t, err)
	assert.Equal(t, 0, sum.Requests)
	assert.Nil(t, sum.PromptHistogram)
	assert.Nil(t, sum.GenHistogram)
}

func TestMetricsDB_SummarySingleValue(t *testing.T) {
	store := openTestStore(t)
	_, err := store.Insert(testEntry("model1", time.Now()))
	require.NoError(t, err)

	sum, err := store.Summary("", time.Time{}, time.Time{})
	require.NoError(t, err)
	require.NotNil(t, sum.GenHistogram)
	// min == max collapses to a single bin with binSize 0.
	assert.Equal(t, []int{1}, sum.GenHistogram.Bins)
	assert.Equal(t, 0.0, sum.GenHistogram.BinSize)
	assert.Equal(t, 25.0, sum.GenHistogram.P50)
}

func TestMetricsDB_RetentionPrune(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metrics.db")
	store, err := Open(dbPath, Options{RetentionDays: 30})
	require.NoError(t, err)
	defer store.Close()

	_, err = store.Insert(testEntry("old", time.Now().AddDate(0, 0, -60)))
	require.NoError(t, err)
	_, err = store.Insert(testEntry("recent", time.Now()))
	require.NoError(t, err)

	pruned, err := store.Prune()
	require.NoError(t, err)
	assert.Equal(t, int64(1), pruned)

	entries, total, _, err := store.List(ListFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, entries, 1)
	assert.Equal(t, "recent", entries[0].Model)

	// Retention 0 keeps everything.
	store.UpdateRetention(0)
	pruned, err = store.Prune()
	require.NoError(t, err)
	assert.Zero(t, pruned)
}

func TestMetricsDB_MemoryModeRowCap(t *testing.T) {
	store, err := Open(MemoryPath, Options{MemoryRowCap: 10})
	require.NoError(t, err)
	defer store.Close()

	for i := 0; i < 25; i++ {
		_, err := store.Insert(testEntry(fmt.Sprintf("model%d", i), time.Now()))
		require.NoError(t, err)
	}

	entries, total, _, err := store.List(ListFilter{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, 10, total)
	require.Len(t, entries, 10)
	assert.Equal(t, 25, entries[0].ID)
	assert.Equal(t, 16, entries[9].ID)
}

func TestMetricsDB_FirstRunAndReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sub", "metrics.db")

	// Opening a path in a missing directory fails cleanly rather than panicking.
	_, err := Open(dbPath, Options{})
	require.Error(t, err)

	dbPath = filepath.Join(t.TempDir(), "metrics.db")
	store, err := Open(dbPath, Options{})
	require.NoError(t, err)
	id, err := store.Insert(testEntry("model1", time.Now()))
	require.NoError(t, err)
	assert.Equal(t, 1, id)
	require.NoError(t, store.Close())

	// Data survives reopen and ids keep counting up.
	store, err = Open(dbPath, Options{})
	require.NoError(t, err)
	defer store.Close()

	entries, total, _, err := store.List(ListFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, entries, 1)

	id, err = store.Insert(testEntry("model2", time.Now()))
	require.NoError(t, err)
	assert.Equal(t, 2, id)
}

func sumInts(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}
