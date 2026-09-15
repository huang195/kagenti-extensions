package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// newTestModelOnPane builds a model through the package's real constructor, sized
// and laid out, sitting on the given pane.
//
// The real constructor rather than a &model{} literal: these tests drive key
// messages through Update, and a literal leaves the tables, the filter input and
// bodyHeight at their zero values — so a binding could "work" against a model no
// user ever sees. spend_strip_test.go's cases build the same thing inline; this is
// the named version of that, and the height is above spendStripMinHeight so the
// strip row is both reserved and drawn.
func newTestModelOnPane(t *testing.T, pane paneID) *model {
	t.Helper()
	resetSettingsForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height = 120, 40
	m.layout()
	m.pane = pane
	return m
}

// pricedSnapshot is the smallest snapshot that carries a real, exact, fully
// covered dollar total.
func pricedSnapshot(t *testing.T) *usage.Snapshot {
	t.Helper()
	return &usage.Snapshot{
		Window: usage.WindowToday,
		Priced: true,
		Totals: usage.Counts{
			Requests: 10, CostMicros: 1_120_000,
			PricedRequests: 10, PriceableRequests: 10,
		},
	}
}

// TestOpenCostPane_RecordsTheReturnPaneAndStartsOneChain.
func TestOpenCostPane_RecordsTheReturnPaneAndStartsOneChain(t *testing.T) {
	m := &model{width: 120, height: 40}
	m.pane = paneEvents

	_ = m.openCostPane()

	if m.pane != paneCost {
		t.Errorf("pane = %v, want paneCost", m.pane)
	}
	if m.costPane.returnPane != paneEvents {
		t.Errorf("returnPane = %v, want paneEvents", m.costPane.returnPane)
	}
	if m.costPane.tickGen == 0 {
		t.Error("tickGen not advanced; no chain was started")
	}
}

func TestCostPane_StaleReplyIsDropped(t *testing.T) {
	m := &model{}
	m.costPane.reqSeq = 5
	fresh := &usage.Snapshot{Window: "today"}

	m.applyCostLoaded(costLoadedMsg{req: 4, snap: fresh})
	if m.costPane.snap != nil {
		t.Error("a reply from an older request was applied")
	}

	m.applyCostLoaded(costLoadedMsg{req: 5, snap: fresh})
	if m.costPane.snap != fresh {
		t.Error("the current request's reply was dropped")
	}
}

func TestCostPane_StaleTickIsDropped(t *testing.T) {
	m := &model{}
	m.costPane.tickGen = 2
	if m.costTickIsCurrent(1) {
		t.Error("a tick from generation 1 was accepted while 2 is current")
	}
	if !m.costTickIsCurrent(2) {
		t.Error("the current generation's tick was dropped")
	}
}

func TestOpenCostPane_TwiceLeavesOnlyOneLiveChain(t *testing.T) {
	// The documented historical bug, and this is the THIRD copy of the guard
	// pattern in this package — exactly where it comes back. usage_pane.go's
	// comment records what it costs: two live chains each rescheduling the other's
	// successor doubled the request rate for the life of the session.
	m := &model{width: 120, height: 40}
	m.pane = paneEvents

	_ = m.openCostPane()
	first := m.costPane.tickGen
	m.pane = paneEvents
	_ = m.openCostPane()

	if m.costPane.tickGen == first {
		t.Fatal("tickGen unchanged on re-entry; the previous chain was not invalidated")
	}
	if m.costTickIsCurrent(first) {
		t.Error("a tick from the first chain is still accepted; two chains are live")
	}
}

func TestCostPane_CycleWindowWrapsAndVisitsEach(t *testing.T) {
	var s costPaneState
	seen := map[string]bool{}
	for i := 0; i < len(costPaneWindows)*2; i++ {
		seen[s.window()] = true
		s.cycleWindow()
	}
	if len(seen) != len(costPaneWindows) {
		t.Errorf("visited %d of %d windows across two full cycles: %v",
			len(seen), len(costPaneWindows), seen)
	}
}

func TestCostPane_CycleGroupNeverYieldsNone(t *testing.T) {
	// This pane's whole purpose is the breakdown. An ungrouped view of it is the
	// strip, which is already on screen one row above.
	//
	// usage.GroupAgent is deliberately absent from the expected set: it does not
	// exist. Client identity is not implemented, so there is no per-agent series to
	// ask for, and naming the constant here would be inventing an axis.
	var s costPaneState
	seen := map[usage.Group]bool{}
	for i := 0; i < 12; i++ {
		s.cycleGroup()
		if s.group == usage.GroupNone || s.group == "" {
			t.Fatalf("cycleGroup yielded %q on iteration %d", s.group, i)
		}
		seen[s.group] = true
	}
	for _, want := range []usage.Group{usage.GroupModel, usage.GroupEndpoint, usage.GroupSession} {
		if !seen[want] {
			t.Errorf("cycleGroup never yielded %q", want)
		}
	}
	if len(seen) != len(costPaneGroups) {
		t.Errorf("cycleGroup visited %d groups, want the %d in costPaneGroups: %v",
			len(seen), len(costPaneGroups), seen)
	}
}

func TestCostPane_DoesNotShareStateWithTheEventsPane(t *testing.T) {
	// #953 asks that the pane update live "without disturbing the events pane".
	// The structural form of that requirement: opening and polling this pane must
	// not touch the events table's cursor, filter, or the spend strip's state.
	m := &model{width: 120, height: 40}
	m.pane = paneEvents
	m.eventsTbl.SetCursor(3)
	// The cursor is read back rather than asserted to be 3: this eventsTbl has no
	// rows, and bubbles' SetCursor clamps to the row range, so pinning the literal
	// would test the table's clamp instead of the Cost pane's isolation.
	cursorBefore := m.eventsTbl.Cursor()
	m.filter = "abc"
	spendBefore := m.spend

	_ = m.openCostPane()
	m.applyCostLoaded(costLoadedMsg{req: m.costPane.reqSeq, snap: &usage.Snapshot{Window: "today"}})

	if got := m.eventsTbl.Cursor(); got != cursorBefore {
		t.Errorf("events cursor moved from %d to %d", cursorBefore, got)
	}
	if m.filter != "abc" {
		t.Errorf("filter changed to %q", m.filter)
	}
	if m.spend != spendBefore {
		t.Error("the spend strip's state was modified by the cost pane")
	}
}

