package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

func TestRenderSpendStrip_NeverExceedsTheWidth(t *testing.T) {
	// The strip lives in the chrome. A line that overflows wraps, and a wrapped
	// chrome line costs a row of the table below it -- the same failure
	// fitHintLine exists to prevent.
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	for _, w := range []int{120, 100, 80, 64, 48, 32, 24, 16, 8, 1} {
		got := renderSpendStrip(s, w)
		if gw := lipgloss.Width(got); gw > w {
			t.Errorf("width %d: rendered %d columns: %q", w, gw, got)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("width %d: strip contains a newline: %q", w, got)
		}
	}
}

func TestRenderSpendStrip_DropsWholeFiguresNeverClipsANumber(t *testing.T) {
	// #953: "no truncated numbers". A half-rendered dollar amount is worse than a
	// missing one -- it reads as a real, smaller figure.
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	for _, w := range []int{120, 100, 80, 64, 48, 32, 24, 16, 8} {
		got := renderSpendStrip(s, w)
		if got == "" {
			continue
		}
		// Any figure that appears at all must appear IN FULL, as formatUSDCell
		// actually renders it: "$1.1200", not "$1.12". Asserting the short form could
		// not detect clipping -- output truncated to "SPEND  $1.12" contains both
		// "$1.1" and "$1.12", so it passed, and only a clip to "$1.1" or shorter was
		// ever caught. The prefix probes stop just past the "$" so a figure clipped
		// anywhere in its digits still trips them.
		for prefix, whole := range map[string]string{
			"$1.": "$1.1200", // the window figure
			"$0.": "$0.0187", // the burn rate
		} {
			if strings.Contains(got, prefix) && !strings.Contains(got, whole) {
				t.Errorf("width %d: %q has a clipped %q figure (want the whole %q)", w, got, prefix, whole)
			}
		}
		if strings.HasSuffix(strings.TrimSpace(got), "$") {
			t.Errorf("width %d: %q ends mid-figure", w, got)
		}
	}
}

func TestRenderSpendStrip_TheClipAssertionCanActuallyFail(t *testing.T) {
	// Guards the guard above. That test is only worth anything if its assertion
	// fails on clipped input, so feed it clipped input directly. This is the defect
	// class that already bit this branch twice: a test that cannot detect the thing
	// it is named for.
	for _, clipped := range []string{"SPEND  $1.12", "SPEND  $1.1", "SPEND  $1.120"} {
		if strings.Contains(clipped, "$1.") && strings.Contains(clipped, "$1.1200") {
			t.Errorf("%q satisfied the whole-figure assertion; the clip test is blind to it", clipped)
		}
	}
	// ...and passes on the real, unclipped rendering, so it is not vacuously strict.
	if whole := "SPEND  $1.1200 /1h"; strings.Contains(whole, "$1.") && !strings.Contains(whole, "$1.1200") {
		t.Errorf("%q failed the whole-figure assertion; the clip test rejects correct output", whole)
	}
}

func TestRenderSpendStrip_WideEnoughShowsBothFigures(t *testing.T) {
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	got := renderSpendStrip(s, 120)
	for _, want := range []string{"SPEND", "$1.12", "1h", "/min"} {
		if !strings.Contains(got, want) {
			t.Errorf("strip %q missing %q", got, want)
		}
	}
}

func TestRenderSpendStrip_NarrowKeepsTheHeadlineFigure(t *testing.T) {
	// Degradation drops from the RIGHT: the leftmost figure is the headline and is
	// the last thing to go. (fitHintLine drops from the front for the opposite
	// reason -- its essential hints are last.)
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	got := renderSpendStrip(s, 24)
	if !strings.Contains(got, "$1.12") {
		t.Errorf("narrow strip %q dropped the headline figure", got)
	}
	if strings.Contains(got, "/min") {
		t.Errorf("narrow strip %q kept the burn rate; it should drop before the headline", got)
	}
}

func TestRenderSpendStrip_UnpricedSaysSoAndNeverShowsZero(t *testing.T) {
	s := spendSummary{WindowLabel: "1h", Priced: false, Unpriced: 12, Priceable: 318}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "unavailable") {
		t.Errorf("strip %q does not say cost is unavailable", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("strip %q renders $0.00 for an unknown cost", got)
	}
}

