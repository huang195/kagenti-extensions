package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// costPollInterval is how often the pane refetches while it is open.
//
// The same 20s as the Usage pane and the spend strip's window chain, and for the
// same reason: the aggregator buckets at one minute, so a faster poll cannot reveal
// anything the server has not already folded. The chain is armed only while the
// pane is focused, so a backgrounded Cost pane costs nothing.
const costPollInterval = 20 * time.Second

// costPaneWindows are the spans [w] cycles, as the SERVER names them.
//
// Strings rather than the Usage pane's time.Duration pairs, because two of the
// three are not lengths: "today" is a boundary and "7d" is longer than the ring
// retains, so both are answered from the durable cost ledger and neither can be
// expressed as a duration (see usage.ParseWindowSpec). "1h" is the rolling ring
// window, kept because it is the only one that answers "what is it costing me right
// now" on a proxy with no ledger.
//
// "today" is first, so it is the default: the question this pane exists for is
// "what did today cost", and a rolling hour is the follow-up.
var costPaneWindows = []string{usage.WindowToday, usage.Window7d, "1h"}

// costPaneGroups are the breakdown axes [g] cycles.
//
// No GroupNone: an ungrouped view of this pane is the spend strip, which is already
// on screen one row above it, so a cycle position that showed nothing new would
// just be a way to lose the breakdown.
//
// No agent axis, because there is none. Client identity is not implemented, so
// usage.Group has no GroupAgent constant and the aggregator keeps no per-agent
// series; declaring one here would put a heading over an empty section forever.
// Session is the closest thing available and is already in the cycle — with the
// caveat usage.GroupSession's own doc records, that while several concurrent agents
// share one session id their spend lands in one entry.
var costPaneGroups = []usage.Group{usage.GroupModel, usage.GroupEndpoint, usage.GroupSession}

// costPaneState is the Cost pane's view state and its poll chain.
//
// This is the THIRD copy of the reqSeq/tickGen guard pair in this package —
// usageState and spendState are the other two — and the triplication is deliberate
// rather than overlooked. Extracting a shared poller would mean refactoring the
// Usage pane's polling in the same commit that adds a pane, so any Usage-pane
// regression would land attributed to the new pane, in the last commit of a long
// PR. The extraction is worth doing; it is not worth doing here.
//
// Deliberately NOT m.usage or m.spend either. m.usage carries a user-chosen metric
// and an optional single-session scope, and m.spend is all-sessions chrome on a
// fixed rolling window; this pane needs its own window and its own breakdown axis,
// and sharing either state would make selecting one view silently move the other.
type costPaneState struct {
	snap      *usage.Snapshot
	err       error
	loading   bool
	lastFetch time.Time

	// windowIdx indexes costPaneWindows; group is the active breakdown axis.
	windowIdx int
	group     usage.Group

	// returnPane is where esc goes back to, kept here rather than in
	// model.previousPane for the reason usageState.returnPane records: that field is
	// shared with the catalog overlay, so opening the catalog from this pane would
	// clobber it and esc would land somewhere the user never came from.
	returnPane paneID

	// reqSeq is the id of the most recently ISSUED request; a reply carrying a
	// different id is stale and dropped. An id rather than a comparison of the
	// request's fields, for the reason usageLoadedMsg.req records: comparing fields
	// means every future view option has to be added to the comparison or it
	// silently stops being covered, while an id cannot be partially right.
	reqSeq uint64
	// tickGen identifies the current polling chain. See usageState.tickGen: a quick
	// exit and re-entry left two chains alive, each rescheduling the other's
	// successor and doubling the request rate for the life of the session.
	tickGen uint64
}

// window returns the span to request, as the server names it. Named to match
// usageState.window() so the third copy of this shape is recognisably the same
// thing rather than a new invention.
func (s *costPaneState) window() string {
	return costPaneWindows[s.windowIdx%len(costPaneWindows)]
}

func (s *costPaneState) cycleWindow() {
	s.windowIdx = (s.windowIdx + 1) % len(costPaneWindows)
}

// cycleGroup advances [g] through costPaneGroups, never landing on GroupNone.
//
// The zero value "" is treated as "before the first entry" so the first press
// advances to the second axis rather than re-selecting the default — the bug
// usageState.cycleGroup's comment records, where matching only GroupNone sent a
// freshly opened pane to the default arm and made the first press a no-op.
func (s *costPaneState) cycleGroup() {
	for i, g := range costPaneGroups {
		if g == s.group {
			s.group = costPaneGroups[(i+1)%len(costPaneGroups)]
			return
		}
	}
	// Unknown or unset (including "" and GroupNone): start at the head of the cycle.
	s.group = costPaneGroups[0]
}

// invalidate drops the data this state describes and disowns anything in flight.
//
// Both counters move, and clearing snap matters as much as either. spend.go learned
// the whole rule the hard way: bumping tickGen alone stops the old chain from
// scheduling but not the reply already in the air, so an in-flight answer still
// passed the reqSeq guard and landed with a fresh timestamp — presenting the
// previous window's figure as the current one's. And leaving snap in place draws the
// previous window's breakdown under the NEW heading until the reply lands, which is
// the same wrong-heading failure arriving from the other direction.
func (s *costPaneState) invalidate() {
	s.snap = nil
	s.err = nil
	s.lastFetch = time.Time{}
	s.loading = true
	s.reqSeq++
	s.tickGen++
}