// pricedSnapshotWithSeries builds a snapshot whose breakdown carries one entry per
// map key, with that key's cost in micros.
//
// Deliberately NOT a fully-covered fixture. It carries a handful of pricing gaps
// alongside the priced traffic, because that is the deployment these renderers have
// to be readable on — a rate table that covers most of the traffic and not all of
// it — and because a COVERAGE section with nothing in it would leave the height
// budget with nothing to trade away. Tests that need the clean case build it inline.
//
// Every series entry reports all four token kinds, so WHERE IT WENT has something to
// render; the tests that pin the omit-an-unreported-tier rule set PresentKinds
// themselves.
func pricedSnapshotWithSeries(t *testing.T, costByLabel map[string]int64) *usage.Snapshot {
	t.Helper()
	var totals usage.Counts
	series := map[string]usage.Counts{}
	for label, micros := range costByLabel {
		c := usage.Counts{
			Requests: 1, PriceableRequests: 1, PricedRequests: 1, CostMicros: micros,
			Tokens:      1000,
			InputTokens: 100, CacheReadTokens: 800, CacheWriteTokens: 50, OutputTokens: 50,
			PresentKinds: kindInput | kindCacheRead | kindCacheWrite | kindOutput,
		}
		series[label] = c
		totals.Add(c)
	}
	// The gaps. Named pairs an operator could actually add a rate for, and more of
	// them than one line could hold, which is what the section's cap is for.
	gaps := map[string]int64{
		"gw.example.test model-a": 40,
		"gw.example.test model-b": 32,
		"gw.example.test model-c": 21,
		"gw.example.test model-d": 13,
		"gw.example.test model-e": 8,
		"gw.example.test model-f": 5,
		"gw.example.test model-g": 3,
		"gw.example.test model-h": 2,
	}
	var unpriced int64
	for _, n := range gaps {
		unpriced += n
	}
	totals.Requests += unpriced
	totals.PriceableRequests += unpriced
	return &usage.Snapshot{
		Window:  usage.WindowToday,
		Group:   usage.GroupModel,
		Priced:  true,
		Totals:  totals,
		Buckets: []usage.Bucket{{Counts: totals, Series: series}},
		// The pairs that could not be priced, and the provenance of those that could.
		UnpricedBy: gaps,
		PricedBy:   map[string]int64{"authoritative": int64(len(costByLabel))},
	}
}

// sectionOf returns one section of the rendered pane: its heading line and every
// line up to the next heading.
//
// It exists so a section-scoped assertion cannot accidentally match text from
// another section. WHERE IT WENT is the case that needs it — the rule it enforces is
// "no dollar figure here", and the rest of the pane is full of them, so an
// output-wide assertion would pass or fail on the wrong section's content.
//
// A heading is an unindented non-empty line; every body line the renderers emit is
// indented. That is the same contract the renderers hold to, so a section that
// forgot to indent its rows would surface here rather than silently swallowing the
// next section.
func sectionOf(t *testing.T, rendered, heading string) string {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, heading) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no %q section in:\n%s", heading, rendered)
	}
	out := []string{lines[start]}
	for _, l := range lines[start+1:] {
		if l != "" && !strings.HasPrefix(l, " ") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func TestRenderCostPane_TotalSaysUnavailableRatherThanZero(t *testing.T) {
	snap := &usage.Snapshot{Window: "today", Priced: false,
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10}}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	if !strings.Contains(got, "unavailable") {
		t.Errorf("TOTAL does not say cost is unavailable:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("TOTAL renders $0.00 for an unknown cost:\n%s", got)
	}
}

func TestRenderCostPane_DisclosesAnInexactTotal(t *testing.T) {
	// IncompleteRequests means at least one figure in this total is a FLOOR — a
	// truncated stream priced prompt-only. A total that hides that reads as exact.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 10, CostMicros: 4_170_000,
			PricedRequests: 10, PriceableRequests: 10, IncompleteRequests: 2}}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	if !strings.Contains(got, "2") {
		t.Errorf("total does not disclose its 2 inexact figures:\n%s", got)
	}
	// Stronger than the brief's bare "2", which the "$4.1700" figure alone would
	// satisfy: the disclosure has to be a sentence about the two figures, and it has
	// to say which way the total is wrong.
	if !strings.Contains(got, "2 of 10") {
		t.Errorf("total does not count the inexact figures against the priced ones:\n%s", got)
	}
	if !strings.Contains(got, "higher") {
		t.Errorf("total does not say which direction it is wrong in:\n%s", got)
	}
}

func TestRenderCostPane_ExactTotalCarriesNoCaveat(t *testing.T) {
	// The mirror. A permanent caveat with nothing to act on is what teaches an
	// operator to ignore the one that matters.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 10, CostMicros: 4_170_000,
			PricedRequests: 10, PriceableRequests: 10}}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	for _, banned := range []string{"inexact", "floor", "incomplete"} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("exact total carries a %q caveat:\n%s", banned, got)
		}
	}
}

func TestRenderCostPane_BreakdownIsOrderedByCostDescending(t *testing.T) {
	// A table that reshuffles under the cursor is unreadable, and map iteration is
	// nondeterministic. The expensive thing goes first.
	snap := pricedSnapshotWithSeries(t, map[string]int64{
		"cheap": 50, "dear": 3_820_000, "middling": 350_000,
	})
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	iDear, iMid, iCheap := strings.Index(got, "dear"), strings.Index(got, "middling"), strings.Index(got, "cheap")
	if !(iDear < iMid && iMid < iCheap) {
		t.Errorf("rows not ordered by cost descending (dear %d, middling %d, cheap %d):\n%s",
			iDear, iMid, iCheap, got)
	}
}

