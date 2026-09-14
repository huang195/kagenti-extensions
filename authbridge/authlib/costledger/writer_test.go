package costledger

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// at is a fixed clock instant used across these tests. Deliberately not UTC
// midnight-adjacent, so a timezone bug does not accidentally pass.
var at = time.Date(2026, 9, 13, 9, 14, 30, 0, time.Local)

// testZone is a fixed non-UTC zone for the tests that turn on a day boundary.
//
// time.Local is not good enough for those. Building both the input and the
// expectation in time.Local makes the assertion vacuous wherever time.Local IS UTC —
// the default in most CI containers — because an implementation that read time.UTC
// would agree with the test by construction. At UTC-7, local midnight is 07:00 UTC,
// so the two readings land on different days and only the right one passes.
var testZone = time.FixedZone("test", -7*3600)

// newTestWriter opens a ledger over dir with a pinned clock, closed on cleanup.
func newTestWriter(t *testing.T, dir string, clock func() time.Time) *Writer {
	t.Helper()
	w, err := New(dir, WithClock(clock))
	if err != nil {
		t.Fatalf("New(%s): %v", dir, err)
	}
	// Error deliberately discarded: several of these tests point the writer at an
	// unwritable directory on purpose, and a cleanup that failed the test would
	// turn the assertion "IO failure does not propagate" into a failure.
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// costedEvent builds a response event carrying a settled cost record, the way
// inference-parser publishes it.
func costedEvent(t *testing.T, host, model string, costUSD float64, in, out int) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(costevent.Event{
		CostUSD: costUSD, Settled: true,
		Source: costevent.SourceUsageFallback, Provenance: "bundled",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: host,
		Inference: &pipeline.InferenceExtension{
			Model: model, InputTokens: in, OutputTokens: out,
			TotalTokens: in + out, PresentKinds: 0b1001,
		},
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	}
}

// setProvenance rewrites the provenance on an event's cost record.
func setProvenance(t *testing.T, e *pipeline.SessionEvent, prov string) {
	t.Helper()
	ev, ok := costevent.Record(e)
	if !ok {
		t.Fatal("event carries no cost record")
	}
	ev.Provenance = prov
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e.Plugins[costevent.Key] = raw
}

// markIncomplete flags an event's cost record as an inexact figure.
func markIncomplete(t *testing.T, e *pipeline.SessionEvent, reason string) {
	t.Helper()
	ev, ok := costevent.Record(e)
	if !ok {
		t.Fatal("event carries no cost record")
	}
	ev.Incomplete, ev.IncompleteReason = true, reason
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e.Plugins[costevent.Key] = raw
}

// readAllBytes concatenates every day file in dir.
func readAllBytes(t *testing.T, dir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var out []byte
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		out = append(out, b...)
	}
	return out
}

// readAllRows decodes every row in every day file, in file order.
func readAllRows(t *testing.T, dir string) []Row {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(readAllBytes(t, dir)))
	var out []Row
	for {
		var r Row
		if err := dec.Decode(&r); err != nil {
			return out
		}
		out = append(out, r)
	}
}

func bytesContains(haystack []byte, needle string) bool {
	return bytes.Contains(haystack, []byte(needle))
}

func TestWriter_AccumulatesTheOpenMinuteWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	// Wait for the writer goroutine, so "no files" is a fact about behaviour rather
	// than about having asked before it got there.
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The open minute stays in the writer's own accumulator. Writing it would mean the
	// same minute existed in two places, and a reader composing them would
	// double-count.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files while the minute was still open; want 0", len(entries))
	}
}

func TestWriter_FlushesOnMinuteRoll(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Advance past the minute boundary and record again: the previous minute closes.
	now = at.Add(time.Minute)
	later := costedEvent(t, "gw", "m", 0.10, 10, 5)
	later.At = now
	w.Record("s1", later)
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (the one closed minute)", len(rows))
	}
	if rows[0].Requests != 2 {
		t.Errorf("Requests = %d, want 2 accumulated", rows[0].Requests)
	}
	if rows[0].CostMicros != 500_000 {
		t.Errorf("CostMicros = %d, want 500000", rows[0].CostMicros)
	}
	if rows[0].InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200", rows[0].InputTokens)
	}
	// The closed minute keeps its OWN timestamp, not the one that closed it.
	if want := at.Truncate(time.Minute); !rows[0].At.Equal(want) {
		t.Errorf("At = %v, want the closed minute %v", rows[0].At, want)
	}
}

