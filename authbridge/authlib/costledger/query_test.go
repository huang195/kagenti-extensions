package costledger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
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
	//
	// A FIXED NON-UTC ZONE, not time.Local. Building both the input and the
	// expectation in time.Local made this vacuous wherever time.Local is UTC — the
	// default in most CI containers — because a dayOf reading UTC would then agree with
	// the test by construction. At UTC-7 the two rows below straddle local midnight but
	// fall in the SAME UTC day, so a UTC day walk visits one date and misses a file.
	// Verified by mutation: with dayOf on a UTC day, the old test passed under TZ=UTC
	// and failed under TZ=America/New_York; this one fails under both.
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, testZone)
	before := midnight.Add(-30 * time.Minute) // 23:30 on the 13th, local
	after := midnight.Add(30 * time.Minute)   // 00:30 on the 14th, local
	writeDay(t, dir, before, line(before, "gw", "m", 1, 10, 5, 100))
	writeDay(t, dir, after, line(after, "gw", "m", 1, 20, 5, 200))
	w := newTestWriter(t, dir, func() time.Time { return after })

	// Two files, named for the two LOCAL days. Asserted rather than assumed, because
	// this is the property a UTC boundary breaks.
	for _, want := range []string{"2026-09-13.jsonl", "2026-09-14.jsonl"} {
		if _, serr := os.Stat(filepath.Join(dir, want)); serr != nil {
			t.Fatalf("missing %s: %v", want, serr)
		}
	}

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

// The case a single json.Decoder over the whole file CANNOT survive: a bad line in
// the middle. A Decoder has no way to resync, so it stopped there and silently
// dropped every later row for that day — permanently, and the shortened figure was
// still labelled "today". Only the final-line variant above passed under that
// behaviour, which is why this test exists.
func TestQuery_CorruptLineMidFileSkipsOnlyThatLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		`{"at":"2026-09-13T09:01:00Z","endpoint":"gw"`, // no closing brace: the damage
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300),
		line(base.Add(3*time.Minute), "gw", "m", 1, 40, 5, 400),
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want the 3 intact ones — a mid-file corruption must not "+
			"truncate the rest of the day: %+v", len(got), got)
	}
	var total int64
	for _, r := range got {
		total += r.CostMicros
	}
	if total != 800 {
		t.Errorf("CostMicros total = %d, want 800 (100 + 300 + 400)", total)
	}
}

// The exact byte pattern the old write path produced: a fragment with no trailing
// newline, then a later append concatenated onto it. One line is unreadable and
// everything after it survives.
func TestQuery_FragmentConcatenatedWithTheNextAppendCostsOneLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	name := filepath.Join(dir, base.Format(dayLayout)+".jsonl")
	body := line(base, "gw", "m", 1, 10, 5, 100) + "\n" +
		`{"at":"2026-09-13T09:01:00Z","endpo` + // short write, no newline
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300) + "\n" +
		line(base.Add(3*time.Minute), "gw", "m", 1, 40, 5, 400) + "\n"
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// The fragment swallows the row it was concatenated with — one line lost, not the
	// day. The 09:03 row is the one that proves the read did not stop.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (the intact first and last): %+v", len(got), got)
	}
	if got[len(got)-1].CostMicros != 400 {
		t.Errorf("last row = %d micros, want the 400 that follows the damage", got[len(got)-1].CostMicros)
	}
}

// A line past maxLineBytes cannot be skipped — a scanner will not buffer it, so it
// cannot step over it. Documented consequence: that day's read ends there. Asserted
// so the behaviour is a decision rather than a surprise, and so the rows BEFORE it
// are known to survive.
func TestQuery_LineBeyondTheBufferLimitEndsThatDay(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		`{"at":"`+strings.Repeat("x", maxLineBytes+1)+`"}`,
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300),
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query must not fail the whole request over one day file: %v", err)
	}
	if len(got) != 1 || got[0].CostMicros != 100 {
		t.Errorf("got %+v, want the one row that preceded the oversized line", got)
	}
}

// Only CLOSED minutes reach disk, so the disk half of the ledger must not see the
// open one. Window is what adds it back; if both halves owned a minute, a reader
// composing them would double-count it.
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

