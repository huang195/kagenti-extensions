package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// costSessionsModel builds a model wide and tall enough that nothing the sessions
// table renders is width- or height-constrained, so a missing cell is a missing
// cell rather than a fitting decision.
func costSessionsModel() *model {
	m := &model{width: 120, height: 40}
	m.sessionsTbl = newSessionsTable()
	return m
}

// sessionsRowCell returns one named column's cell from the first row, addressed by
// the column's TITLE rather than by an index.
//
// Positional indexing is what makes table.Row dangerous: it is a []string, so
// inserting a column shifts every reader silently instead of failing to compile.
// A test that hardcodes an index would keep passing while asserting about the
// wrong column.
func sessionsRowCell(t *testing.T, m *model, title string) string {
	t.Helper()
	rows := m.sessionsTbl.Rows()
	if len(rows) == 0 {
		t.Fatal("sessions table has no rows")
	}
	for i, c := range m.sessionsTbl.Columns() {
		if c.Title != title {
			continue
		}
		if i >= len(rows[0]) {
			t.Fatalf("column %q is at index %d but the row has only %d cells: %v", title, i, len(rows[0]), rows[0])
		}
		return rows[0][i]
	}
	t.Fatalf("no %q column in the sessions table (columns: %v)", title, m.sessionsTbl.Columns())
	return ""
}

func TestSessionsTable_ShowsCostPerSession(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 3, TotalTokens: 100}}
	m.spend.snap = &usage.Snapshot{
		Window: "6h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"sess-a": {Requests: 3, CostMicros: 250_000, PricedRequests: 3, PriceableRequests: 3},
			},
		}},
	}

	m.rebuildSessionsTable()

	row := m.sessionsTbl.Rows()[0]
	joined := strings.Join(row, " ")
	if !strings.Contains(joined, "0.25") {
		t.Errorf("row %v does not show the session's $0.25 cost", row)
	}
	// Named-column check as well as the joined one: the joined assertion would pass
	// if the figure landed in the wrong cell.
	if got := sessionsRowCell(t, m, "COST"); !strings.Contains(got, "0.25") {
		t.Errorf("COST cell = %q, want the session's $0.25", got)
	}
}

func TestSessionsTable_UnpricedSessionShowsNoZero(t *testing.T) {
	// The rule the whole feature rests on: an unknown cost is not a zero one, and
	// a table cell has even less room to explain itself than the strip does.
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 3, TotalTokens: 100}}
	m.spend.snap = &usage.Snapshot{Window: "6h", Priced: false}

	m.rebuildSessionsTable()

	row := m.sessionsTbl.Rows()[0]
	for _, cell := range row {
		if strings.Contains(cell, "$0.00") || cell == "0.0000" {
			t.Errorf("row %v renders a zero cost for an unpriced session", row)
		}
	}
}

// A snapshot that priced SOME traffic still knows nothing about a session that
// contributed none of it. Snapshot.Priced is a window-wide flag, so trusting it
// alone would price every row off another session's figure — and the row would
// read $0.0000, which is the one thing the feature forbids.
func TestSessionsTable_SessionAbsentFromAPricedWindowShowsNoZero(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "quiet", EventCount: 1}}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"busy": {Requests: 9, CostMicros: 900_000, PricedRequests: 9, PriceableRequests: 9},
			},
		}},
	}

	m.rebuildSessionsTable()

	if got := sessionsRowCell(t, m, "COST"); got != "" {
		t.Errorf("COST cell = %q for a session the priced window has no figure for, want blank", got)
	}
}

// A session whose requests were all PRICEABLE but none PRICED has an unknown
// cost, not a zero one — the same distinction the strip's coverage figure exists
// to report, applied per row.
func TestSessionsTable_PriceableButUnpricedSessionShowsNoZero(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 4}}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"sess-a": {Requests: 4, PriceableRequests: 4},
				"other":  {Requests: 1, CostMicros: 5_000, PricedRequests: 1, PriceableRequests: 1},
			},
		}},
	}

	m.rebuildSessionsTable()

	if got := sessionsRowCell(t, m, "COST"); got != "" {
		t.Errorf("COST cell = %q for a priceable-but-unpriced session, want blank", got)
	}
}

// Cached-only rows are sessions the server no longer lists, so the aggregator's ring
// has been reclaimed too. The COST cell is blank, and the row must still have exactly
// as many cells as there are columns — a short row shifts nothing but renders one
// column of nothing, while a long one panics inside bubbles.
func TestSessionsTable_CachedOnlyRowHasNoCost(t *testing.T) {
	m := costSessionsModel()
	m.events = map[string][]pipeline.SessionEvent{"vanished": make([]pipeline.SessionEvent, 2)}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"vanished": {Requests: 2, CostMicros: 100_000, PricedRequests: 2, PriceableRequests: 2},
			},
		}},
	}

	m.rebuildSessionsTable()

	if got := sessionsRowCell(t, m, "COST"); got != "" {
		t.Errorf("cached-only COST cell = %q, want blank", got)
	}
}