func TestWriter_SeparateRowPerCompositeKey(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw-a", "opus", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw-b", "opus", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw-a", "haiku", 0.05, 10, 5))

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 distinct (endpoint, model) keys", len(rows))
	}
}

func TestWriter_ProvenanceIsPartOfTheKey(t *testing.T) {
	// A single minute can mix a gateway's own figures with modelled ones, and one
	// provenance per row would have to pick a winner. Keying on it keeps PricedBy
	// reconstructible from the ledger exactly as /v1/usage reports it.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	a := costedEvent(t, "gw", "m", 0.25, 100, 50)
	b := costedEvent(t, "gw", "m", 0.25, 100, 50)
	setProvenance(t, b, "authoritative")
	w.Record("s1", a)
	w.Record("s1", b)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — one per provenance", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Provenance] = true
	}
	if !seen["bundled"] || !seen["authoritative"] {
		t.Errorf("provenances = %v, want both bundled and authoritative", seen)
	}
}

func TestWriter_UnpricedTrafficIsRecordedWithoutCost(t *testing.T) {
	// An unpriced request still happened. It contributes no dollars and shows up as
	// the priced/priceable gap — dropping it would make the gap invisible and the
	// total look complete.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	delete(e.Plugins, costevent.Key) // no settled cost
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", rows[0].CostMicros)
	}
	if rows[0].PricedRequests != 0 {
		t.Errorf("PricedRequests = %d, want 0", rows[0].PricedRequests)
	}
	if rows[0].PriceableRequests != 1 {
		t.Errorf("PriceableRequests = %d, want 1 — it carried a model and tokens", rows[0].PriceableRequests)
	}
}

// The caveat a persisted total cannot afford to lose: a truncated stream's figure
// is a FLOOR, and once the process restarts this counter is the only thing left
// saying so. Without it the dollars on disk quietly gain a precision they never had.
func TestWriter_IncompleteFigureIsRecordedAsInexact(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 0)
	markIncomplete(t, e, "output-uncounted")
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].IncompleteRequests != 1 {
		t.Errorf("IncompleteRequests = %d, want 1", rows[0].IncompleteRequests)
	}
	// Disclosed, not deducted. The dollars and the priced count both stand; only the
	// claim of exactness is withdrawn. See usage.Counts.IncompleteRequests.
	if rows[0].CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want the figure kept at 250000", rows[0].CostMicros)
	}
	if rows[0].PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1 — incomplete is a subset, not a deduction", rows[0].PricedRequests)
	}
	// And it must round-trip: it is carried by the embedded usage.Counts, so a
	// consumer reading the file back sees the caveat too.
	if !bytesContains(readAllBytes(t, dir), "incompleteRequests") {
		t.Error("the serialized row does not carry incompleteRequests")
	}
}

// An exact figure must NOT carry the caveat: a permanent warning with nothing to
// act on is what teaches an operator to ignore the one signal that matters.
func TestWriter_ExactFigureCarriesNoIncompleteCount(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].IncompleteRequests != 0 {
		t.Errorf("IncompleteRequests = %d, want 0", rows[0].IncompleteRequests)
	}
	if bytesContains(readAllBytes(t, dir), "incompleteRequests") {
		t.Error("an exact row still serializes incompleteRequests")
	}
}

func TestWriter_NonInferenceTrafficIsIgnored(t *testing.T) {
	// MCP calls, health checks, tunnel opens. Recording them would put every
	// proxied response in the ledger and in the cost denominator.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", &pipeline.SessionEvent{At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw"})

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if rows := readAllRows(t, dir); len(rows) != 0 {
		t.Errorf("got %d rows for non-inference traffic, want 0", len(rows))
	}
}

// A request event has no token counts and no cost. Folding it would double the
// request count for every turn, halving every coverage ratio the ledger reports.
func TestWriter_RequestPhaseIsIgnored(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Phase = pipeline.SessionRequest
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if rows := readAllRows(t, dir); len(rows) != 0 {
		t.Errorf("got %d rows for a request event, want 0", len(rows))
	}
}