func TestRenderSpendStrip_PartiallyPricedDisclosesTheGap(t *testing.T) {
	// A dollar total covering only the priced subset must say so; presenting a
	// subtotal as the whole spend is the failure the coverage counters exist for.
	s := spendSummary{
		WindowUSD: 4.17, WindowLabel: "1h", BurnPerMin: 0.07,
		Priced: true, Unpriced: 12, Priceable: 318,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "12") {
		t.Errorf("strip %q does not disclose the 12 unpriced requests", got)
	}
}

func TestRenderSpendStrip_FullyPricedIsNotAnnotated(t *testing.T) {
	// A correctly configured deployment must not carry a permanent warning; that
	// is what trains an operator to ignore the one signal that matters.
	s := spendSummary{
		WindowUSD: 4.17, WindowLabel: "1h", BurnPerMin: 0.07,
		Priced: true, Unpriced: 0, Priceable: 318,
	}
	got := renderSpendStrip(s, 120)
	if strings.Contains(got, "unpriced") {
		t.Errorf("fully priced strip %q still warns about coverage", got)
	}
}

func TestRenderSpendStrip_NoDataYetRendersNothingUseful(t *testing.T) {
	// Before the first poll returns. An empty strip is honest; "$0.00" is not.
	got := renderSpendStrip(spendSummary{}, 120)
	if strings.Contains(got, "$0.00") {
		t.Errorf("strip %q renders $0.00 before any data arrived", got)
	}
}

func TestRenderSpendStrip_NoSavedFigureWhenUnmeasured(t *testing.T) {
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true, HasSaved: false}
	if got := renderSpendStrip(s, 120); strings.Contains(got, "saved") {
		t.Errorf("strip %q shows a saved figure with nothing measuring it", got)
	}
}

func TestRenderSpendStrip_ShowsSavedWhenMeasured(t *testing.T) {
	// Proves the field is wired now so a later commit adds data, not a branch.
	s := spendSummary{
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		SavedUSD: 0.24, HasSaved: true,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "saved") || !strings.Contains(got, "$0.24") {
		t.Errorf("strip %q does not show the measured saving", got)
	}
}

func TestRenderSpendStrip_ShowsTodayWhenAvailable(t *testing.T) {
	s := spendSummary{
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		TodayUSD: 4.17, HasToday: true,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "today") || !strings.Contains(got, "$4.17") {
		t.Errorf("strip %q does not show today's total", got)
	}
	// Today becomes the headline when present, so it must survive a narrow width.
	if narrow := renderSpendStrip(s, 24); !strings.Contains(narrow, "$4.17") {
		t.Errorf("narrow strip %q dropped today's total, which is the headline", narrow)
	}
}

func TestRenderSpendStrip_WideCharacterSafety(t *testing.T) {
	// footer.go:88-92 records the bug: a budget computed in display columns but
	// sliced by rune index overflowed on any wide character. The strip's own text
	// is ASCII, but the width arithmetic must be column-based regardless, because
	// the next figure added may not be.
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	for w := 1; w <= 60; w++ {
		if gw := lipgloss.Width(renderSpendStrip(s, w)); gw > w {
			t.Fatalf("width %d: rendered %d columns", w, gw)
		}
	}
}

func TestSpendStripVisible_HiddenOnPreConnectionPickers(t *testing.T) {
	// The namespace and pod pickers run before any session exists, so there is no
	// cost to show. They also return early from paneView with their own layout.
	for _, p := range []paneID{paneNamespaces, panePods} {
		m := &model{height: 40}
		m.pane = p
		if m.spendStripVisible() {
			t.Errorf("pane %v: strip visible on a pre-connection picker", p)
		}
	}
}

func TestSpendStripVisible_ShownOnTheDataPanes(t *testing.T) {
	for _, p := range []paneID{paneSessions, paneEvents, paneDetail, panePipeline, paneUsage, paneCatalog} {
		m := &model{height: 40}
		m.pane = p
		if !m.spendStripVisible() {
			t.Errorf("pane %v: strip hidden on a data pane", p)
		}
	}
}