func TestRenderCostPane_NeverRendersAnEmptySeriesKey(t *testing.T) {
	snap := pricedSnapshotWithSeries(t, map[string]int64{"": 100, "m": 200})
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	lines := strings.Split(got, "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == "$0.0001" {
			t.Errorf("rendered a row for an empty series key:\n%s", got)
		}
	}
	// The brief's assertion above only catches a row that renders as the bare figure.
	// The breakdown is a label column and a figure column, so pin the row count too:
	// one series entry is nameable, the other is not.
	sec := sectionOf(t, got, "BY MODEL")
	if n := strings.Count(sec, "$"); n != 1 {
		t.Errorf("BY MODEL has %d figures, want exactly the one nameable series:\n%s", n, sec)
	}
}

func TestRenderCostPane_WhereItWentIsTokensNotDollars(t *testing.T) {
	// The section deliberately claims tokens, not money. usage.Counts aggregates the
	// four-way TOKEN split and no per-tier dollars at all, and costevent.Event's
	// PromptUSD/OutputUSD are modelled parts that do not reconcile with a gateway's
	// own total — PromptUSD's doc says so in as many words. A dollar figure here
	// would therefore be invented.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1,
			InputTokens: 100, CacheReadTokens: 800, CacheWriteTokens: 50, OutputTokens: 50,
			PresentKinds: 0b1111}}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	sec := sectionOf(t, got, "WHERE IT WENT")
	if strings.Contains(sec, "$") {
		t.Errorf("WHERE IT WENT contains a dollar figure; it reports tokens:\n%s", sec)
	}
	for _, want := range []string{"cache-read", "input", "output"} {
		if !strings.Contains(sec, want) {
			t.Errorf("WHERE IT WENT missing %q:\n%s", want, sec)
		}
	}
}

func TestRenderCostPane_WhereItWentOmitsAnUnreportedTier(t *testing.T) {
	// presentKinds distinguishes "wrote no cache" from "never reports cache". A 0%
	// row for a tier the provider does not expose invents a measurement.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1,
			InputTokens: 100, OutputTokens: 50,
			PresentKinds: 0b1001, // Input|Output only: no cache tiers reported
		}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "WHERE IT WENT")
	if strings.Contains(sec, "cache-read") {
		t.Errorf("rendered a cache-read row for a provider that never reported it:\n%s", sec)
	}
}

func TestRenderCostPane_WhereItWentKeepsAValueWhoseBitIsUnset(t *testing.T) {
	// The mirror of the omit rule, and cmd_cost.go's tokenSplit already holds it: a
	// non-zero count with its bit clear comes from a producer predating PresentKinds,
	// where the value is the only evidence there is. Dropping it would hide a real
	// number in the name of not inventing one.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1,
			InputTokens: 100, CacheReadTokens: 800, OutputTokens: 50,
			PresentKinds: 0, // nothing declared a breakdown
		}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "WHERE IT WENT")
	if !strings.Contains(sec, "cache-read") {
		t.Errorf("dropped a non-zero cache-read count because its bit was clear:\n%s", sec)
	}
	if strings.Contains(sec, "cache-write") {
		t.Errorf("rendered cache-write, which was neither declared nor counted:\n%s", sec)
	}
}

func TestRenderCostPane_WhereItWentSaysSoWhenNothingReportedASplit(t *testing.T) {
	// A gateway reporting only total_tokens: Tokens is non-zero, every split field is
	// zero and PresentKinds is empty. Four 0% rows would assert a measurement that was
	// never made; the section says what happened instead.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1, Tokens: 4200}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "WHERE IT WENT")
	if strings.Contains(sec, "0.0%") || strings.Contains(sec, "cache-read") {
		t.Errorf("rendered a tier for a provider that reported only a total:\n%s", sec)
	}
	if !strings.Contains(sec, "only a total") {
		t.Errorf("section does not say why it is empty:\n%s", sec)
	}
}

func TestRenderCostPane_CoverageNamesTheGapAndIsSilentWhenThereIsNone(t *testing.T) {
	withGap := &usage.Snapshot{Window: "today", Priced: true,
		Totals:     usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 306, PriceableRequests: 318},
		UnpricedBy: map[string]int64{"api.example.com claude-opus-5": 12}}
	got := renderCostPane(withGap, usage.GroupModel, 120, 40)
	if !strings.Contains(got, "api.example.com") {
		t.Errorf("coverage does not NAME the unpriced pair:\n%s", got)
	}

	clean := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 318, PriceableRequests: 318}}
	if g := renderCostPane(clean, usage.GroupModel, 120, 40); strings.Contains(strings.ToLower(g), "unpriced") {
		t.Errorf("fully priced deployment carries a coverage warning:\n%s", g)
	}
}

func TestRenderCostPane_CoverageSaysWhenThePairsAreUnavailable(t *testing.T) {
	// A ledger-backed window (today, 7d) never carries UnpricedBy: a per-minute row
	// holds only its own labels and cannot tell a priced pair from an unpriced one.
	// The gap is still real and visible in the counters, so the section reports it and
	// says why it cannot name it — rendering nothing would read as "no gaps here".
	snap := &usage.Snapshot{Window: usage.WindowToday, Priced: true,
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 306, PriceableRequests: 318}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "COVERAGE")
	if !strings.Contains(sec, "not reported for this window") {
		t.Errorf("coverage neither names the pairs nor says it cannot:\n%s", sec)
	}
}