func TestWriter_HoldsNoPromptContent(t *testing.T) {
	// A user-facing promise in the docs. Assert on the serialized bytes, because
	// that is what lands on disk.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Inference.Messages = []pipeline.InferenceMessage{{Role: "user", Content: "SECRET-PROMPT-TEXT"}}
	e.Inference.Completion = "SECRET-COMPLETION-TEXT"
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	raw := readAllBytes(t, dir)
	for _, secret := range []string{"SECRET-PROMPT-TEXT", "SECRET-COMPLETION-TEXT", "Messages", "messages", "completion"} {
		if bytesContains(raw, secret) {
			t.Errorf("ledger bytes contain %q", secret)
		}
	}
}

func TestWriter_IOFailureDoesNotPropagate(t *testing.T) {
	// The ledger is observability. A full disk or a read-only home must never turn
	// into a failed request.
	dir := filepath.Join(t.TempDir(), "unwritable")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	// Must not panic and must not block. Flush may return an error; Record never
	// surfaces one.
	_ = w.Flush()
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
}

// A day file records spend, so it must not be readable by other accounts on a
// shared machine. 0o644 is the default that would be wrong here.
func TestWriter_DayFileIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("readdir: %d entries, err %v", len(entries), err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s mode = %v, want no group or other access", entries[0].Name(), perm)
	}
}

// A flush that straddles local midnight must split across two day files. A cached
// handle would append tomorrow's minute to yesterday's file, silently giving that
// day 24 extra hours.
//
// In testZone rather than time.Local, for the reason recorded there: at UTC-7 these
// two minutes are in one UTC day and two local days, so a UTC filename would put both
// rows in one file and the assertion below would fail. Built in time.Local it passed
// on any UTC host whichever zone the implementation used.
func TestWriter_FlushStraddlingMidnightSplitsByDay(t *testing.T) {
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, testZone)
	before := midnight.Add(-time.Minute)
	now := before
	w := newTestWriter(t, dir, func() time.Time { return now })

	early := costedEvent(t, "gw", "m", 0.25, 100, 50)
	early.At = before
	w.Record("s1", early)
	// A late arrival for a closed minute appends immediately, so both days are
	// written by one writer without needing a roll.
	late := costedEvent(t, "gw", "m", 0.25, 100, 50)
	late.At = midnight
	now = midnight
	w.Record("s1", late)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	for _, want := range []string{"2026-09-13.jsonl", "2026-09-14.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
}

// The property finding 3 is about: a filesystem that has stopped responding must not
// reach the request path. Record is called under session.Store's write lock, so a
// blocking hand-off would stall every other request in the proxy.
//
// Closing the writer first leaves nothing draining the queue, which is the only way a
// unit test can stand in for a hung mount. A blocking implementation would hang here
// rather than fail.
func TestRecord_DropsRatherThanBlocksWhenTheWriterCannotKeepUp(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Each new minute closes the previous one, so this is one queued batch per event.
	for i := 0; i < opsBuffer+50; i++ {
		now = at.Add(time.Duration(i) * time.Minute)
		e := costedEvent(t, "gw", "m", 0.25, 100, 50)
		e.At = now
		w.Record("s1", e)
	}

	if w.Dropped() == 0 {
		t.Error("Dropped() = 0 after overrunning the queue; a drop that is not counted " +
			"is a cost total that is short by an unknown amount")
	}
}

// The settle path: a minute that has ENDED is written even though no new event has
// arrived to roll it. Without this an idle proxy held its last minute in memory
// indefinitely, so a kill lost it and every reader had to reach into memory for it.
func TestSettleClosedMinute_WritesAnEndedMinuteWithNoNewTraffic(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Time moves on; no further traffic.
	now = at.Add(90 * time.Second)
	w.settleClosedMinute()

	rows := readAllRows(t, dir)
	if len(rows) != 1 || rows[0].CostMicros != 250_000 {
		t.Fatalf("got %+v, want the ended minute written", rows)
	}
	if _, open := w.pending(); !open.IsZero() {
		t.Error("the minute is still held after being settled; it would be counted twice")
	}
}