// costLoadedMsg carries a fetched snapshot back to Update.
type costLoadedMsg struct {
	snap *usage.Snapshot
	req  uint64
	err  error
}

// costTickMsg fires the periodic refetch. gen ties it to the chain that scheduled
// it, so a tick from a superseded chain is ignored.
type costTickMsg struct{ gen uint64 }

// applySettings puts the pane on the remembered view, or on its defaults.
//
// DEVIATIONS ONLY, matching how EventSettings records columns: an empty field means
// "whatever this build defaults to", never "no window". So a release that reorders
// costPaneWindows moves every user who never pressed [w] along with it, instead of
// pinning them to a position they never chose.
//
// Both fields are reset first rather than only assigned when present. Without that, a
// second visit would inherit the previous visit's axis whenever the file had nothing
// to say about it — which is the same class of bug as a stale snapshot under a fresh
// heading, arriving through the settings instead of the wire.
//
// A value costView already discarded arrives here as "", and lands on the default.
func (s *costPaneState) applySettings(cs CostSettings) {
	s.windowIdx = 0
	s.group = costPaneGroups[0]
	if cs.Window != "" {
		for i, w := range costPaneWindows {
			if w == cs.Window {
				s.windowIdx = i
				break
			}
		}
	}
	if cs.Group != "" {
		for _, g := range costPaneGroups {
			if string(g) == cs.Group {
				s.group = g
				break
			}
		}
	}
}

// openCostPane enters the pane, remembering where to return to.
//
// The remembered view is applied on the way IN rather than at startup: the pane may
// never be opened in a session, and reading the settings here means a hand-edited file
// takes effect on the next open rather than only on the next launch.
func (m *model) openCostPane() tea.Cmd {
	m.costPane.returnPane = m.pane
	m.pane = paneCost
	m.costPane.applySettings(Settings.costView())
	// Shares resumeCostPolling with the catalog-return path so the two entry points
	// cannot drift on how a chain is started or how the previous one is invalidated.
	return m.resumeCostPolling()
}

// resumeCostPolling starts exactly one chain, invalidating whatever preceded it.
//
// invalidate() rather than a bare tickGen++, for the reason startSpendPolling gives:
// any path that starts a chain then gets the in-flight-reply guarantee without
// having to remember it. Fetching immediately as well means the pane is current on
// arrival rather than blank for up to costPollInterval.
func (m *model) resumeCostPolling() tea.Cmd {
	m.costPane.invalidate()
	return tea.Batch(m.fetchCost(), costTick(m.costPane.tickGen))
}

// beginCostFetch invalidates what is on screen and returns the command for a fresh
// request. Every view-option change goes through it.
//
// It does NOT bump tickGen — that would orphan the live chain and leave the pane
// with no scheduled successor. Only the request sequence moves, which is what
// discards the reply to the window the user just cycled away from.
func (m *model) beginCostFetch() tea.Cmd {
	m.costPane.snap = nil
	m.costPane.err = nil
	m.costPane.loading = true
	m.costPane.reqSeq++
	return m.fetchCost()
}

// costTickIsCurrent reports whether a tick belongs to the live chain.
func (m *model) costTickIsCurrent(gen uint64) bool { return gen == m.costPane.tickGen }

// applyCostLoaded stores a reply unless it is stale.
//
// lastFetch moves only on an accepted reply: advancing it for a discarded one would
// have the pane report the age of data it just threw away.
func (m *model) applyCostLoaded(msg costLoadedMsg) {
	if msg.req != m.costPane.reqSeq {
		return
	}
	m.costPane.loading = false
	if msg.err != nil {
		// snap is cleared rather than left in place: once the fetch failed we do not
		// know the current spend, and continuing to draw the last figure would present
		// a stale number as a current one. renderCostPane says so instead.
		m.costPane.snap = nil
		m.costPane.err = msg.err
		return
	}
	m.costPane.err = nil
	m.costPane.snap = msg.snap
	m.costPane.lastFetch = time.Now()
}

// costTick schedules the next poll for the given generation.
func costTick(gen uint64) tea.Cmd {
	return tea.Tick(costPollInterval, func(time.Time) tea.Msg { return costTickMsg{gen: gen} })
}

// fetchCost requests the pane's snapshot off the render loop.
//
// GetUsageWindow rather than GetUsage: two of the three windows are symbolic names a
// time.Duration cannot express. Resolution 0 omits the parameter — this pane reads
// Totals and the window-wide Series, never a per-bucket series, so asking for a
// resolution would only constrain how the server slices data nothing here draws.
//
// Returns nil before a client exists (picker mode), which keeps the tick chain alive
// without issuing a request — the same shape fetchSpend uses.
func (m *model) fetchCost() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	window := m.costPane.window()
	group := m.costPane.group
	// Every issued request gets its own id, including the ones the 20s tick issues,
	// matching fetchSpend. Reusing the previous id would let a slow reply land AFTER a
	// faster newer one and overwrite it with an older figure carrying a fresh timestamp.
	// The doubled increment on the paths that also go through beginCostFetch is
	// harmless: the sequence only has to be monotonic.
	m.costPane.reqSeq++
	req := m.costPane.reqSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, window, 0, "", group)
		return costLoadedMsg{snap: snap, req: req, err: err}
	}
}

