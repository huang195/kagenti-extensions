package ledger

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// The tests in this file pin issue #1566 item 4. Query re-decoded every day file in the
// window on every call: agentop polls today each minute and 7d/month each five, so on
// the 31st the month span decoded 31 files per poll — and past days almost never change.
// The fix caches a decoded past day keyed on the file's size and mtime; the current day
// is never cached because its file is still being appended to.

// queryAt runs one Query over a window around the ledger's pinned clock.
func queryAt(t *testing.T, w *Writer) ([]Row, Caveats) {
	t.Helper()
	rows, caveats, err := w.Query(context.Background(), at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return rows, caveats
}

// TestQuery_APastDayIsServedWithoutReopeningTheFile observes the cache through
// permissions rather than timing: a past day already decoded answers from the cache,
// where validation only needs Stat, while an uncached read must Open the file. Running
// as root defeats the chmod and the test then passes vacuously — it is red only where
// Open actually fails, which is every non-root laptop and CI runner this targets.
//
// Two writers, one day apart: the first files rows under the pinned clock, the second
// — a day later — is the reader whose "today" the filed day is past. One writer cannot
// play both parts: the store decides "past" from the writer's own clock, so to the
// writer that filed them, the rows are today's and are never cached.
func TestQuery_APastDayIsServedWithoutReopeningTheFile(t *testing.T) {
	dir := t.TempDir()
	filed := newTestWriter(t, dir, func() time.Time { return at })
	filed.Record("s1", costedEvent(t, "api.example", "m", 0.01, 10, 10))
	if err := filed.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	reader := newTestWriter(t, dir, func() time.Time { return at.Add(24 * time.Hour) })

	if rows, c := queryAt(t, reader); len(rows) != 1 || c.UnreadableDays != 0 {
		t.Fatalf("first query: %d rows, UnreadableDays %d; want 1, 0", len(rows), c.UnreadableDays)
	}

	path := reader.store.path(at)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	rows, c := queryAt(t, reader)
	if len(rows) != 1 || c.UnreadableDays != 0 {
		t.Fatalf("second query: %d rows, UnreadableDays %d; want 1, 0 — a past day already "+
			"decoded must be served from the cache instead of re-opened and re-decoded on "+
			"every poll (issue #1566)", len(rows), c.UnreadableDays)
	}
}

// TestQuery_APastDayCacheIsInvalidatedWhenTheFileChanges keeps the cache honest: the
// key is the file's size and mtime, so a past day that changed is re-read, not served
// stale. A writer still pinned to the filed day appends to it — the smallest way to
// change a past day's file through the writer's own path.
func TestQuery_APastDayCacheIsInvalidatedWhenTheFileChanges(t *testing.T) {
	dir := t.TempDir()
	filed := newTestWriter(t, dir, func() time.Time { return at })
	filed.Record("s1", costedEvent(t, "api.example", "m", 0.01, 10, 10))
	if err := filed.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	reader := newTestWriter(t, dir, func() time.Time { return at.Add(24 * time.Hour) })
	if rows, _ := queryAt(t, reader); len(rows) != 1 {
		t.Fatalf("first query: %d rows, want 1", len(rows))
	}

	filed.Record("s2", costedEvent(t, "api2.example", "m", 0.02, 20, 20))
	if err := filed.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows, _ := queryAt(t, reader)
	if len(rows) != 2 {
		t.Fatalf("second query: %d rows, want 2 — the cache is keyed on size and mtime, so "+
			"a past day whose file changed must be decoded again", len(rows))
	}
}

// TestQuery_TodaysFileIsNeverCached guards the other half: the current day's file is
// still being appended to, so it must be re-read on every query. Rows recorded after a
// first query must be visible to the second.
func TestQuery_TodaysFileIsNeverCached(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, time.Now)

	nowEvent := func(host string) *pipeline.SessionEvent {
		rec, err := json.Marshal(event.Event{
			CostUSD: 0.01, Settled: true,
			Source: event.SourceUsageFallback, Provenance: "bundled",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return &pipeline.SessionEvent{
			At: time.Now(), Phase: pipeline.SessionResponse, StatusCode: 200, Host: host,
			Inference: &pipeline.InferenceExtension{
				Model: "m", InputTokens: 10, OutputTokens: 10, TotalTokens: 20,
			},
			Plugins: map[string]json.RawMessage{event.Key: rec},
		}
	}

	queryNow := func() []Row {
		t.Helper()
		now := time.Now()
		rows, _, err := w.Query(context.Background(), now.Add(-time.Minute), now.Add(time.Minute))
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		return rows
	}

	w.Record("s1", nowEvent("api.example"))
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if rows := queryNow(); len(rows) != 1 {
		t.Fatalf("first query: %d rows, want 1", len(rows))
	}

	w.Record("s2", nowEvent("api2.example"))
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if rows := queryNow(); len(rows) != 2 {
		t.Fatalf("second query: %d rows, want 2 — today's file is still being written, so "+
			"no query may serve it from a cache", len(rows))
	}
}