// And it must NOT write the minute that is still accumulating — the one state that
// would put a minute on disk and in memory at once.
func TestSettleClosedMinute_LeavesTheCurrentMinuteAlone(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	now = at.Add(20 * time.Second) // same minute
	w.settleClosedMinute()

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files for a minute that has not ended yet", len(entries))
	}
}

// Retention still runs on a day roll — it just runs on the writer goroutine now
// rather than inside a request. The prune request rides the same queue as the rows,
// so a sync is what proves it arrived.
func TestPrune_OnADayRollRunsThroughTheWriter(t *testing.T) {
	dir := t.TempDir()
	now := at
	w, err := New(dir, WithClock(func() time.Time { return now }), WithRetentionDays(3))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	old := w.store.path(at.AddDate(0, 0, -10))
	if werr := os.WriteFile(old, []byte("{}\n"), 0o600); werr != nil {
		t.Fatalf("seed: %v", werr)
	}
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Cross local midnight, which is what arms the prune.
	now = dayOf(at).AddDate(0, 0, 1).Add(9 * time.Hour)
	next := costedEvent(t, "gw", "m", 0.25, 100, 50)
	next.At = now
	w.Record("s1", next)
	if serr := w.sync(); serr != nil {
		t.Fatalf("sync: %v", serr)
	}

	if _, serr := os.Stat(old); !os.IsNotExist(serr) {
		t.Errorf("the 10-day-old file survived a day roll under a 3-day retention: %v", serr)
	}
}

// Close is called from a shutdown path that may already have failed once, so a second
// call must not panic on a closed channel or block on a goroutine that has gone.
func TestClose_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if rows := readAllRows(t, dir); len(rows) != 1 {
		t.Errorf("got %d rows, want the open minute flushed by Close", len(rows))
	}
}

// Flush after Close still writes: main flushes the ledger during shutdown, and a
// Writer whose goroutine has gone must do the work inline rather than queue it for
// nobody.
func TestFlush_AfterCloseStillWrites(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush after Close: %v", err)
	}

	if rows := readAllRows(t, dir); len(rows) != 1 {
		t.Errorf("got %d rows, want 1 written inline after Close", len(rows))
	}
}

// shortWriter accepts limit bytes and then fails, the way a file on a filesystem
// that has just run out of space does.
type shortWriter struct {
	limit     int
	written   []byte
	truncated []int64
}

func (s *shortWriter) Write(b []byte) (int, error) {
	n := len(b)
	if n > s.limit {
		n = s.limit
	}
	s.written = append(s.written, b[:n]...)
	return n, io.ErrShortWrite
}

func (s *shortWriter) Truncate(size int64) error {
	s.truncated = append(s.truncated, size)
	return nil
}

// The reachability argument for the whole corrupt-line problem: a short write leaves
// a fragment with no trailing newline, and the NEXT append concatenates onto it,
// guaranteeing a syntax error mid-file. Rolling back to where the file started costs
// this one minute and leaves the day readable.
func TestAppendBytes_ShortWriteIsRolledBack(t *testing.T) {
	f := &shortWriter{limit: 7}

	err := appendBytes(f, 4096, []byte(`{"at":"2026-09-13T09:14:00Z"}`+"\n"))

	if err == nil {
		t.Fatal("appendBytes swallowed a short write; the caller has to be able to log it")
	}
	if len(f.truncated) != 1 || f.truncated[0] != 4096 {
		t.Errorf("truncated = %v, want one rollback to the pre-write size 4096", f.truncated)
	}
}

// Nothing was written, so nothing needs undoing — and truncating anyway would be a
// pointless write against a file we have just been told we cannot write to.
func TestAppendBytes_FailureBeforeAnyByteDoesNotTruncate(t *testing.T) {
	f := &shortWriter{limit: 0}

	if err := appendBytes(f, 4096, []byte("{}\n")); err == nil {
		t.Fatal("appendBytes reported success for a write that wrote nothing")
	}
	if len(f.truncated) != 0 {
		t.Errorf("truncated = %v, want no rollback when no bytes landed", f.truncated)
	}
}