// Token-kind bits, matching pipeline.InferenceExtension.PresentKinds and
// parsercommon.Kind (Input=1, CacheRead=2, CacheWrite=4, Output=8, Reasoning=16).
//
// Declared here rather than imported for the reason cmd_cost.go gives for its own
// copy: abctl decodes a wire shape, and the JSON contract is what pins the layout —
// parsercommon lives under authlib/plugins/internal and is not importable from here
// at all.
const (
	kindInput uint8 = 1 << iota
	kindCacheRead
	kindCacheWrite
	kindOutput
	kindReasoning
)

// Layout constants. costIndent is what makes a line a BODY line rather than a
// heading, which is the contract sectionOf reads in the tests: every heading is
// unindented, every line under it is indented.
const (
	costIndent = "  "
	costColGap = "  "
	// costMaxBarWidth caps the share bars so a 200-column terminal does not render a
	// 180-cell block. Bars encode a proportion; past a point more cells add no
	// precision a reader can use.
	costMaxBarWidth = 28
	// costMinBarWidth is the width below which a bar stops being readable and is
	// dropped entirely. Four cells cannot distinguish 10% from 20%.
	costMinBarWidth = 6
	// costMaxSeriesRows and costMaxGapRows cap the two list sections. Both disclose
	// what they elided, because a silently truncated list reads as a complete one.
	costMaxSeriesRows = 8
	costMaxGapRows    = 5
)