func TestSpendStripVisible_FoldsAwayOnAShortTerminal(t *testing.T) {
	// The spec's rule: below 20 rows the strip yields its row to the table, which
	// needs it more than the chrome does.
	m := &model{height: 19}
	m.pane = paneEvents
	if m.spendStripVisible() {
		t.Error("strip took a row on a 19-row terminal")
	}
	m.height = 20
	if !m.spendStripVisible() {
		t.Error("strip hidden at 20 rows, the documented threshold")
	}
}

func TestLayout_ReservesExactlyOneRowForTheStrip(t *testing.T) {
	// Get this wrong and every table renders one row too tall, pushing the footer
	// off-screen. The row is ADDED to the existing title(1) + footer(2) budget --
	// layout's old comment said "title + blank + footer" but there was never a
	// blank row to borrow.
	tall := &model{width: 100, height: 40}
	tall.pane = paneEvents
	tall.layout()

	short := &model{width: 100, height: 19} // below the fold threshold
	short.pane = paneEvents
	short.layout()

	if want := 40 - 4; tall.bodyHeight != want {
		t.Errorf("bodyHeight with strip = %d, want %d (height - title - strip - 2 footer rows)",
			tall.bodyHeight, want)
	}
	if want := 19 - 3; short.bodyHeight != want {
		t.Errorf("bodyHeight below the fold = %d, want %d (no strip row)", short.bodyHeight, want)
	}
}

func TestLayout_PickerPanesStillReserveTheStripRow(t *testing.T) {
	// The ruling: reserve by HEIGHT, not per pane. layout() runs on resize, so a
	// pane-aware reservation would need re-running on every pane transition -- and
	// a picker one row shorter than it could be is invisible next to an events
	// table that is wrong by a row and pushes the footer off the bottom.
	picker := &model{width: 100, height: 40}
	picker.pane = panePods
	picker.layout()

	if want := 40 - 4; picker.bodyHeight != want {
		t.Errorf("picker bodyHeight = %d, want %d: the reservation must not depend on the pane",
			picker.bodyHeight, want)
	}
	if picker.spendStripVisible() {
		t.Error("the picker reserves the row but must not DRAW the strip")
	}
}

func TestPaneView_DrawsTheStrip(t *testing.T) {
	// The mutation guard for task 3 step 7. A renderSpendStrip call that nothing
	// asserts on is the failure mode here: the strip would be written, reviewed,
	// and never reach the screen. Deleting the append in paneView must fail this.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height = 100, 40
	m.layout()
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 10, CostMicros: 1_120_000,
			PricedRequests: 10, PriceableRequests: 10,
		},
		Priced: true,
	}

	got := m.paneView()
	if !strings.Contains(got, stripLabel) {
		t.Errorf("paneView output has no %q row; the strip is not wired to the screen", stripLabel)
	}
	if !strings.Contains(got, "$1.12") {
		t.Error("paneView output has no spend figure; the strip row is rendered from something other than spendSummary")
	}
	// The strip must be the SECOND row, directly under the title bar. "In the
	// chrome, read before the data" is the whole requirement -- a strip rendered
	// below the table is just the per-row cost figure again, one pane over.
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("paneView rendered %d lines; expected at least a title and a strip", len(lines))
	}
	if !strings.Contains(lines[1], stripLabel) {
		t.Errorf("row 1 is %q, want the %q strip directly under the title", lines[1], stripLabel)
	}
}

func TestPaneView_NoStripRowOnAShortTerminal(t *testing.T) {
	// Below the fold the row is not drawn AND not reserved; drawing it without the
	// reservation is what pushes the footer off the bottom of the terminal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height = 100, 19
	m.layout()
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}

	if got := m.paneView(); strings.Contains(got, stripLabel) {
		t.Errorf("19-row terminal drew the strip row it did not reserve: %q", got)
	}
}

func TestPaneView_PickerPanesDrawNoStrip(t *testing.T) {
	// paneNamespaces and panePods return early from paneView with their own
	// JoinVertical, so this also guards against the strip being added there.
	for _, p := range []paneID{paneNamespaces, panePods} {
		ctx, cancel := context.WithCancel(context.Background())
		m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
		m.width, m.height = 100, 40
		m.layout()
		m.pane = p
		m.spend.snap = &usage.Snapshot{
			Window: "1h",
			Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
			Priced: true,
		}
		if got := m.paneView(); strings.Contains(got, stripLabel) {
			t.Errorf("pane %v drew the strip: %q", p, got)
		}
		cancel()
	}
}