// The defect this whole seam exists to close: a turn whose spend all happened
// inside the current minute has NOTHING on disk, and a reader that saw only the day
// files would answer "no spend today" over real money.
func TestWindow_IncludesTheOpenMinuteWithNothingOnDisk(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	if disk, err := w.Query(at.Add(-time.Hour), at); err != nil || len(disk) != 0 {
		t.Fatalf("disk half = %d rows (err %v), want 0 — the premise of this test", len(disk), err)
	}

	rows, err := w.Window(at.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	totals, _ := Fold(rows, usage.GroupNone)
	if totals.CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want 250000 from the open minute", totals.CostMicros)
	}
	if totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1 — priced:false here reads as $0.00 for a day with spend",
			totals.PricedRequests)
	}
}

// THE non-overlap proof. The same two events are counted once while the minute is
// open and once after it has been written, and the total must not move: if either
// half leaked the other's minute, this figure would double.
func TestWindow_CountsAMinuteExactlyOnceAcrossTheFlush(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	from, to := at.Add(-time.Hour), at.Add(time.Hour)
	before, err := w.Window(from, to)
	if err != nil {
		t.Fatalf("Window while open: %v", err)
	}
	openTotals, _ := Fold(before, usage.GroupNone)

	// Roll the minute: the same spend moves from memory to disk.
	now = at.Add(time.Minute)
	later := costedEvent(t, "gw", "m", 0.10, 10, 5)
	later.At = now
	w.Record("s1", later)
	// The closed minute reaches disk on the writer goroutine, so wait for it: the
	// question here is whether BOTH halves claim it, which needs it to be in one.
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	after, err := w.Window(from, to)
	if err != nil {
		t.Fatalf("Window after the roll: %v", err)
	}
	closedTotals, _ := Fold(after, usage.GroupNone)

	// The first minute's 500000 is now on disk and the second minute's 100000 is held.
	if openTotals.CostMicros != 500_000 {
		t.Errorf("open-minute total = %d, want 500000", openTotals.CostMicros)
	}
	if closedTotals.CostMicros != 600_000 {
		t.Errorf("total after the roll = %d, want 600000 (500000 on disk + 100000 held); "+
			"1100000 would mean the first minute was counted in both halves", closedTotals.CostMicros)
	}
	if closedTotals.Requests != 3 {
		t.Errorf("Requests = %d, want 3", closedTotals.Requests)
	}
}

// Step 2 of Window's non-overlap rule, tested against a state the writer's own
// invariant forbids: a disk row for the minute currently held. Only a flush racing
// between Window's two reads can produce it, and when it does those rows are the
// ones already in hand — so they must be dropped, not added.
func TestWindow_DropsADiskRowForTheHeldMinute(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Hand-seed exactly what a racing flush of the held minute would leave behind.
	minute := at.Truncate(time.Minute)
	writeDay(t, dir, minute, line(minute, "gw", "m", 1, 100, 50, 250_000))

	rows, err := w.Window(at.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	totals, _ := Fold(rows, usage.GroupNone)
	if totals.CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want 250000 counted once, not 500000", totals.CostMicros)
	}
}

// The held minute is not always in the window asked for. An idle proxy at 00:05
// still holds yesterday's last minute, and that spend is yesterday's.
func TestWindow_ExcludesAHeldMinuteOutsideTheRange(t *testing.T) {
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local)
	yesterday := midnight.Add(-30 * time.Second) // 23:59:30
	now := yesterday
	w := newTestWriter(t, dir, func() time.Time { return now })
	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.At = yesterday
	w.Record("s1", e)

	// "today" as ParseWindowSpec builds it: local midnight to now.
	now = midnight.Add(5 * time.Minute)
	rows, err := w.Window(midnight, now)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows; yesterday's held minute must not land in today: %+v", len(rows), rows)
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

