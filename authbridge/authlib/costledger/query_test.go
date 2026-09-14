package costledger

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// writeDay writes lines straight to a day file, bypassing Writer, so a query test
// controls exactly what is on disk without driving the accumulation path.
func writeDay(t *testing.T, dir string, day time.Time, lines ...string) {
	t.Helper()
	name := filepath.Join(dir, day.Format(dayLayout)+".jsonl")
	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(name, buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// line renders one ledger row as JSON, so a test can state exactly what is on disk.
func line(min time.Time, endpoint, model string, requests, in, out, micros int64) string {
	return fmt.Sprintf(
		`{"at":%q,"endpoint":%q,"model":%q,"requests":%d,"inputTokens":%d,`+
			`"outputTokens":%d,"costMicros":%d,"pricedRequests":%d,"priceableRequests":%d}`,
		min.Format(time.RFC3339Nano), endpoint, model, requests, in, out, micros, requests, requests)
}

func TestQuery_SpanInsideOneDay(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		line(base.Add(time.Minute), "gw", "m", 1, 20, 5, 200),
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300),
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Inclusive of both endpoints at minute granularity, so the third row is out.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
}

func TestQuery_SpanCrossingLocalMidnight(t *testing.T) {
	// The case a UTC-vs-local bug shows up in, and the reason "today" is local
	// midnight: a laptop crossing a timezone must not have its day reset
	// mid-afternoon.
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local)
	before := midnight.Add(-30 * time.Minute) // 23:30 on the 13th
	after := midnight.Add(30 * time.Minute)   // 00:30 on the 14th
	writeDay(t, dir, before, line(before, "gw", "m", 1, 10, 5, 100))
	writeDay(t, dir, after, line(after, "gw", "m", 1, 20, 5, 200))
	w := newTestWriter(t, dir, func() time.Time { return after })

	got, err := w.Query(before, after)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 across two day files: %+v", len(got), got)
	}
}

func TestQuery_MissingDayFileIsNotAnError(t *testing.T) {
	// An idle day writes no file. That is the normal case on a laptop, not a
	// fault — erroring would make "this week" fail for anyone who took a day off.
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base.AddDate(0, 0, -3), base)
	if err != nil {
		t.Fatalf("Query over an empty range: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows from an empty ledger, want 0", len(got))
	}
}

func TestQuery_TruncatedFinalLineIsSkippedAndTheRestSurvives(t *testing.T) {
	// A truncated tail is the expected outcome of a crash mid-append. Losing the
	// whole day because its last line is half-written would turn a 60-second gap
	// into a 24-hour one.
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		line(base.Add(time.Minute), "gw", "m", 1, 20, 5, 200),
		`{"at":"2026-09-13T09:02:00`, // truncated mid-write
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the 2 intact ones: %+v", len(got), got)
	}
}

// The open minute is the RING's, never the ledger's — so a query must not see it.
// If both owned a minute, a reader stitching the two would double-count it.
func TestQuery_DoesNotSeeTheOpenMinute(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	got, err := w.Query(at.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows while the minute was open, want 0: %+v", len(got), got)
	}
}

// A reversed range is a caller mistake, not a reason to return nothing: swapping
// answers the question that was meant instead of an empty result a client would
// render as "no spend".
func TestQuery_ReversedRangeIsNormalised(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base, line(base, "gw", "m", 1, 10, 5, 100))
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base.Add(time.Hour), base)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d rows for a reversed range, want 1", len(got))
	}
}

func TestFold_ByModelSumsToTotals(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Endpoint: "gw", Model: "opus", Counts: usage.Counts{
			Requests: 2, InputTokens: 100, CostMicros: 300, PricedRequests: 2, PriceableRequests: 2}},
		{At: base, Endpoint: "gw", Model: "haiku", Counts: usage.Counts{
			Requests: 1, InputTokens: 20, CostMicros: 50, PricedRequests: 1, PriceableRequests: 1}},
	}

	totals, series := Fold(rows, usage.GroupModel)

	if totals.CostMicros != 350 {
		t.Errorf("totals.CostMicros = %d, want 350", totals.CostMicros)
	}
	if series["opus"].CostMicros != 300 || series["haiku"].CostMicros != 50 {
		t.Errorf("series = %+v, want opus 300 and haiku 50", series)
	}
	// The property that makes a breakdown table trustworthy: its rows account for
	// the total it sits under.
	var sum int64
	for _, c := range series {
		sum += c.CostMicros
	}
	if sum != totals.CostMicros {
		t.Errorf("series sums to %d but totals is %d", sum, totals.CostMicros)
	}
}