// Every line a successful write produces has to be independently decodable, because
// that is the property readDay's per-line resync depends on. A row written without a
// terminating newline would make the NEXT row unreadable.
func TestWriteLines_EveryLineEndsWithANewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "day.jsonl")
	rows := []Row{
		{At: at, Endpoint: "gw", Model: "opus"},
		{At: at, Endpoint: "gw", Model: "haiku"},
	}
	if err := writeLines(path, rows); err != nil {
		t.Fatalf("writeLines: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Error("the file does not end with a newline; the next append would concatenate")
	}
	for i, l := range bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n")) {
		var r Row
		if err := json.Unmarshal(l, &r); err != nil {
			t.Errorf("line %d is not independently decodable: %v", i, err)
		}
	}
}

// assertOwnership checks the rule Window's arithmetic rests on: while a minute is
// held in memory, nothing on disk carries that minute or a later one.
func assertOwnership(t *testing.T, w *Writer, dir, step string) {
	t.Helper()
	// Settle the writer first: the rule is about what is ON DISK versus what is held,
	// and a batch still in flight would make the disk side look emptier than it is.
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	_, open := w.pending()
	if open.IsZero() {
		return
	}
	for _, r := range readAllRows(t, dir) {
		if !r.At.Truncate(time.Minute).Before(open) {
			t.Errorf("after %s: disk row at %v is not below the held minute %v — "+
				"Window would count it twice", step, r.At, open)
		}
	}
}

// Every path that writes a row has to keep the ownership rule, so this drives all
// three of them in one sequence rather than trusting the one that is obvious.
func TestPendingMinute_IsNeverAlsoOnDisk(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	// 1. The ordinary path: accumulate, then roll so the minute is flushed.
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	assertOwnership(t, w, dir, "the first open minute")
	now = at.Add(time.Minute)
	second := costedEvent(t, "gw", "m", 0.25, 100, 50)
	second.At = now
	w.Record("s1", second)
	assertOwnership(t, w, dir, "a minute roll")

	// 2. A late event for a minute below the held one goes straight to disk.
	late := costedEvent(t, "gw", "m", 0.25, 100, 50)
	late.At = at.Add(-5 * time.Minute)
	w.Record("s1", late)
	assertOwnership(t, w, dir, "a late event")

	// 3. An event arriving in the SAME minute after a Flush must not be re-held —
	// the case a periodic settle or a shutdown flush creates.
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	sameMinute := costedEvent(t, "gw", "m", 0.25, 100, 50)
	sameMinute.At = now
	w.Record("s1", sameMinute)
	assertOwnership(t, w, dir, "an event in an already-flushed minute")

	// And that last event must still be recorded somewhere — the guard sends it to
	// disk rather than dropping it.
	var total int64
	for _, r := range readAllRows(t, dir) {
		total += r.CostMicros
	}
	pending, _ := w.pending()
	for _, r := range pending {
		total += r.CostMicros
	}
	if want := int64(4 * 250_000); total != want {
		t.Errorf("total across disk and memory = %d, want %d — every event exactly once", total, want)
	}
}

// Retention deletes day files past the window and leaves everything else alone,
// including names it cannot date — deleting an unrecognised file under an
// operator-configured path is the one unrecoverable mistake available here.
func TestWriter_PruneDropsOldDaysAndSparesUnknownNames(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir, 3)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	old := at.AddDate(0, 0, -10)
	keep := at.AddDate(0, 0, -1)
	for _, d := range []time.Time{old, keep} {
		if werr := os.WriteFile(s.path(d), []byte("{}\n"), 0o600); werr != nil {
			t.Fatalf("seed: %v", werr)
		}
	}
	stranger := filepath.Join(dir, "notes.txt")
	if werr := os.WriteFile(stranger, []byte("hi"), 0o600); werr != nil {
		t.Fatalf("seed: %v", werr)
	}

	if perr := s.prune(at); perr != nil {
		t.Fatalf("prune: %v", perr)
	}

	if _, serr := os.Stat(s.path(old)); !os.IsNotExist(serr) {
		t.Errorf("the 10-day-old file survived a 3-day retention: %v", serr)
	}
	if _, serr := os.Stat(s.path(keep)); serr != nil {
		t.Errorf("yesterday's file was deleted: %v", serr)
	}
	if _, serr := os.Stat(stranger); serr != nil {
		t.Errorf("an undatable file was deleted: %v", serr)
	}
}