func TestRenderCostPane_NoBreakdownForAWindowThatCarriesNone(t *testing.T) {
	// group=session on a ledger-backed window: costledger.Fold returns no series for
	// it, because a ledger row carries no session id. An empty section under a "BY
	// SESSION" heading reads as "nothing cost anything"; say what happened instead.
	snap := &usage.Snapshot{Window: usage.WindowToday, Priced: true, Group: usage.GroupSession,
		Totals:  usage.Counts{Requests: 10, CostMicros: 1_000_000, PricedRequests: 10, PriceableRequests: 10},
		Buckets: []usage.Bucket{{Counts: usage.Counts{Requests: 10}}},
	}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupSession, 120, 40), "BY SESSION")
	if !strings.Contains(sec, "no session breakdown") {
		t.Errorf("an empty breakdown does not say it is empty:\n%s", sec)
	}
}

func TestRenderCostPane_UnpricedSeriesRowSaysSoRatherThanZero(t *testing.T) {
	// A series entry with priceable traffic and no priced request has an UNKNOWN cost,
	// not a zero one. $0.0000 against it would assert that model was free.
	snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 2, CostMicros: 1_000_000, PricedRequests: 1, PriceableRequests: 2},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"priced-model":   {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000},
			"unpriced-model": {Requests: 1, PriceableRequests: 1},
		}}},
	}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "BY MODEL")
	if !strings.Contains(sec, "unpriced-model") {
		t.Errorf("the series entry with no figure was dropped entirely:\n%s", sec)
	}
	if strings.Contains(sec, "$0.0000") {
		t.Errorf("rendered a settled zero for a cost nobody knows:\n%s", sec)
	}
}

func TestRenderCostPane_FitsEveryWidthAndHeight(t *testing.T) {
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 100, "b": 200, "c": 300})
	for _, w := range []int{120, 100, 80, 64} {
		for _, h := range []int{40, 24, 20} {
			got := renderCostPane(snap, usage.GroupModel, w, h)
			for i, line := range strings.Split(got, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("w=%d h=%d line %d is %d columns: %q", w, h, i, lw, line)
				}
			}
		}
	}
}

func TestRenderCostPane_FitsAWideCharacterLabel(t *testing.T) {
	// A model name is workload-chosen and arrives verbatim from the request body, so
	// it can be full-width CJK — two display cells per rune. footer.go records what a
	// rune-indexed budget costs on that input: 55 columns rendered for a 40-column
	// budget. trunc and truncStr are both cell-unaware, which is why neither is used
	// in this file.
	snap := pricedSnapshotWithSeries(t, map[string]int64{
		strings.Repeat("日本語", 20): 3_820_000,
	})
	for _, w := range []int{120, 80, 64, 40} {
		got := renderCostPane(snap, usage.GroupModel, w, 40)
		for i, line := range strings.Split(got, "\n") {
			if lw := lipgloss.Width(line); lw > w {
				t.Errorf("w=%d line %d is %d columns: %q", w, i, lw, line)
			}
		}
	}
}

func TestRenderCostPane_SmallTerminalDropsWholeSectionsNotDigits(t *testing.T) {
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000})
	tall := renderCostPane(snap, usage.GroupModel, 120, 40)
	short := renderCostPane(snap, usage.GroupModel, 120, 20)
	if len(short) >= len(tall) {
		t.Fatal("a 20-row terminal rendered no less than a 40-row one; test premise is wrong")
	}
	// Whatever survives, no figure is half-rendered.
	if strings.Contains(short, "$12.5") && !strings.Contains(short, "$12.5000") {
		t.Errorf("a dollar figure was clipped mid-digits:\n%s", short)
	}
	// And the budget is actually respected, not merely smaller.
	if n := len(strings.Split(short, "\n")); n > 20 {
		t.Errorf("a 20-row terminal got %d lines:\n%s", n, short)
	}
	// The total is the one thing that must never be the section that goes. It is the
	// answer to the pane's own question, and the caveats that qualify it ride with it.
	if !strings.Contains(short, "$12.5000") {
		t.Errorf("the total was dropped before the sections below it:\n%s", short)
	}
}

func TestRenderCostPane_TinyHeightStillShowsTheTotal(t *testing.T) {
	// bodyHeight can be small enough that not even the first section fits — a 24-row
	// terminal with the filter open and the strip drawn. Rendering nothing there would
	// make the pane look broken; rendering a clipped "$12.5" would be worse still.
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000})
	for _, h := range []int{1, 2, 3, 4} {
		got := renderCostPane(snap, usage.GroupModel, 120, h)
		if !strings.Contains(got, "$12.5000") {
			t.Errorf("h=%d rendered no total:\n%s", h, got)
		}
		if n := len(strings.Split(got, "\n")); n > h {
			t.Errorf("h=%d rendered %d lines:\n%s", h, n, got)
		}
	}
}

func TestRenderCostPane_NoSnapshotSaysSoWithoutAFigure(t *testing.T) {
	// Before the first reply lands there is no answer, and the pane must not imply one.
	got := renderCostPane(nil, usage.GroupModel, 120, 40)
	if strings.Contains(got, "$") {
		t.Errorf("rendered a dollar figure with no snapshot:\n%s", got)
	}
	if !strings.Contains(got, "COST") {
		t.Errorf("rendered no heading at all:\n%s", got)
	}
}

func TestKey_DollarOpensTheCostPane(t *testing.T) {
	for _, key := range []string{"$", "C"} {
		t.Run(key, func(t *testing.T) {
			m := newTestModelOnPane(t, paneEvents)
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			if m.pane != paneCost {
				t.Errorf("pane = %v after %q, want paneCost", m.pane, key)
			}
		})
	}
}

func TestKey_EscReturnsToTheOpeningPane(t *testing.T) {
	m := newTestModelOnPane(t, paneEvents)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("$")})
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pane != paneEvents {
		t.Errorf("pane = %v after esc, want the pane it was opened from", m.pane)
	}
}

func TestKey_EscOutOfTheCostPaneEndsItsPollChain(t *testing.T) {
	// The Usage pane's esc arm bumps tickGen on the way out for this reason: a chain
	// left running against a backgrounded pane keeps issuing a request every 20s for
	// the life of the session, and nothing on screen shows the cost of it.
	m := newTestModelOnPane(t, paneEvents)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("$")})
	live := m.costPane.tickGen
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.costTickIsCurrent(live) {
		t.Error("the chain that was live in the pane is still accepted after esc")
	}
}