// Every row must carry exactly one cell per column. bubbles indexes m.cols[i] as
// it walks the row, so a row LONGER than the column list panics inside View();
// a shorter one silently renders a blank trailing column. Both live rows —
// server-listed and cached-only — are built in different places, so both need
// pinning or one of them drifts.
func TestSessionsTable_EveryRowHasOneCellPerColumn(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "listed", EventCount: 1, CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now()}}
	m.events = map[string][]pipeline.SessionEvent{"vanished": make([]pipeline.SessionEvent, 2)}

	m.rebuildSessionsTable()

	rows := m.sessionsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want a listed one and a cached-only one", len(rows))
	}
	want := len(m.sessionsTbl.Columns())
	for _, r := range rows {
		if len(r) != want {
			t.Errorf("row %v has %d cells, want %d (one per column)", r, len(r), want)
		}
	}
	// Renders without panicking, which is the failure a long row produces.
	_ = m.sessionsTbl.View()
}

// The cells have to fit the columns they were declared with. A cell wider than
// its column is truncated by bubbles with runewidth.Truncate, which would put an
// ellipsis in the middle of a dollar amount — the one thing #953 rules out.
func TestSessionsTable_CostFitsItsColumn(t *testing.T) {
	widths := map[string]int{}
	for _, c := range newSessionsTable().Columns() {
		widths[c.Title] = c.Width
	}
	// lipgloss.Width, not len: this measures DISPLAY COLUMNS, which is what bubbles
	// compares against. The package's trunc and truncStr helpers are both
	// cell-unaware (one slices runes, the other bytes) and neither may be used for
	// width arithmetic.
	for _, cost := range []float64{0, 0.00001, 0.25, 12.5, 1234.5678, 9999.9999} {
		if got := lipgloss.Width(formatUSDCell(cost)); got > widths["COST"] {
			t.Errorf("formatUSDCell(%v) is %d columns, COST is %d wide", cost, got, widths["COST"])
		}
	}
	// The ceiling, asserted rather than left implicit: five figures of dollars does
	// NOT fit, and bubbles would truncate it into "$10000.00…". Accepted because it
	// means one session spending $10,000 inside the strip's one-hour window, and the
	// two extra columns are paid for on every terminal. If this ever starts failing
	// because the formatter got narrower, widen the column and delete this — a
	// documented ceiling that has silently moved is worse than none.
	if lipgloss.Width(formatUSDCell(10_000)) <= widths["COST"] {
		t.Errorf("formatUSDCell(10000) now fits COST (%d wide); the documented ceiling in newSessionsTable is stale", widths["COST"])
	}
}

// fittedCostWidth returns the width fitTableColumns leaves the COST column at terminal
// width w, failing the test if the column is not in the fitted set at all.
//
// The absence check is the invariant the sessions godoc used to have backwards: the fitter
// SHRINKS the widest column and never drops one, which is what keeps the positional-index
// trap closed (event_retention_test.go asserts row[5]). A fitter that started dropping
// columns would spring it, and a shifted index still compiles and still asserts.
func fittedCostWidth(t *testing.T, w int) int {
	t.Helper()
	fitted := fitTableColumns(sessionsColumns(), w)
	if len(fitted) != len(sessionsColumns()) {
		t.Fatalf("width %d: fitTableColumns returned %d of %d columns; a dropped column shifts every positional row reader",
			w, len(fitted), len(sessionsColumns()))
	}
	for _, c := range fitted {
		if c.Title == "COST" {
			return c.Width
		}
	}
	t.Fatalf("width %d: no COST column in the fitted set: %v", w, fitted)
	return 0
}