// truncCells shortens s to at most n DISPLAY CELLS, marking the cut with an
// ellipsis.
//
// Cells, not runes and not bytes. footer.go:88-92 records what the difference costs:
// a budget computed in display columns and then sliced by rune index rendered 55
// columns for a 40-column budget on wide characters. The package's trunc (rune
// indexed) and truncStr (byte indexed) are both display-cell-unaware, so neither is
// usable anywhere in this file — and model names are workload-chosen, arriving
// verbatim from the request body, so full-width input is not hypothetical.
//
// Only ever applied to LABELS. A figure is never truncated: "$12.5" for a $12.5000
// session is not a shortened number, it is a different and smaller one.
func truncCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	budget := n - 1 // the ellipsis costs one cell
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > budget {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// wrapCells word-wraps prose to n display cells per line.
//
// Prose only — the caveat sentences, never a figure. A word wider than the budget is
// truncated rather than hard-split, because the alternative on a 40-column terminal
// is a model name broken across two lines and unrecognisable in both.
func wrapCells(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = truncCells(word, n)
		case lipgloss.Width(line)+1+lipgloss.Width(word) <= n:
			line += " " + word
		default:
			out = append(out, line)
			line = truncCells(word, n)
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// costRow is one line of a list section: a wire-derived label, an optional share to
// draw as a bar, and the figures that must never be clipped.
//
// hasBar is separate from frac == 0 on purpose. An unpriced series has an UNKNOWN
// share, not a zero one, and an empty bar beside a blank right column would read as
// "this cost nothing" — which is the single claim this pane is forbidden to make.
type costRow struct {
	label  string
	right  string
	frac   float64
	hasBar bool
}

// renderCostRows lays rows out into aligned lines that fit width.
//
// Three columns — label, bar, figures — with the bar taking whatever is left over
// and being dropped entirely below costMinBarWidth. Dropping the bar rather than the
// figures is the priority order the whole pane holds to: the bar is a reading aid for
// a number that is already on the line, so losing it costs nothing but comfort.
//
// The label column is truncated to fit; the figure column never is. Returns nil when
// even a one-cell label cannot sit beside the figures, which the caller renders as a
// dropped section rather than as a row with no name on it.
//
// Every measurement is lipgloss.Width. See truncCells for what the alternative costs.
func renderCostRows(rows []costRow, width int) []string {
	if len(rows) == 0 {
		return nil
	}
	rightW, labelW := 0, 0
	for _, r := range rows {
		if w := lipgloss.Width(r.right); w > rightW {
			rightW = w
		}
		if w := lipgloss.Width(r.label); w > labelW {
			labelW = w
		}
	}
	fixed := lipgloss.Width(costIndent) + lipgloss.Width(costColGap) + rightW
	if avail := width - fixed; labelW > avail {
		labelW = avail
	}
	if labelW < 1 {
		return nil
	}
	// Whatever is left after the label and the figures, capped. Computed once for the
	// whole section so the bars share a baseline — a per-row width would make two rows
	// with the same share draw different lengths.
	barW := width - fixed - labelW - lipgloss.Width(costColGap)
	if barW > costMaxBarWidth {
		barW = costMaxBarWidth
	}
	if barW < costMinBarWidth {
		barW = 0
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		lbl := truncCells(r.label, labelW)
		var b strings.Builder
		b.WriteString(costIndent)
		b.WriteString(lbl)
		b.WriteString(strings.Repeat(" ", labelW-lipgloss.Width(lbl)))
		if barW > 0 {
			b.WriteString(costColGap)
			b.WriteString(costBar(r.frac, r.hasBar, barW))
		}
		b.WriteString(costColGap)
		b.WriteString(strings.Repeat(" ", rightW-lipgloss.Width(r.right)))
		b.WriteString(r.right)
		out = append(out, b.String())
	}
	return out
}

// costBar draws frac of n cells, padded to n so the column after it stays aligned.
//
// An absent share draws BLANK, not an empty bar at the left edge: the two look
// identical at 0% and mean opposite things, so the row's figure column carries the
// distinction and the bar declines to guess. n is a cell count and the block glyph is
// one cell wide, so the arithmetic and lipgloss.Width agree here.
func costBar(frac float64, has bool, n int) string {
	if !has || n <= 0 {
		return strings.Repeat(" ", max(n, 0))
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(n) + 0.5)
	// A non-zero share always gets at least one cell. Rounding 0.4% of 28 cells to
	// nothing renders a present-but-tiny series identically to an absent one.
	if filled == 0 && frac > 0 {
		filled = 1
	}
	return strings.Repeat("█", filled) + strings.Repeat(" ", n-filled)
}

// costSection is one stacked block: an unindented heading and its indented body.
//
// A struct rather than a []string so the height budget can drop a WHOLE section
// rather than trailing lines of one. Half a section is worse than none: a truncated
// COVERAGE reads as a complete list of gaps, and a truncated figure reads as a
// smaller number.
type costSection struct {
	heading string
	lines   []string
}

// costMoney renders micros as dollars to four places.
//
// Four, not two: a single turn can cost a fraction of a cent, and rounding it to
// $0.00 would render a real cost as free — the one thing this pane must never do.
// Division by 1e6 is the only arithmetic anywhere in this file that touches money:
// the server publishes an integer count of millionths, and turning that into a
// display string is presentation. Multiplying tokens by a rate would be pricing, and
// pricing does not happen here.
func costMoney(micros int64) string { return fmt.Sprintf("$%.4f", float64(micros)/1e6) }

// costShare renders one published figure as a percentage of another.
//
// Legitimate for the same reason costMoney is: both operands come off the wire, and a
// ratio of two published numbers is a way of displaying them, not a new measurement.
// The moment a percentage is taken of something the server did not publish it stops
// being presentation.
func costShare(part, whole int64) string {
	if whole <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1f%%", 100*float64(part)/float64(whole))
}

// costTotalSection is the answer to the pane's own question, with every caveat that
// applies to it and none that does not.
//
// It carries the coverage RATIO as well as the figure, while the COVERAGE section
// below names the pairs. That split is deliberate: the ratio is the part that must
// survive the height budget, because a partial total presented without it reads as a
// complete one, and COVERAGE is the first section a short terminal drops.
func costTotalSection(snap *usage.Snapshot, width int) costSection {
	sec := costSection{heading: "TOTAL"}
	body := width - lipgloss.Width(costIndent)
	add := func(prose string) {
		for _, l := range wrapCells(prose, body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
	}
	if !snap.Priced {
		// Never "$0.0000". A zero cost and an unknown cost are different answers and
		// only one of them means the traffic was free.
		add("cost unavailable — nothing in this window carried a cost")
		if gap := snap.Totals.PriceableRequests; gap > 0 {
			add(fmt.Sprintf("%s priceable request%s went through, none of them priced",
				formatCount(int(gap)), plural(int(gap))))
		}
		return sec
	}
	// The figure on its own line so the height budget can never cut it, and the
	// provenance beside it because $12.40 from a gateway's own numbers and $12.40
	// modelled from a shipped vendor list are not equally trustworthy. provenanceNote
	// is silent for a wholly authoritative total, which is the baseline a reader
	// already assumes.
	//
	// Beside it only WHEN IT FITS. A mixed-provenance total on a narrow terminal renders
	// "[authoritative 200, bundled 60, configured 40]" — 46 columns of annotation next to
	// a 7-column figure — so on anything narrower the note moves to its own wrapped line
	// below. It is never truncated onto the figure's line, because clipping there would
	// eat the digits: the figure is the answer, the annotation qualifies it.
	figure := costIndent + costMoney(snap.Totals.CostMicros)
	prov := provenanceNote(snap.PricedBy)
	if lipgloss.Width(figure+prov) <= width {
		sec.lines = append(sec.lines, figure+prov)
	} else {
		sec.lines = append(sec.lines, figure)
		add(strings.TrimSpace(prov))
	}

	priced, priceable := snap.Totals.PricedRequests, snap.Totals.PriceableRequests
	// Against PRICEABLE requests, never all of them. Requests counts every proxied
	// response — MCP tool calls, health checks — while only inference can ever be
	// priced, so the wrong denominator left a correctly configured deployment reading
	// a permanent warning with nothing to act on.
	if priceable > 0 && priced < priceable {
		add(fmt.Sprintf("covers %s of %s priceable requests — the rest carry no figure",
			formatCount(int(priced)), formatCount(int(priceable))))
	}
	// IncompleteRequests is a SUBSET of PricedRequests: their dollars are in the total
	// and the disclosure rides alongside. Rendered only when it is non-zero, which is
	// the other half of the rule — a permanent caveat with nothing to act on is what
	// teaches an operator to ignore the one that matters.
	if inc := snap.Totals.IncompleteRequests; inc > 0 {
		add(fmt.Sprintf("%s of %s priced figures are lower bounds — a stream ended before "+
			"its output count arrived, so the real total is higher",
			formatCount(int(inc)), formatCount(int(priced))))
	}
	return sec
}

// costSeries sums a snapshot's per-label counts across the WHOLE window.
//
// Every bucket, not Buckets[0]. This pane asks for a single-bucket resolution but the
// server negotiates it (see Snapshot.BucketSeconds), and a ledger-backed window
// returns exactly one bucket while a ring-backed one returns many — so reading the
// first bucket would report the first slice of the window as the whole of it.
//
// An empty label is dropped. The aggregator already refuses to key a series on one,
// but a bucket that arrived from an older producer could carry it, and a row with no
// name against a real dollar figure is unactionable.
func costSeries(snap *usage.Snapshot) map[string]usage.Counts {
	out := map[string]usage.Counts{}
	for _, b := range snap.Buckets {
		for label, c := range b.Series {
			if label == "" {
				continue
			}
			cur := out[label]
			cur.Add(c)
			out[label] = cur
		}
	}
	return out
}

// costBreakdownSection is where the money went, by the selected axis.
//
// Ordered by cost descending, ties broken on the label: map iteration is
// nondeterministic, and a table that reshuffles between 20-second polls is unreadable
// even when every number in it is right.
//
// Series with no priced request sort LAST and render "cost unavailable" rather than a
// figure. Their cost is unknown, not zero, and Snapshot.Priced is window-wide — so a
// snapshot that priced another model's traffic says priced:true, and trusting that
// flag per row would print $0.0000 against a model whose cost nobody knows.
func costBreakdownSection(snap *usage.Snapshot, group usage.Group, width int) costSection {
	// The requested axis rather than snap.Group: beginCostFetch clears the snapshot on
	// every view change, so the two cannot disagree, and snap.Group can echo the
	// GroupMethod alias — which would put "BY METHOD" over what is really the model
	// series (see usage.GroupModel's doc for why that name was wrong from the start).
	sec := costSection{heading: "BY " + strings.ToUpper(string(group))}
	body := width - lipgloss.Width(costIndent)
	series := costSeries(snap)
	if len(series) == 0 {
		// Not silence. group=session on a ledger-backed window carries no series at all —
		// a per-minute row holds no session id — and an empty block under a "BY SESSION"
		// heading reads as "none of this cost anything".
		for _, l := range wrapCells(fmt.Sprintf(
			"no %s breakdown for this window", group), body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
		return sec
	}
	labels := make([]string, 0, len(series))
	for label := range series {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		a, b := series[labels[i]], series[labels[j]]
		// Priced before unpriced: a known figure outranks an unknown one whatever their
		// notional order, and comparing a real total against a placeholder zero would
		// interleave the two.
		if (a.PricedRequests > 0) != (b.PricedRequests > 0) {
			return a.PricedRequests > 0
		}
		if a.CostMicros != b.CostMicros {
			return a.CostMicros > b.CostMicros
		}
		return labels[i] < labels[j]
	})
	shown := labels
	if len(shown) > costMaxSeriesRows {
		shown = shown[:costMaxSeriesRows]
	}
	rows := make([]costRow, 0, len(shown))
	for _, label := range shown {
		c := series[label]
		// sanitizeLabel because these keys are wire-derived: the model half comes from the
		// request body's `model` field, chosen by the workload and recorded verbatim by the
		// parser, so writing it raw to a TTY lets an escape sequence recolour the pane or
		// erase the very row it is reporting. CWE-150.
		row := costRow{label: sanitizeLabel(label)}
		if c.PricedRequests == 0 {
			row.right = "cost unavailable"
			rows = append(rows, row)
			continue
		}
		row.right = costMoney(c.CostMicros)
		if pct := costShare(c.CostMicros, snap.Totals.CostMicros); pct != "" {
			// Right-padded to a fixed width so the shares line up on the percent sign. Without
			// it "91.6%" and "0.2%" sit at different columns and the eye cannot compare them,
			// which is the one thing a share column is for.
			row.right += costColGap + fmt.Sprintf("%6s", pct)
			row.frac = float64(c.CostMicros) / float64(snap.Totals.CostMicros)
			row.hasBar = true
		}
		rows = append(rows, row)
	}
	sec.lines = renderCostRows(rows, width)
	if rest := len(labels) - len(shown); rest > 0 {
		// Disclosed, not silently dropped: a truncated list reads as a complete one, and
		// the elided rows are the cheap ones only because the sort put them there.
		for _, l := range wrapCells(fmt.Sprintf("+%d more, each smaller than the last row", rest), body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
	}
	return sec
}

// costTierHeading names the token section, and says in the heading what it reports.
//
// "tokens, not dollars" is in the heading rather than in a footnote because the
// footnote is the part a reader skips. See costTokenSection for why there is no
// dollar figure to put here.
const costTierHeading = "WHERE IT WENT (tokens, not dollars)"

// costTier is one billed token kind: its declaration bit, its label, and its count.
type costTier struct {
	bit   uint8
	label string
	count int64
}

// costTokenSection reports the four-way token split as shares.
//
// TOKENS, NOT DOLLARS, and that is the whole design of this section rather than a
// shortcut. usage.Counts aggregates the token split — real, exact, already on the
// wire — and aggregates NO per-tier dollars; nothing does. costevent.Event carries
// PromptUSD and OutputUSD, but PromptUSD's own doc is explicit that when the total
// came from a gateway header "PromptUSD + anything is not a total of anything", and
// OutputUSD warns it is not CostUSD minus PromptUSD. So a four-tier dollar breakdown
// summing to the total would be an invented number, and a prompt-versus-output split
// would fail to reconcile precisely on the deployments whose figures are most
// trustworthy — the ones with a gateway header.
//
// Volume still answers the question a reader came with, because the tiers price
// roughly 1x / 0.1x / 1.25x / output: the largest share is the first place to look.
// If a dollar breakdown is wanted, the honest route is to aggregate the per-tier
// figures into Counts AND disclose that they do not reconcile with an authoritative
// total. That is a separate change with its own argument.
//
// AVOIDED is deliberately absent, which is where a reader would expect it. The
// tool-prune savings container exists upstream but nothing aggregates it into Counts,
// so there is no window-scoped figure: a section reading a per-request field would
// show one request's saving under a window heading.
func costTokenSection(snap *usage.Snapshot, width int) costSection {
	sec := costSection{heading: costTierHeading}
	body := width - lipgloss.Width(costIndent)
	add := func(prose string) {
		for _, l := range wrapCells(prose, body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
	}
	t := snap.Totals
	// A tier is shown when the provider DECLARED it or when it carries a count.
	//
	// Declared-and-zero is a real measurement worth a row: "this traffic wrote no
	// cache" is an answer. Undeclared-and-zero is not — it means nothing here reports
	// cache writes, and a 0% row would assert a measurement nobody made. That is
	// exactly the distinction PresentKinds exists for.
	//
	// Counted-but-undeclared still shows, matching cmd_cost.go's tokenSplit: it comes
	// from a producer predating PresentKinds, where the value is the only evidence
	// there is, and dropping it would hide a real number.
	var tiers []costTier
	for _, c := range []costTier{
		{kindInput, "input", t.InputTokens},
		{kindCacheRead, "cache-read", t.CacheReadTokens},
		{kindCacheWrite, "cache-write", t.CacheWriteTokens},
		{kindOutput, "output", t.OutputTokens},
	} {
		if t.PresentKinds&c.bit == 0 && c.count == 0 {
			continue
		}
		tiers = append(tiers, c)
	}
	if len(tiers) == 0 {
		if t.Tokens > 0 {
			// The gateway-reports-a-total case. parsercommon.Fill prefers the provider's own
			// total_tokens when one was reported, so Tokens is non-zero with every split field
			// at zero — a different answer from a split that was genuinely all zeros.
			add(fmt.Sprintf("%s tokens, but no breakdown: this provider reported only a total",
				formatCount(int(t.Tokens))))
		} else {
			add("no tokens recorded in this window")
		}
		return sec
	}
	// The denominator is the sum of the SHOWN tiers, never Counts.Tokens. Tokens is not
	// always the sum of the split — a gateway reporting only a total yields a non-zero
	// Tokens with every field at zero — so normalising against it would draw shares
	// that do not add up, and would do so exactly on the responses whose split is least
	// trustworthy.
	var whole int64
	for _, c := range tiers {
		whole += c.count
	}
	countW := 0
	for _, c := range tiers {
		if w := lipgloss.Width(formatCount(int(c.count))); w > countW {
			countW = w
		}
	}
	sort.SliceStable(tiers, func(i, j int) bool { return tiers[i].count > tiers[j].count })
	rows := make([]costRow, 0, len(tiers)+1)
	for _, c := range tiers {
		count := formatCount(int(c.count))
		right := strings.Repeat(" ", countW-lipgloss.Width(count)) + count
		row := costRow{label: c.label, right: right}
		if pct := costShare(c.count, whole); pct != "" {
			row.right = right + costColGap + fmt.Sprintf("%6s", pct)
			row.frac = float64(c.count) / float64(whole)
			row.hasBar = true
		}
		rows = append(rows, row)
	}
	// Reasoning is a SUBSET of output, so it gets no share and no bar: adding it to the
	// tiers above would bill every reasoning token twice, at the dearest rate there is.
	// The label says so, for the reader who would otherwise add the column up.
	if t.PresentKinds&kindReasoning != 0 || t.ReasoningTokens != 0 {
		count := formatCount(int(t.ReasoningTokens))
		rows = append(rows, costRow{
			label: "reasoning (part of output)",
			right: strings.Repeat(" ", max(countW-lipgloss.Width(count), 0)) + count,
		})
	}
	sec.lines = renderCostRows(rows, width)
	add("Tiers price very differently — a cache read is roughly 0.1x uncached input, a cache " +
		"write roughly 1.25x, and output the dearest — so the largest share is the first place " +
		"to look, not the largest cost. Volume only: no per-tier dollars are aggregated " +
		"anywhere, so a money breakdown here would be invented.")
	return sec
}

// costCoverageSection names the pricing gaps, or renders nothing at all.
//
// Nothing at all is the important half. A fully priced deployment carries NO coverage
// warning: the ratio it would show reaches parity when it should, and a permanent
// warning with nothing to act on is what trains an operator to ignore the one signal
// that matters. That is the same lesson the old Requests-based denominator taught, and
// it is why the gap is measured against PRICEABLE requests here.
//
// A gap has to be nameable, not merely countable. "Cost is incomplete" gives an
// operator nothing to do; "api.openai.com gpt-5 x412" names the rate-table entry to
// add. Ordered largest first, because the entry that buys the most goes first.
func costCoverageSection(snap *usage.Snapshot, width int) costSection {
	priced, priceable := snap.Totals.PricedRequests, snap.Totals.PriceableRequests
	if priceable <= 0 || priced >= priceable {
		return costSection{}
	}
	sec := costSection{heading: "COVERAGE"}
	body := width - lipgloss.Width(costIndent)
	add := func(prose string) {
		for _, l := range wrapCells(prose, body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
	}
	if len(snap.UnpricedBy) == 0 {
		// A ledger-backed window (today, 7d) never carries UnpricedBy: a per-minute row
		// holds only its own labels, so it cannot tell a priced pair from an unpriced one.
		// The gap is still real and still visible in the counters, so it is reported and
		// the absence explained — rendering nothing here would read as "no gaps found",
		// which is a claim the rows do not support.
		// Two calls, not one sentence: wrapCells breaks on word boundaries, and a phrase a
		// reader is scanning for must not be able to straddle two lines.
		add(fmt.Sprintf("%s priceable requests carry no figure.",
			formatCount(int(priceable-priced))))
		add("The pairs are not reported for this window; ask for a duration window such as 1h to name them.")
		return sec
	}
	add("unpriced pairs, largest first — add a rate for each:")
	pairs := make([]string, 0, len(snap.UnpricedBy))
	for k := range snap.UnpricedBy {
		pairs = append(pairs, k)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if snap.UnpricedBy[pairs[i]] != snap.UnpricedBy[pairs[j]] {
			return snap.UnpricedBy[pairs[i]] > snap.UnpricedBy[pairs[j]]
		}
		// Ties break on the key so the list does not reshuffle between refreshes.
		return pairs[i] < pairs[j]
	})
	shown := pairs
	if len(shown) > costMaxGapRows {
		shown = shown[:costMaxGapRows]
	}
	rows := make([]costRow, 0, len(shown))
	for _, k := range shown {
		n := int(snap.UnpricedBy[k])
		rows = append(rows, costRow{
			// sanitizeLabel for the reason costBreakdownSection gives: the model half of the
			// key is workload-chosen and reaches here verbatim. CWE-150.
			label: sanitizeLabel(k),
			right: fmt.Sprintf("x%s", formatCount(n)),
		})
	}
	sec.lines = append(sec.lines, renderCostRows(rows, width)...)
	if rest := len(pairs) - len(shown); rest > 0 {
		add(fmt.Sprintf("+%d more pair%s, each with fewer requests", rest, plural(rest)))
	}
	return sec
}

// renderCostPane draws the pane: a header, then the sections in importance order.
//
// The window in the header comes from snap.Window — what the server ACTUALLY served —
// never from what this pane requested. A proxy with no durable cost ledger answers
// window=today from the ring's maximum span and reports that span here, so echoing
// the request would label six hours of spend as a day's: a wrong number wearing a
// right-looking label, which is worse than no number. It is also why the pane's title
// bar names no window at all.
//
// height is a budget, not a target. Sections are dropped WHOLE from the bottom when
// they do not fit, and the blank separators go first, because a half-rendered section
// misinforms: a truncated COVERAGE reads as a complete list of gaps, and a truncated
// "$12.5" for a $12.5000 total reads as a real, smaller number. No figure is ever
// clipped, at any width or height.
//
// group is the axis to break down by; a zero or GroupNone value falls back to the head
// of costPaneGroups, because this pane's whole purpose is the breakdown and an
// ungrouped view of it is the spend strip one row above.
func renderCostPane(snap *usage.Snapshot, group usage.Group, width, height int) string {
	if group == "" || group == usage.GroupNone {
		group = costPaneGroups[0]
	}
	if snap == nil {
		// No answer yet, and the pane must not imply one. Not an error either — the poll
		// chain fetches on arrival, so this is a frame or two, and the error path renders
		// its own diagnostic.
		return truncCells("COST — waiting for the first answer", width)
	}
	header := truncCells(fmt.Sprintf("COST — %s — by %s", sanitizeLabel(snap.Window), group), width)
	// Importance order. TOTAL first because it is the pane's answer, COVERAGE last
	// because the part of it that must survive truncation — the priced-versus-priceable
	// ratio — already rides on TOTAL.
	sections := []costSection{
		costTotalSection(snap, width),
		costBreakdownSection(snap, group, width),
		costTokenSection(snap, width),
		costCoverageSection(snap, width),
	}
	// An empty section is not a section. costCoverageSection returns one for a fully
	// priced deployment, and a bare heading over nothing would be exactly the permanent
	// warning it declines to render.
	kept := sections[:0]
	for _, s := range sections {
		if s.heading != "" && len(s.lines) > 0 {
			kept = append(kept, s)
		}
	}
	sections = kept
	if lines, ok := fitCostSections(header, sections, height); ok {
		return strings.Join(lines, "\n")
	}
	// Not even the header and the first section fit. The "TOTAL" heading is what goes —
	// the header one row above already says COST, so the heading is the only thing here
	// carrying no information — and the figure is kept, because it is the answer to the
	// question the pane exists for. Whole lines only; costTotalSection puts the figure on
	// its own first line precisely so this path cannot cut it.
	out := []string{header}
	if len(sections) > 0 {
		out = append(out, sections[0].lines...)
	}
	if height > 0 && len(out) > height {
		if height == 1 && len(out) > 1 {
			// One row to spend: the figure outranks the header. A header alone answers nothing.
			return out[1]
		}
		out = out[:height]
	}
	return strings.Join(out, "\n")
}

// fitCostSections assembles the widest prefix of sections that fits the height
// budget, preferring more sections over prettier spacing.
//
// The preference order is the specification: all sections with separators, then all
// sections without, then one fewer section with separators, and so on. Separators are
// decoration and go first; a section is information and goes only when it must.
//
// height <= 0 means unbounded, which is what a model that has not laid out yet
// reports. Refusing to render then would leave the pane blank until the first
// WindowSizeMsg.
func fitCostSections(header string, sections []costSection, height int) ([]string, bool) {
	assemble := func(n int, spaced bool) []string {
		out := []string{header}
		for _, s := range sections[:n] {
			if spaced {
				out = append(out, "")
			}
			out = append(out, s.heading)
			out = append(out, s.lines...)
		}
		return out
	}
	for n := len(sections); n >= 1; n-- {
		for _, spaced := range []bool{true, false} {
			if out := assemble(n, spaced); height <= 0 || len(out) <= height {
				return out, true
			}
		}
	}
	return nil, false
}

// renderCostBody is the pane's body: the rendered answer, or why there is none.
//
// The error lives here rather than in renderCostPane because renderCostPane takes
// only the ANSWER — it is a pure function of a snapshot, which is what makes every
// honesty rule in it table-testable without a model. The failure is model state.
//
// A failed poll must SAY so. spend.go records what the alternative costs: a broken,
// absent or unauthorised /v1/usage that renders "" produces no figure, no explanation
// and no diagnostic, on every poll, forever — while the layout goes on reserving the
// space. Rendering nothing and having nothing notice is the failure the whole cost
// surface exists to end.
//
// The error text is sanitized: it can carry a server-authored body, and writing that
// straight to a TTY lets an escape sequence recolour the pane or erase the line it is
// reporting on. CWE-150, the same reason the series labels go through it.
func (m *model) renderCostBody() string {
	if m.costPane.err != nil {
		lines := []string{truncCells("COST — unavailable", m.width)}
		for _, l := range wrapCells(sanitizeLabel(m.costPane.err.Error()), m.width-lipgloss.Width(costIndent)) {
			lines = append(lines, costIndent+l)
		}
		// No figure of any kind on this path. A stale total drawn beside a failed poll
		// presents a number nobody can vouch for as the current one.
		return strings.Join(lines, "\n")
	}
	if m.costPane.snap == nil {
		// "loading" and "no data" are different answers, exactly as renderUsage
		// distinguishes them: the first self-corrects on the reply already in flight, the
		// second means nothing is coming. renderCostPane has its own nil guard — reached
		// only if a future caller passes a snapshot-less state — but it cannot tell the two
		// apart, because it is a pure function of the answer and this is a fact about the
		// request.
		if m.costPane.loading {
			return truncCells("COST — loading…", m.width)
		}
		return truncCells("COST — no data yet", m.width)
	}
	out := renderCostPane(m.costPane.snap, m.costPane.group, m.width, m.bodyHeight)
	// A freshness line, in space the sections did not want.
	//
	// It earns its place because the pane refreshes on a timer with no other sign of
	// life: a figure carrying no age cannot be told from a frozen one, and a chain that
	// silently died looks exactly like a quiet hour. But it is worth strictly less than
	// any section, so it takes a row only when one is spare — the same priority order the
	// height budget applies to the separators.
	if !m.costPane.lastFetch.IsZero() {
		if n := len(strings.Split(out, "\n")); m.bodyHeight <= 0 || n < m.bodyHeight {
			age := time.Since(m.costPane.lastFetch).Truncate(time.Second)
			out += "\n" + costIndent + truncCells(
				fmt.Sprintf("updated %s ago (every %s)", age, costPollInterval),
				m.width-lipgloss.Width(costIndent))
		}
	}
	return out
}