func TestKey_DollarInsideTheFilterIsLiteralText(t *testing.T) {
	// The events pane's / filter accepts arbitrary text. A bare key check would
	// swallow a literal "$" the user is typing into it and yank them to another
	// pane mid-word. keys.go's column-picker guard is the precedent for scoping a
	// letter key on !m.filtering.
	for _, key := range []string{"$", "C"} {
		t.Run(key, func(t *testing.T) {
			m := newTestModelOnPane(t, paneEvents)
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			if !m.filtering {
				t.Fatal("pressing / did not enter filter mode; test premise is wrong")
			}

			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})

			if m.pane == paneCost {
				t.Errorf("%q opened the Cost pane while the filter was active", key)
			}
			if !strings.Contains(m.filterInput.Value(), key) {
				t.Errorf("filter value = %q, want it to contain the literal %s", m.filterInput.Value(), key)
			}
		})
	}
}

func TestPaneKeys_DocumentsTheCostPane(t *testing.T) {
	// TestPaneKeysCoverAllPanes already fails until paneKeys has an entry, because
	// lastPaneID grows. This asserts the entry says something useful rather than
	// existing to silence that test.
	g, ok := paneKeys[paneCost]
	if !ok {
		t.Fatal("paneKeys has no entry for paneCost")
	}
	var found bool
	for _, k := range g.bindings {
		if strings.Contains(k.keys, "w") || strings.Contains(k.keys, "g") {
			found = true
		}
	}
	if !found {
		t.Error("the Cost pane's help entry documents neither [w]indow nor [g]roup")
	}
}

func TestHelpView_MentionsTheCostPaneKeys(t *testing.T) {
	m := newTestModelOnPane(t, paneCost)
	got := m.helpView()
	for _, want := range []string{"w", "g", "esc"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer for paneCost missing %q: %s", want, got)
		}
	}
	// The bare letters above would match almost any prose, so pin the bracketed forms
	// the rest of this footer uses. A footer that names a key nobody can find in it is
	// the failure TestPaneKeysCoverAllPanes exists for, one layer down.
	for _, want := range []string{"[w]", "[g]", "[esc]"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer for paneCost missing %q: %s", want, got)
		}
	}
}

func TestFooterHints_AdvertiseTheCostKeyWhereItIsPressed(t *testing.T) {
	// A key nobody can discover is a key nobody uses — the same rule
	// TestFooterHintsMentionUsageKey holds `u` to. $ works from every session-view
	// pane, so every one of them has to say so.
	m := newTestModelOnPane(t, paneEvents)
	for _, p := range []paneID{paneSessions, paneEvents, paneDetail} {
		m.pane = p
		if got := m.helpView(); !strings.Contains(got, "[$] cost") {
			t.Errorf("pane %v footer omits the cost key:\n  %s", p, got)
		}
	}
}

func TestPaneView_RendersTheCostPaneWithTheStripAboveIt(t *testing.T) {
	// The strip is on every data pane and the Cost pane is one. This is also the
	// mutation target for Step 5.
	m := newTestModelOnPane(t, paneCost)
	m.spend.snap = pricedSnapshot(t)
	m.costPane.snap = pricedSnapshot(t)

	got := m.paneView()

	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("paneView produced %d lines:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[1], stripLabel) {
		t.Errorf("line 1 is not the spend strip:\n%s", got)
	}
	if !strings.Contains(got, "COST") {
		t.Errorf("the Cost pane body did not render:\n%s", got)
	}
}

func TestPaneView_CostPaneSaysWhenThePollFailed(t *testing.T) {
	// spend.go records what the alternative costs: a broken or absent /v1/usage that
	// renders "" produces no figure, no explanation and no diagnostic on every poll
	// forever, while the row stays reserved. renderCostPane takes only the answer, so
	// the error has to be rendered by the arm that owns the model state.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.err = errors.New("connection refused")

	got := m.paneView()
	if !strings.Contains(got, "connection refused") {
		t.Errorf("a failed poll rendered no diagnostic:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("a failed poll rendered a zero figure:\n%s", got)
	}
}

func TestCostPane_RestoresThePersistedView(t *testing.T) {
	// Both a non-default value and a default-matching one. With only the latter the
	// test passes against a pane that ignores the settings entirely, because the
	// assertion would be satisfied by the default it never read.
	for _, tc := range []struct {
		name   string
		in     CostSettings
		window string
		group  usage.Group
	}{
		{"non-default window", CostSettings{Window: "7d", Group: "endpoint"}, "7d", usage.GroupEndpoint},
		{"default window", CostSettings{Window: "today", Group: "session"}, "today", usage.GroupSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModelOnPane(t, paneEvents)
			Settings.Cost = tc.in

			_ = m.openCostPane()

			if m.costPane.window() != tc.window {
				t.Errorf("window = %q, want the persisted %q", m.costPane.window(), tc.window)
			}
			if m.costPane.group != tc.group {
				t.Errorf("group = %q, want the persisted %q", m.costPane.group, tc.group)
			}
		})
	}
}

func TestCostPane_EmptySettingsUseTheDefaults(t *testing.T) {
	// The deviations-only convention: an empty section is not an empty window. It is
	// what makes a build that changes the default move existing users with it, instead
	// of pinning them to a choice they never made.
	m := newTestModelOnPane(t, paneEvents)
	_ = m.openCostPane()
	if m.costPane.window() == "" {
		t.Error("empty settings produced an empty window rather than the default")
	}
	if m.costPane.group == "" || m.costPane.group == usage.GroupNone {
		t.Errorf("empty settings produced group %q rather than the default", m.costPane.group)
	}
}