// The FITTER'S OUTPUT, which is what nothing checked.
//
// TestSessionsTable_CostFitsItsColumn above pins formatUSDCell against the 10 columns
// sessionsColumns DECLARES — a width the table only has from a 70-column terminal up.
// fitTableColumns shrinks COST to 5 at 40, 7 at 50 and 8 at 60, and bubbles renders a cell
// as runewidth.Truncate(value, col.Width, "…"), which cuts from the right and keeps the
// leading digits: $1234.5678 in a 5-wide column comes out "$123…", ten times smaller and
// still readable as an amount. That is exactly the "never render a partial number" rule the
// COST column's own comment says #953 forbids, and a constructor-only assertion could not
// see it.
//
// Every cell must therefore be one of exactly two things: the whole figure, or the elision
// marker. Nothing in between, and NOT blank — blank means "unpriced" in this table.
func TestSessionsTable_CostCellIsNeverATruncatedNumber(t *testing.T) {
	// Micros rather than dollars so the expected string is derived from the same value the
	// row builder formats; a float literal round-tripped through 1e6 could disagree in the
	// fourth decimal and make the test about arithmetic instead of about width.
	amounts := []int64{
		30,            // "<$0.0001", the sub-floor form: 8 columns
		120_000,       // "$0.1200": 7 columns, fits from a 50-column terminal up
		12_500_000,    // "$12.5000": 8 columns
		1_234_567_800, // "$1234.5678": 10 columns, needs the full declared width
		9_999_999_900, // "$9999.9999": 10 columns, the documented ceiling
	}
	for _, w := range []int{40, 50, 60, 70, 80, 96} {
		costW := fittedCostWidth(t, w)
		for _, micros := range amounts {
			usd := float64(micros) / 1e6
			whole := formatUSDCell(usd)

			m := &model{width: w, height: 40}
			m.sessionsTbl = newSessionsTable()
			m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 1}}
			m.spend.snap = &usage.Snapshot{
				Window: "1h", Priced: true,
				Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
					"sess-a": {Requests: 1, CostMicros: micros, PricedRequests: 1, PriceableRequests: 1},
				}}},
			}
			// layout() is the production path — it is the only caller of
			// SetColumns(fitTableColumns(...)) — so this exercises the real fitted widths
			// rather than a width the test computed for itself.
			m.layout()
			m.rebuildSessionsTable()

			got := sessionsRowCell(t, m, "COST")
			switch got {
			case whole:
				// Stated in full, so it must actually fit or bubbles will clip it anyway.
				if gw := lipgloss.Width(got); gw > costW {
					t.Errorf("term %d, %s: COST cell is %d columns in a %d-wide column; bubbles truncates it to a smaller-looking figure",
						w, whole, gw, costW)
				}
			case costElision:
				// Elided, so the whole figure must genuinely not have fitted — otherwise
				// the fix is hiding a figure it could have shown.
				if lipgloss.Width(whole) <= costW {
					t.Errorf("term %d, %s: COST elided although %d columns fit a %d-wide column",
						w, whole, lipgloss.Width(whole), costW)
				}
			case "":
				t.Errorf("term %d, %s: COST cell is blank, which in this table means UNPRICED; a known cost that will not fit must say %q",
					w, whole, costElision)
			default:
				t.Errorf("term %d, %s: COST cell = %q, want the whole figure or %q — never a partial number",
					w, whole, got, costElision)
			}
		}
	}
}

// The widths the sessions godoc and fitCostCell's comment both quote. Asserted so the
// prose cannot go stale silently the way the "106 columns / drops trailing columns" note
// it replaced did.
func TestFitTableColumns_SessionsCostWidthsAreWhatTheCommentsClaim(t *testing.T) {
	if got := tableWidth(sessionsColumns()); got != 102 {
		t.Errorf("declared sessions table width = %d, want 102 (90 declared + 6 columns × cellPadding)", got)
	}
	for _, tc := range []struct{ term, cost int }{{40, 5}, {50, 7}, {60, 8}, {70, 10}, {80, 10}, {96, 10}} {
		if got := fittedCostWidth(t, tc.term); got != tc.cost {
			t.Errorf("terminal %d: fitted COST width = %d, want %d", tc.term, got, tc.cost)
		}
	}
}

func TestSessionCost_SumsEveryBucketForTheSession(t *testing.T) {
	// The strip asks for one bucket, but nothing guarantees the server folds to one
	// — resolution is negotiated, not dictated. Reading Buckets[0] alone would
	// report the first slice of the window as the whole of it.
	m := costSessionsModel()
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{
			{Series: map[string]usage.Counts{"sess-a": {CostMicros: 100_000, PricedRequests: 1, PriceableRequests: 1}}},
			{Series: map[string]usage.Counts{"sess-a": {CostMicros: 150_000, PricedRequests: 1, PriceableRequests: 1}}},
			{Series: nil},
		},
	}

	usd, priced := m.sessionCost("sess-a")
	if !priced {
		t.Fatal("priced = false for a session with two priced buckets")
	}
	if usd != 0.25 {
		t.Errorf("sessionCost = %v, want 0.25 (both buckets summed)", usd)
	}
}

func TestSessionCost_UnknownCases(t *testing.T) {
	priced := &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"sess-a": {CostMicros: 100_000, PricedRequests: 1, PriceableRequests: 1},
		}}},
	}
	cases := []struct {
		name string
		snap *usage.Snapshot
		id   string
	}{
		{"no snapshot yet", nil, "sess-a"},
		{"window priced nothing", &usage.Snapshot{Window: "1h", Priced: false}, "sess-a"},
		{"empty session id", priced, ""},
		{"session not in the series", priced, "sess-b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := costSessionsModel()
			m.spend.snap = tc.snap
			usd, ok := m.sessionCost(tc.id)
			if ok {
				t.Errorf("priced = true; an unknown cost must be reported as unknown, not as %v", usd)
			}
			if usd != 0 {
				t.Errorf("usd = %v alongside priced=false; a caller trusting the figure would render it", usd)
			}
		})
	}
}
