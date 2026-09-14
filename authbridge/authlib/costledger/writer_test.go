package costledger

import (
	"bytes"
	"encoding/json"
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

	// The open minute belongs to the in-memory ring, not to the ledger. Writing it
	// would mean the same minute existed in two places and a reader stitching them
	// would double-count.
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
func TestWriter_FlushStraddlingMidnightSplitsByDay(t *testing.T) {
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local)
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