func TestCostPane_ADiscardedSettingFallsBackToTheDefault(t *testing.T) {
	// A hand-edited file naming an axis this pane has no series for must not leave the
	// pane on it. costView drops the field; this pins that the pane then uses its
	// default rather than an empty group.
	m := newTestModelOnPane(t, paneEvents)
	Settings.Cost = CostSettings{Window: "6h", Group: "status"}

	_ = m.openCostPane()

	if m.costPane.group != costPaneGroups[0] {
		t.Errorf("group = %q, want the default %q", m.costPane.group, costPaneGroups[0])
	}
	if m.costPane.window() != costPaneWindows[0] {
		t.Errorf("window = %q, want the default %q", m.costPane.window(), costPaneWindows[0])
	}
}

func TestCostPane_CyclingUpdatesTheSettings(t *testing.T) {
	m := newTestModelOnPane(t, paneEvents)
	_ = m.openCostPane()
	before := m.costPane.group
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if m.costPane.group == before {
		t.Fatal("g did not change the group; test premise is wrong")
	}
	if Settings.Cost.Group != string(m.costPane.group) {
		t.Errorf("Settings.Cost.Group = %q, want %q", Settings.Cost.Group, m.costPane.group)
	}

	wBefore := m.costPane.window()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	if m.costPane.window() == wBefore {
		t.Fatal("w did not change the window; test premise is wrong")
	}
	if Settings.Cost.Window != m.costPane.window() {
		t.Errorf("Settings.Cost.Window = %q, want %q", Settings.Cost.Window, m.costPane.window())
	}
}

func TestCostPane_LeavingThePaneWritesTheSettingsOnce(t *testing.T) {
	// Persist on the way OUT, not per keypress, mirroring the column picker: a user
	// cycling three windows to reach the one they want must not produce three writes
	// describing states they rejected. Pinned because it is a POLICY, and the obvious
	// "save where the value changes" refactor would silently reverse it.
	m := newTestModelOnPane(t, paneEvents)
	saved := recordSaves(t, m)
	_ = m.openCostPane()

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if len(*saved) != 0 {
		t.Errorf("cycling wrote %d times before the pane was left", len(*saved))
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(*saved) != 1 {
		t.Fatalf("leaving the pane wrote %d times, want exactly 1", len(*saved))
	}
	if got := (*saved)[0].Cost; got.Window != m.costPane.window() || got.Group != string(m.costPane.group) {
		t.Errorf("wrote %+v, want the view the pane was left on (%q/%q)",
			got, m.costPane.window(), m.costPane.group)
	}
}

func TestRenderCostPane_SumsEveryBucketNotJustTheFirst(t *testing.T) {
	// A RING-backed window (1h) returns one bucket per resolution slice, so a series
	// entry's window total is spread across all of them. Reading Buckets[0] would report
	// the first slice of the window as the whole of it — the bug spend.go's sessionCost
	// comment records, arriving one pane over. Every fixture elsewhere in this file has a
	// single bucket, which is exactly why that mistake would go unnoticed.
	snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 3, PricedRequests: 3, PriceableRequests: 3, CostMicros: 3_000_000},
		Buckets: []usage.Bucket{
			{Series: map[string]usage.Counts{"m": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000}}},
			{Series: map[string]usage.Counts{"m": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000}}},
			{Series: map[string]usage.Counts{"m": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000}}},
		},
	}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "BY MODEL")
	if !strings.Contains(sec, "$3.0000") {
		t.Errorf("the series row does not sum every bucket (want $3.0000):\n%s", sec)
	}
}

func TestRenderCostPane_SharesAreOfThePublishedTotal(t *testing.T) {
	// A percentage of two PUBLISHED figures is presentation. Pinned to the digit,
	// because a share is the one number here a reader will act on without re-deriving,
	// and nothing else in this file would notice a wrong divisor.
	snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 4, PricedRequests: 4, PriceableRequests: 4, CostMicros: 4_000_000,
			InputTokens: 250, CacheReadTokens: 750,
			PresentKinds: kindInput | kindCacheRead},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"dear":  {Requests: 3, PricedRequests: 3, PriceableRequests: 3, CostMicros: 3_000_000},
			"cheap": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000},
		}}},
	}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	by := sectionOf(t, got, "BY MODEL")
	for _, want := range []string{"75.0%", "25.0%"} {
		if !strings.Contains(by, want) {
			t.Errorf("BY MODEL missing the share %q:\n%s", want, by)
		}
	}
	// And the token shares are of the SHOWN TIERS, not of Counts.Tokens — which is not
	// always the sum of the split, so normalising against it would draw shares that do
	// not add up. Tokens is left at zero here, which would make that divisor a no-op.
	tok := sectionOf(t, got, "WHERE IT WENT")
	for _, want := range []string{"75.0%", "25.0%"} {
		if !strings.Contains(tok, want) {
			t.Errorf("WHERE IT WENT missing the share %q:\n%s", want, tok)
		}
	}
}

func TestRenderCostPane_ASurvivingSectionIsWhole(t *testing.T) {
	// The height budget drops sections; it never CLIPS one. A truncated COVERAGE reads
	// as a complete list of gaps, and a truncated breakdown as a complete breakdown —
	// both are wrong answers rather than short ones.
	//
	// Asserted by comparing each surviving section against its unbounded rendering, so
	// any per-line trimming anywhere in the assembler surfaces here rather than looking
	// like a shorter pane.
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000, "b": 900_000, "c": 4})
	tall := renderCostPane(snap, usage.GroupModel, 120, 0)
	headings := []string{"TOTAL", "BY MODEL", costTierHeading, "COVERAGE"}
	for _, h := range []int{16, 18, 20, 24, 30} {
		short := renderCostPane(snap, usage.GroupModel, 120, h)
		var kept int
		for _, heading := range headings {
			if !strings.Contains(short, heading) {
				continue
			}
			kept++
			// Blank lines dropped from both sides: the separators between sections are the
			// FIRST thing the budget gives up, and a section that lost its trailing blank has
			// lost decoration rather than content.
			got, want := withoutBlankLines(sectionOf(t, short, heading)), withoutBlankLines(sectionOf(t, tall, heading))
			if got != want {
				t.Errorf("h=%d section %q was clipped rather than dropped:\ngot:\n%s\nwant:\n%s",
					h, heading, got, want)
			}
		}
		if kept == 0 {
			t.Errorf("h=%d kept no section at all:\n%s", h, short)
		}
	}
}