// TestFold_GroupAgentBreaksDownByAgent covers labelFor's usage.GroupAgent arm, which
// the ledger shipped without because the constant did not exist yet.
func TestFold_GroupAgentBreaksDownByAgent(t *testing.T) {
	rows := []Row{
		{Endpoint: "gw", Model: "m", Agent: "claude-code/2.1.14",
			Counts: usage.Counts{Requests: 1, CostMicros: 100, InputTokens: 10}},
		{Endpoint: "gw", Model: "m", Agent: "opencode/0.4.2",
			Counts: usage.Counts{Requests: 1, CostMicros: 200, InputTokens: 20}},
	}

	totals, series := Fold(rows, usage.GroupAgent)

	if totals.CostMicros != 300 {
		t.Errorf("totals.CostMicros = %d, want 300", totals.CostMicros)
	}
	if got := series["claude-code/2.1.14"].CostMicros; got != 100 {
		t.Errorf("claude-code CostMicros = %d, want 100", got)
	}
	if got := series["opencode/0.4.2"].CostMicros; got != 200 {
		t.Errorf("opencode CostMicros = %d, want 200", got)
	}
}

// TestFold_GroupAgentMapsAbsenceToUnknown is the consistency guarantee between the
// ledger and the live aggregator.
//
// The two differ in REPRESENTATION on purpose — the ledger stores "" so the durable
// file stays lossless, the aggregator's series keys are display strings — but they
// must not differ in what a client SEES for the same traffic. Mapping "" to the same
// reserved "unknown" bucket the aggregator uses is what makes group=agent answer
// identically whether it was served from the ring or from disk.
//
// This is a deliberate departure from how labelFor treats an absent endpoint or
// model, which produce no series entry at all. That is right for those axes: a row
// with no model is not inference, so it is not ABOUT that axis. An absent agent is
// different — the spend certainly happened and certainly belongs somewhere in a
// per-agent breakdown, which is also why the aggregator's byAgent has no guard where
// byEndpoint and byMethod do.
func TestFold_GroupAgentMapsAbsenceToUnknown(t *testing.T) {
	rows := []Row{
		{Endpoint: "gw", Model: "m", Agent: "claude-code/2.1.14",
			Counts: usage.Counts{Requests: 1, CostMicros: 100}},
		{Endpoint: "gw", Model: "m", Agent: "",
			Counts: usage.Counts{Requests: 1, CostMicros: 200}},
	}

	totals, series := Fold(rows, usage.GroupAgent)

	if _, blank := series[""]; blank {
		t.Error(`series has an "" key; a blank row reads as a bug rather than as unattributed traffic`)
	}
	if got := series["unknown"].CostMicros; got != 200 {
		t.Errorf("unknown CostMicros = %d, want 200", got)
	}
	// The series sums to the total, which is the property the mapping buys: dropping
	// unattributed rows from the breakdown would leave a client unable to reconcile
	// a per-agent table against the figure beside it.
	var sum int64
	for _, c := range series {
		sum += c.CostMicros
	}
	if sum != totals.CostMicros {
		t.Errorf("series sums to %d but totals is %d", sum, totals.CostMicros)
	}
}

// TestLedgerAndRingAgreeOnASpoofedUnknownAgent pins the claim agentLabel's comment
// makes: a caller that sends literally "User-Agent: unknown" is bucketed the same way
// by the ledger and by the live aggregator.
//
// Reachable from off-host, so it is worth a test rather than a claim. The ring folds
// it under Label() == "unknown"; the ledger normalises it to "" on disk and labelFor
// maps that back to "unknown". Two spellings of unattributed traffic would show as two
// rows in any client that merged the two sources, which is the bug this prevents.
func TestLedgerAndRingAgreeOnASpoofedUnknownAgent(t *testing.T) {
	c := pipeline.ParseUserAgent("unknown")
	if c == nil {
		t.Fatal("ParseUserAgent(\"unknown\") = nil; a header WAS sent")
	}

	// The ring's key.
	ringKey := c.Label()
	// The ledger's: stored by agentLabel, read back by labelFor.
	stored := agentLabel(c)
	if stored != "" {
		t.Errorf("agentLabel = %q, want \"\": absence has one representation on disk", stored)
	}
	ledgerKey, ok := labelFor(Row{Agent: stored}, usage.GroupAgent)
	if !ok {
		t.Fatal("labelFor dropped the row; unattributed spend must still appear in the series")
	}
	if ledgerKey != ringKey {
		t.Errorf("ledger key %q != ring key %q; the two sources would render two rows for one thing",
			ledgerKey, ringKey)
	}
}