func TestFold_ByEndpoint(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Endpoint: "gw-a", Model: "m", Counts: usage.Counts{Requests: 1, CostMicros: 10}},
		{At: base, Endpoint: "gw-b", Model: "m", Counts: usage.Counts{Requests: 1, CostMicros: 20}},
	}

	_, series := Fold(rows, usage.GroupEndpoint)

	if series["gw-a"].CostMicros != 10 || series["gw-b"].CostMicros != 20 {
		t.Errorf("series = %+v, want gw-a 10 and gw-b 20", series)
	}
}

func TestFold_MethodIsAnAliasForModel(t *testing.T) {
	// Same equivalence the live path guarantees: two spellings, one series.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "opus", Counts: usage.Counts{Requests: 1, CostMicros: 10}}}

	_, byModel := Fold(rows, usage.GroupModel)
	_, byMethod := Fold(rows, usage.GroupMethod)

	if len(byModel) != len(byMethod) || byModel["opus"] != byMethod["opus"] {
		t.Errorf("model series %+v and method series %+v differ", byModel, byMethod)
	}
}

func TestFold_UnpricedRowsCountButCostNothing(t *testing.T) {
	// The coverage gap must survive the round trip to disk, or a partial total
	// reads as a complete one.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "m", Counts: usage.Counts{Requests: 1, PriceableRequests: 1}}}

	totals, _ := Fold(rows, usage.GroupModel)

	if totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", totals.CostMicros)
	}
	if totals.PricedRequests != 0 || totals.PriceableRequests != 1 {
		t.Errorf("coverage = %d/%d, want 0/1", totals.PricedRequests, totals.PriceableRequests)
	}
}

// The inexactness caveat must survive the fold as well as the round trip: it is
// carried by the embedded usage.Counts, so Counts.Add is what makes it work, and a
// fold that dropped it would report a floor as an exact figure.
func TestFold_CarriesTheIncompleteCount(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Model: "m", Counts: usage.Counts{
			Requests: 1, CostMicros: 100, PricedRequests: 1, PriceableRequests: 1, IncompleteRequests: 1}},
		{At: base, Model: "m", Counts: usage.Counts{
			Requests: 1, CostMicros: 200, PricedRequests: 1, PriceableRequests: 1}},
	}

	totals, series := Fold(rows, usage.GroupModel)

	if totals.IncompleteRequests != 1 {
		t.Errorf("totals.IncompleteRequests = %d, want 1", totals.IncompleteRequests)
	}
	if series["m"].IncompleteRequests != 1 {
		t.Errorf("series IncompleteRequests = %d, want 1", series["m"].IncompleteRequests)
	}
	// A subset, not a deduction: both requests stay priced and both figures stay in.
	if totals.PricedRequests != 2 || totals.CostMicros != 300 {
		t.Errorf("priced = %d, cost = %d; want 2 and 300", totals.PricedRequests, totals.CostMicros)
	}
}

func TestFold_EmptyLabelIsNeverASeriesKey(t *testing.T) {
	// A blank row in a breakdown table reads as a bug rather than as missing
	// attribution — the same guard the live foldInto applies.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "", Counts: usage.Counts{Requests: 1, CostMicros: 10}}}

	totals, series := Fold(rows, usage.GroupModel)

	if _, ok := series[""]; ok {
		t.Error(`series has an "" key`)
	}
	if totals.CostMicros != 10 {
		t.Errorf("CostMicros = %d, want the row still counted in totals", totals.CostMicros)
	}
}

func TestFold_GroupNoneReturnsNoSeries(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	totals, series := Fold([]Row{{At: base, Counts: usage.Counts{Requests: 1, CostMicros: 10}}}, usage.GroupNone)
	if totals.CostMicros != 10 {
		t.Errorf("CostMicros = %d, want 10", totals.CostMicros)
	}
	if series != nil {
		t.Errorf("series = %+v, want nil for GroupNone", series)
	}
}

// A ledger row carries no session, status or plugin, so those groupings produce no
// series rather than a misleading one. The totals still stand.
func TestFold_GroupingsTheLedgerCannotAnswerReturnNoSeries(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "m", Endpoint: "gw", Counts: usage.Counts{Requests: 1, CostMicros: 10}}}
	for _, g := range []usage.Group{usage.GroupSession, usage.GroupStatus, usage.GroupPlugin} {
		totals, series := Fold(rows, g)
		if series != nil {
			t.Errorf("group=%s produced a series %+v; the ledger holds no such column", g, series)
		}
		if totals.CostMicros != 10 {
			t.Errorf("group=%s totals.CostMicros = %d, want 10", g, totals.CostMicros)
		}
	}
}