// withoutBlankLines drops empty lines, so a comparison is about content rather than
// spacing.
func withoutBlankLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestCostPane_InvalidateDisownsAReplyAlreadyInFlight(t *testing.T) {
	// Bumping tickGen alone stops the old chain from SCHEDULING, not the reply already
	// in the air — that reply would still pass the reqSeq guard and land with a fresh
	// timestamp, presenting the previous window's figure as the current one's. spend.go
	// records learning exactly this the hard way, which is why invalidate moves both
	// counters.
	//
	// m.client is nil, so fetchCost issues nothing and bumps nothing: invalidate's own
	// increment is the only thing standing between the old reply and the screen.
	m := &model{}
	inFlight := m.costPane.reqSeq

	_ = m.resumeCostPolling()
	m.applyCostLoaded(costLoadedMsg{req: inFlight, snap: &usage.Snapshot{Window: "1h"}})

	if m.costPane.snap != nil {
		t.Error("a reply issued before the view changed was applied to the new view")
	}
	if !m.costPane.lastFetch.IsZero() {
		t.Error("a discarded reply stamped lastFetch; the pane would report the age of data it threw away")
	}
}

func TestRenderCostPane_FullCoverageRendersNoCoverageSectionAtAll(t *testing.T) {
	// Not merely "no warning text" — no SECTION. A heading with a 0-request line under
	// it is still a permanent block about a problem the deployment does not have, and a
	// reader who learns to skip it will skip the real one too.
	clean := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 318, PriceableRequests: 318}}
	got := renderCostPane(clean, usage.GroupModel, 120, 40)
	if strings.Contains(got, "COVERAGE") {
		t.Errorf("fully priced deployment rendered a COVERAGE section:\n%s", got)
	}
}

func TestRenderCostPane_SanitizesWireDerivedLabels(t *testing.T) {
	// A series key and an UnpricedBy key are both "<endpoint> <model>", and the model
	// half comes from the request body's `model` field — chosen by the workload and
	// recorded verbatim by the parser. Written raw to a TTY, an escape sequence can
	// reposition the cursor, recolour the pane, or erase the very row being reported; a
	// newline alone breaks the table apart. CWE-150, the same rule renderCostSummary
	// already holds to.
	for name, hostile := range map[string]string{
		"ANSI colour":     "gw \x1b[31mclaude-opus-5",
		"cursor move":     "gw \x1b[2Aclaude",
		"newline":         "gw claude\nFAKE TOTAL: $9.9999",
		"carriage return": "gw claude\rerased",
		"NUL":             "gw claude\x00",
		"DEL":             "gw claude\x7f",
	} {
		t.Run(name, func(t *testing.T) {
			snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
				Totals: usage.Counts{Requests: 10, PriceableRequests: 10, PricedRequests: 4, CostMicros: 1_000_000},
				Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
					hostile: {Requests: 4, PricedRequests: 4, PriceableRequests: 4, CostMicros: 1_000_000},
				}}},
				UnpricedBy: map[string]int64{hostile: 6},
			}
			got := renderCostPane(snap, usage.GroupModel, 200, 40)
			for _, bad := range []string{"\x1b", "\r", "\x00", "\x7f"} {
				if strings.Contains(got, bad) {
					t.Errorf("rendered output still carries %q:\n%q", bad, got)
				}
			}
			// A newline cannot be caught by scanning for the byte — the pane is multi-line by
			// design — so the assertion is the ROW COUNT against a benign control of the same
			// length. A smuggled newline would split one row into two and shift everything
			// below it; a control character rendered as U+FFFD leaves the count alone.
			benign := strings.Map(func(r rune) rune {
				if r < 0x20 || r == 0x7f {
					return 'x'
				}
				return r
			}, hostile)
			ctrl := renderCostPane(&usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
				Totals: usage.Counts{Requests: 10, PriceableRequests: 10, PricedRequests: 4, CostMicros: 1_000_000},
				Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
					benign: {Requests: 4, PricedRequests: 4, PriceableRequests: 4, CostMicros: 1_000_000},
				}}},
				UnpricedBy: map[string]int64{benign: 6},
			}, usage.GroupModel, 200, 40)
			if g, w := len(strings.Split(got, "\n")), len(strings.Split(ctrl, "\n")); g != w {
				t.Errorf("hostile label rendered %d rows, benign control %d — a control character moved the layout:\n%s", g, w, got)
			}
			// The label is still recognisable, so a real gap remains actionable.
			if !strings.Contains(got, "claude") {
				t.Errorf("sanitizing destroyed the label:\n%s", got)
			}
		})
	}
}