func TestRenderSpendStrip_FailedPollSaysUnavailableNotNothing(t *testing.T) {
	// Finding 2. A failing /v1/usage used to render "" on every poll forever, while
	// layout() went on reserving the row: a permanent blank line above the footer
	// and no diagnostic anywhere on screen. Silence is the one unacceptable answer,
	// because the row is spent either way.
	s := spendSummary{Failed: true}
	got := renderSpendStrip(s, 120)

	if got == "" {
		t.Fatal("a failed poll rendered nothing; the reserved row becomes a permanent blank line")
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("strip %q does not say cost is unavailable", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("strip %q shows a dollar amount for a poll that never answered", got)
	}
}

func TestRenderSpendStrip_FailedPollFitsEveryWidth(t *testing.T) {
	// The failure path is the one an operator sees for as long as the endpoint is
	// broken, so it must obey the width contract like any other.
	s := spendSummary{Failed: true}
	for w := 1; w <= 80; w++ {
		got := renderSpendStrip(s, w)
		if gw := lipgloss.Width(got); gw > w {
			t.Fatalf("width %d: rendered %d columns: %q", w, gw, got)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("width %d: strip contains a newline: %q", w, got)
		}
	}
}

func TestRenderSpendStrip_FailedPollOutranksTheNoDataPath(t *testing.T) {
	// Failed must not be inferred from the counters. A failed poll has none, so if
	// the renderer reached the Priceable == 0 branch it would return "" -- which is
	// exactly the bug. Pin the precedence.
	failed := renderSpendStrip(spendSummary{Failed: true}, 120)
	quiet := renderSpendStrip(spendSummary{}, 120)

	if failed == quiet {
		t.Errorf("a failed poll renders identically to no-data-yet (%q); the two are different answers", failed)
	}
	if quiet != "" {
		t.Errorf("no-data-yet rendered %q, want the empty string", quiet)
	}
}

func TestRenderSpendStrip_SettledZeroShowsZeroNotUnavailable(t *testing.T) {
	// Finding 3, the half that was unpinned. A window that WAS priced and cost
	// exactly nothing is a known answer: the gateway declared the traffic free.
	// authlib/usage asserts "a settled zero IS priced", and formatUSDCell renders
	// "<$0.0001" for any positive amount under the floor -- so "$0.0000" in the
	// strip can only ever mean an exact, settled zero.
	//
	// Without this test, someone could "fix" the zero into an unavailable branch
	// and the suite would stay green, silently conflating free traffic with
	// unmeasured traffic -- the precise distinction the strip exists to draw.
	s := spendSummary{WindowUSD: 0, WindowLabel: "1h", Priced: true, Priceable: 10}
	got := renderSpendStrip(s, 120)

	if got == "" {
		t.Fatal("a settled zero rendered nothing; free traffic is a real, knowable answer")
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("strip %q reports a SETTLED zero as unavailable; that conflates free with unknown", got)
	}
	if !strings.Contains(got, "$0.0000") {
		t.Errorf("strip %q does not state the settled zero as an amount", got)
	}
	// And it must be distinguishable from the unknown case, which is the whole point.
	if unknown := renderSpendStrip(spendSummary{WindowLabel: "1h", Priceable: 10}, 120); got == unknown {
		t.Errorf("a settled zero and an unknown cost render identically as %q", got)
	}
}

func TestRenderSpendStrip_NoPriceableTrafficSaysSoRatherThanNothing(t *testing.T) {
	// A poll answered and found no inference traffic at all. That is a finding, not
	// an absence: the row is reserved on height alone, so rendering "" here buys a
	// blank line above the footer that reads as a broken UI.
	s := spendSummary{WindowLabel: "1h", HasSnapshot: true, Priceable: 0}
	got := renderSpendStrip(s, 120)

	if got == "" {
		t.Fatal("a window with no priceable traffic rendered nothing, wasting its reserved row")
	}
	if !strings.Contains(got, "no priceable traffic") {
		t.Errorf("strip %q does not say there was no priceable traffic", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("strip %q shows a dollar amount for a window with nothing to price", got)
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("strip %q says cost is unavailable; the cost is knowable and there simply was none to price", got)
	}
}

func TestRenderSpendStrip_BeforeTheFirstPollStaysSilent(t *testing.T) {
	// The one case where "" is right. "We have not looked" is honest, brief and
	// self-correcting within a poll interval -- and it must stay distinguishable
	// from "we looked and found nothing to price", which is the test above.
	quiet := renderSpendStrip(spendSummary{}, 120)
	if quiet != "" {
		t.Errorf("pre-first-poll rendered %q, want the empty string", quiet)
	}

	looked := renderSpendStrip(spendSummary{WindowLabel: "1h", HasSnapshot: true}, 120)
	if looked == quiet {
		t.Error("'not looked yet' and 'looked, nothing priceable' render identically")
	}
}

func TestRenderSpendStrip_NoPriceableTrafficFitsEveryWidth(t *testing.T) {
	// It is a steady state for a proxy handling only non-LLM traffic, so it obeys
	// the width contract like every other branch.
	s := spendSummary{WindowLabel: "1h", HasSnapshot: true}
	for w := 1; w <= 80; w++ {
		got := renderSpendStrip(s, w)
		if gw := lipgloss.Width(got); gw > w {
			t.Fatalf("width %d: rendered %d columns: %q", w, gw, got)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("width %d: strip contains a newline: %q", w, got)
		}
	}
}

func TestRenderSpendStrip_UnpricedGapStillOutranksTheNoTrafficLine(t *testing.T) {
	// Priceable > 0 with nothing priced is a COVERAGE problem and must keep saying
	// so; the no-traffic line is only for Priceable == 0. Pins the precedence so
	// the new branch cannot swallow the coverage warning.
	s := spendSummary{WindowLabel: "1h", HasSnapshot: true, Unpriced: 12, Priceable: 318}
	got := renderSpendStrip(s, 120)

	if strings.Contains(got, "no priceable traffic") {
		t.Errorf("strip %q claims no priceable traffic while reporting 318 priceable requests", got)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("strip %q lost the coverage warning", got)
	}
}

// A LATENT bug, armed by the commit that first sets HasToday. The "nothing was
// priced" branch is guarded on `!s.Priced && !s.HasToday`, so a summary carrying a
// today figure over a window that priced nothing falls through to the figures
// path — where the window figure was rendered unconditionally and read "$0.0000
// /1h". That states a settled zero for a cost nobody knows, which is the one thing
// this whole feature forbids, and it is exactly the misreading formatUSDCell's
// floor and the strip's "cost unavailable" branch both exist to prevent.
//
// Unreachable while HasToday is never set, which is why it survived review twice.
// Pinned here rather than left for the ledger commit to trip over: a test is the
// only artefact that a later author cannot skip reading.
func TestRenderSpendStrip_TodayWithAnUnpricedWindowStatesNoWindowZero(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true,
		WindowUSD: 0, WindowLabel: "1h", Priced: false,
		HasSnapshot: true, Priceable: 40, Unpriced: 40,
	}
	got := renderSpendStrip(s, 120)

	if strings.Contains(got, "$0.0000 /1h") {
		t.Errorf("strip %q reports an UNPRICED window as a settled $0.0000", got)
	}
	// The today figure is the one thing here that IS known, so it must survive.
	if !strings.Contains(got, "4.17") {
		t.Errorf("strip %q dropped the today figure, which is the only known cost", got)
	}
	// And the coverage gap still qualifies the total.
	if !strings.Contains(got, "40 of 40 unpriced") {
		t.Errorf("strip %q lost the coverage warning for an unpriced window", got)
	}
}

// The mirror of the above: a PRICED window keeps its figure when today is present.
// Without this the guard could be "fixed" by dropping the window figure whenever
// HasToday is set, which would silently delete a correct reading.
func TestRenderSpendStrip_TodayWithAPricedWindowKeepsBoth(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true,
		WindowUSD: 1.12, WindowLabel: "1h", Priced: true,
		HasSnapshot: true, Priceable: 40,
	}
	got := renderSpendStrip(s, 120)

	if !strings.Contains(got, "$4.1700 today") {
		t.Errorf("strip %q lost the today figure", got)
	}
	if !strings.Contains(got, "$1.1200 /1h") {
		t.Errorf("strip %q lost the priced window figure", got)
	}
}