func TestRenderCostPane_FitsEveryWidthWithEveryCaveatOn(t *testing.T) {
	// The caveat prose is the longest text the pane emits, and the fixtures elsewhere in
	// this file switch most of it off — so a renderer that never wrapped would still fit
	// them. This one turns every line on at once, and gives snap.Window a long value.
	//
	// A long window label is not contrived: the field is free-form on the wire, a
	// ring-answered request comes back as time.Duration.String() ("6h0m0s"), and a proxy
	// is entitled to name a span rather than measure it. The header carries no figure, so
	// it may be truncated — but it may not overflow.
	snap := &usage.Snapshot{
		Window: "since-the-last-deploy-rollout-window-label", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{
			Requests: 900, PriceableRequests: 318, PricedRequests: 300,
			IncompleteRequests: 27, CostMicros: 4_170_000, Tokens: 216_208_900,
			InputTokens: 308_900, CacheReadTokens: 215_900_000, CacheWriteTokens: 1_200_000,
			OutputTokens: 3_100_000, ReasoningTokens: 410_000,
			PresentKinds: kindInput | kindCacheRead | kindCacheWrite | kindOutput | kindReasoning,
		},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"a-model-with-a-genuinely-long-name": {Requests: 300, PricedRequests: 300, PriceableRequests: 300, CostMicros: 4_170_000},
		}}},
		UnpricedBy: map[string]int64{"gateway.internal.example.test a-model-with-a-genuinely-long-name": 18},
		PricedBy:   map[string]int64{"authoritative": 200, "bundled": 60, "configured": 40},
	}
	for _, w := range []int{200, 120, 100, 80, 64, 48, 40} {
		for _, h := range []int{60, 40, 24, 20, 16} {
			got := renderCostPane(snap, usage.GroupModel, w, h)
			for i, line := range strings.Split(got, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("w=%d h=%d line %d is %d columns: %q", w, h, i, lw, line)
				}
			}
		}
	}
}

func TestRenderCostPane_RowsInASectionShareTheirColumns(t *testing.T) {
	// Every row of a list section is padded to the same column layout, so the figures sit
	// under one another. That is the whole reason a share column exists: a reader compares
	// them by eye, and a ragged column defeats it. Wide characters are the case that
	// breaks a byte- or rune-counted pad, which is why one label here is full-width.
	//
	// The WIDEST label is deliberately the ASCII one. With the wide label widest, its byte
	// length and its cell count both exceed the column and a byte-counted pad happens to
	// produce the same zero — the two spellings agree and the bug hides. It only shows
	// when a wide label has to be padded OUT to a wider column.
	snap := pricedSnapshotWithSeries(t, map[string]int64{
		"a-really-long-ascii-model-name": 3_820_000,
		"日本語モデル":                         350_000,
		"another-name":                   50,
	})
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 60), "BY MODEL")
	var width = -1
	for _, l := range strings.Split(sec, "\n") {
		// Rows only: the heading and the "+N more" prose are not part of the column grid.
		if !strings.HasPrefix(l, costIndent) || !strings.Contains(l, "$") {
			continue
		}
		if width < 0 {
			width = lipgloss.Width(l)
			continue
		}
		if got := lipgloss.Width(l); got != width {
			t.Errorf("row is %d columns wide, the first row was %d — the columns do not line up:\n%s",
				got, width, sec)
		}
	}
	if width < 0 {
		t.Fatalf("no rows matched; test premise is wrong:\n%s", sec)
	}
}

func TestPaneView_CostPaneDistinguishesLoadingFromNoData(t *testing.T) {
	// Two states with an identical (nil) snapshot and opposite meanings: one resolves on
	// the reply already in flight, the other never will. renderUsage draws the same
	// distinction, and collapsing it here would make a dead poll chain look like a quiet
	// window.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.loading = true
	if got := m.paneView(); !strings.Contains(got, "loading") {
		t.Errorf("a pane with a request in flight does not say so:\n%s", got)
	}

	m.costPane.loading = false
	got := m.paneView()
	if strings.Contains(got, "loading") {
		t.Errorf("a pane with nothing in flight claims to be loading:\n%s", got)
	}
	if !strings.Contains(got, "no data") {
		t.Errorf("a pane with no answer and nothing in flight says nothing:\n%s", got)
	}
}

func TestPaneView_CostPaneReportsTheAgeOfItsFigure(t *testing.T) {
	// The pane refreshes on a timer with no other sign of life, so a figure carrying no
	// age cannot be told from a frozen one — a chain that silently died looks exactly like
	// a quiet hour.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.snap = pricedSnapshot(t)
	m.costPane.lastFetch = time.Now().Add(-4 * time.Second)

	got := m.paneView()
	if !strings.Contains(got, "updated 4s ago") {
		t.Errorf("the pane does not report how old its figure is:\n%s", got)
	}
	if !strings.Contains(got, "every 20s") {
		t.Errorf("the pane does not say how often it refreshes:\n%s", got)
	}
}

func TestPaneView_CostPaneDropsTheAgeLineBeforeASection(t *testing.T) {
	// The age is worth strictly less than any section, so it takes a row only when one is
	// spare — the same priority the height budget gives the separators. A terminal that
	// cannot fit both must show the money.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.snap = pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000})
	m.costPane.lastFetch = time.Now().Add(-4 * time.Second)

	m.width, m.height = 120, 40
	m.layout()
	tall := m.paneView()
	if !strings.Contains(tall, "updated 4s ago") {
		t.Fatalf("the age line is absent with room to spare; test premise is wrong:\n%s", tall)
	}

	// Squeeze the body to exactly what the sections need, leaving no spare row.
	m.costPane.lastFetch = time.Now().Add(-4 * time.Second)
	m.bodyHeight = len(strings.Split(renderCostPane(m.costPane.snap, m.costPane.group, m.width, 0), "\n"))
	if got := m.paneView(); strings.Contains(got, "updated") {
		t.Errorf("the age line took a row the sections needed:\n%s", got)
	}
}

func TestCostPane_AnAcceptedReplyStampsItsAgeAndClearsLoading(t *testing.T) {
	// The other half of "lastFetch moves only on an ACCEPTED reply": the stale-reply tests
	// pin that a discarded one leaves it alone, and without this one a renderer could
	// report an age nothing ever set — which reads as a figure that never refreshes.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.loading = true

	m.applyCostLoaded(costLoadedMsg{req: m.costPane.reqSeq, snap: pricedSnapshot(t)})

	if m.costPane.lastFetch.IsZero() {
		t.Error("an accepted reply did not stamp lastFetch")
	}
	if m.costPane.loading {
		t.Error("an accepted reply left the pane claiming a request is still in flight")
	}
	if got := m.paneView(); !strings.Contains(got, "updated") {
		t.Errorf("the pane reports no age after a reply landed:\n%s", got)
	}
}
