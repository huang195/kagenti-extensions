package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// spendPollInterval is how often the strip refreshes. Matches the Usage pane's
// cadence: the strip is glanceable chrome, not a live meter, and a faster poll
// would spend requests to move a figure the user is not watching.
const spendPollInterval = 20 * time.Second

// spendTodayPollInterval is how often the "today" headline refreshes.
//
// Fifteen times slower than the window poll on purpose: "today" is a figure that
// moves slowly by construction — it only ever grows, and by the size of one turn —
// so a 20s cadence would spend a request every 20 seconds to move a number a user
// reads once a session. It is also the more expensive of the two answers, since the
// server reads day files off disk for it rather than summing an in-memory ring.
const spendTodayPollInterval = 5 * time.Minute

// spendWindow is the span the strip REQUESTS, and spendResolution asks for it as
// a SINGLE bucket. One bucket means the server folds and the client does no
// arithmetic over buckets — a client-side sum would be a second implementation of
// the fold, and the one place cost is settled is the aggregator.
//
// Requested, not covered: what the answer actually spans is snap.Window, and
// nothing may assume the two agree. Deriving the burn rate from this constant
// instead of from the snapshot is a real defect (a rate over a span the label
// contradicts), so the divisor is parsed from the answer — see spendSummary.
const (
	spendWindow     = time.Hour
	spendResolution = time.Hour
)

// spendState is the always-on spend summary behind the strip.
//
// Deliberately NOT m.usage. That state is pane-scoped: openUsage sets it, and it
// carries a user-chosen window and an optional single-session scope. The strip
// needs all-sessions data whenever abctl is running, whatever pane is showing —
// driving both from one chain would blank the strip the moment a user scoped the
// Usage pane to one session, which is exactly when they are looking at cost.
//
// The cost is one extra GET /v1/usage per 20s asking for a single bucket, plus one
// per 5 MINUTES for the ledger-backed "today" figure on its own chain. Together with
// the Usage pane's chain that is three, and all three can be alive at once —
// recorded here so it reads as a decision rather than an oversight. The today poll is
// the only one that touches disk server-side, which is why it is the slow one.
type spendState struct {
	snap      *usage.Snapshot
	err       error
	lastFetch time.Time

	// todaySnap is the ledger-backed "today" answer, on its OWN chain with its own
	// counters. Two chains against one endpoint, and a reply from one must never be
	// applied as the other's: they ask different questions, so a today reply stored
	// as the window snapshot would label a day's spend with the window's label and
	// drive the burn rate off it.
	//
	// A separate error too, because the two can fail independently — an older proxy
	// answers the window fine and 400s on window=today — and one broken figure must
	// not blank the other.
	todaySnap *usage.Snapshot
	todayErr  error

	// reqSeq is the id of the most recently ISSUED request; a reply carrying a
	// different id is stale and dropped. An id rather than a comparison of the
	// request's fields, for the reason usageLoadedMsg.req records: comparing
	// fields means every future request option has to be added to the comparison
	// or it silently stops being covered, while an id cannot be partially right.
	reqSeq uint64
	// tickGen identifies the current polling chain. See usageState.tickGen: a
	// quick exit and re-entry left two chains alive, each rescheduling the other's
	// successor and doubling the request rate for the life of the session.
	tickGen uint64

	// todayReqSeq and todayTickGen are the today chain's own counters, for the reason
	// the snapshot is its own field: sharing reqSeq with the window chain would make
	// every window reply invalidate the today request in flight, so the slower poll
	// would never land at all.
	todayReqSeq  uint64
	todayTickGen uint64
}

// invalidate drops the data this state describes and disowns anything in flight.
//
// Called when the pod behind the strip changes. A different pod is a different
// aggregator, so both the figure and any in-flight request belong to the old one.
// Keeping the snapshot draws the previous pod's spend as the new pod's; and NOT
// bumping reqSeq lets an old reply — a 5s timeout, so it easily outlives the
// switch — pass applySpendLoaded's guard and be stored with a FRESH lastFetch,
// presenting a stale number as a current one. Bumping tickGen alone achieves
// neither: it stops the old chain from scheduling, not the reply already in the
// air.
//
// Mirrors the m.usage reset in backToPodsPane, which is the same state shape
// facing the same hazard.
func (s *spendState) invalidate() {
	s.snap = nil
	s.err = nil
	s.lastFetch = time.Time{}
	s.reqSeq++
	s.tickGen++
	// The today figure is a different pod's day just as much as the window figure is
	// its hour, and its reply outlives the switch by the same 5s timeout. Both
	// counters move for the reason the window's do: bumping tickGen alone stops the
	// old chain from scheduling, not the reply already in the air.
	s.todaySnap = nil
	s.todayErr = nil
	s.todayReqSeq++
	s.todayTickGen++
}

// spendLoadedMsg carries a fetched snapshot back to Update.
type spendLoadedMsg struct {
	snap *usage.Snapshot
	req  uint64
	err  error
}

// spendTickMsg fires the periodic refetch. gen ties it to the chain that
// scheduled it, so a tick from a superseded chain is ignored.
type spendTickMsg struct{ gen uint64 }

// spendTodayLoadedMsg carries the ledger-backed "today" snapshot back to Update.
// A distinct type from spendLoadedMsg so the two replies cannot be confused by the
// dispatch switch — the compiler enforces what a shared type would leave to a field.
type spendTodayLoadedMsg struct {
	snap *usage.Snapshot
	req  uint64
	err  error
}

// spendTodayTickMsg fires the today refetch, on its own generation.
type spendTodayTickMsg struct{ gen uint64 }

// spendSummary is what the strip renders.
//
// Optional fields rather than a narrower struct, because a figure can be genuinely
// unavailable rather than zero. "Today" is now measurable — the durable cost ledger
// supplies it, and applyTodayFigure sets HasToday only when the server actually
// served the "today" window AND priced it. "Saved" still is not: it needs tool-prune
// attribution aggregated across requests, so HasSaved stays false and the strip shows
// nothing for it rather than a zero, because "saved $0.00" asserts that pruning saved
// nothing when the truth is that nothing measures it yet.
type spendSummary struct {
	WindowUSD   float64
	WindowLabel string
	// BurnPerMin is 0 when it cannot be derived. Zero means "not enough data",
	// never "free".
	BurnPerMin float64
	// Priced reports whether ANY request in the window produced a cost. False
	// means render "cost unavailable" — never $0.00, which reads as free traffic.
	Priced bool
	// Failed reports that the last poll did not answer at all — a broken, absent or
	// unauthorised /v1/usage.
	//
	// Distinct from Priced == false, which means the endpoint DID answer and
	// nothing in the window carried a cost. Both render "cost unavailable", but
	// only this one can be true with no counters at all, so the renderer must not
	// infer it from Priceable == 0: doing that made a failing endpoint render an
	// empty strip forever, with the row still reserved and nothing saying why.
	Failed bool
	// Unpriced is how many PRICEABLE requests carry no figure, and Priceable is
	// the denominator that makes it readable.
	Unpriced  int64
	Priceable int64
	// HasSnapshot reports that a poll actually answered.
	//
	// It is what separates "we looked, and there was no inference traffic" from
	// "we have not looked yet" — identical in every counter, opposite in meaning.
	// The first is a finding worth a row; the second is honest silence that
	// self-corrects on the next poll. Without this the renderer had to guess from
	// Priceable == 0 and got both wrong the same way.
	HasSnapshot bool

	// TodayUSD is spend since local midnight, from the durable cost ledger.
	//
	// HasToday false means NO figure, never zero — see applyTodayFigure for the two
	// ways that happens (the server degraded to a ring window because it has no
	// ledger, or the window priced nothing).
	TodayUSD float64
	HasToday bool
	SavedUSD float64 // set once tool-prune savings are aggregated
	HasSaved bool
}

// spendSummary derives the strip's figures from the last snapshot.
func (m *model) spendSummary() spendSummary {
	// An errored poll is an UNKNOWN cost, and the strip must SAY so. Reporting it
	// as the zero value made the renderer fall through to its "nothing to say"
	// path, so a broken or absent /v1/usage produced no figure, no explanation and
	// no diagnostic on every poll forever — while layout() went on reserving the
	// row, leaving a permanent blank line above the footer. Rendering nothing and
	// having nothing notice is the exact failure this whole strip exists to end.
	if m.spend.err != nil {
		// Failed describes the WINDOW poll. The today figure is deliberately NOT carried
		// here even when its own chain answered: renderSpendStrip returns on Failed
		// before it reads any figure, so setting one would be an assignment nothing
		// reads. Showing a good day total beside a failed window poll would be the
		// better strip, but it is a renderer change — the Failed branch would have to
		// yield to the figures — and this commit touches the data side only.
		return spendSummary{Failed: true}
	}
	snap := m.spend.snap
	if snap == nil {
		out := spendSummary{}
		m.applyTodayFigure(&out)
		return out
	}
	out := spendSummary{
		WindowLabel: snap.Window,
		Priced:      snap.Priced,
		Priceable:   snap.Totals.PriceableRequests,
		HasSnapshot: true,
	}
	// Priceable minus priced, NOT requests minus priced. Requests counts every
	// proxied response — MCP calls, health checks — while only inference can ever
	// be priced, so the wrong denominator left a correctly configured deployment
	// reading a permanent warning with nothing to act on.
	if gap := snap.Totals.PriceableRequests - snap.Totals.PricedRequests; gap > 0 {
		out.Unpriced = gap
	}
	// The divisor comes from the span the SNAPSHOT reports, never from the
	// spendWindow constant. spendWindow is what we ASKED for; snap.Window is what
	// the server answered with, and the two are not the same promise. Dividing the
	// answer's cost by the request's span renders a "/min" rate over a period the
	// strip's own label contradicts — a wrong number wearing a right-looking label,
	// which is worse than no number.
	//
	// It also fixes a mismatch that is live today rather than hypothetical: the
	// aggregator sets Window from time.Duration.String(), so a one-hour request
	// comes back as "1h0m0s". Deriving the span lets the label be rendered from the
	// parsed duration instead of echoing that.
	span, spanOK := parseWindowSpan(snap.Window)
	if spanOK {
		out.WindowLabel = formatWindowLabel(span)
	}
	m.applyTodayFigure(&out)
	if snap.Priced {
		out.WindowUSD = float64(snap.Totals.CostMicros) / 1e6
		// Suppressed, not approximated, when the span is unknown. A rate is a
		// quotient: without a trustworthy denominator there is no honest figure to
		// show, and BurnPerMin == 0 already means "cannot be derived" everywhere
		// else. Substituting the requested window here is exactly the bug above.
		if spanOK {
			if mins := span.Minutes(); mins > 0 {
				out.BurnPerMin = out.WindowUSD / mins
			}
		}
	}
	return out
}

// sessionCost returns what one session cost over the strip's window.
//
// Reads the strip's own snapshot rather than issuing anything: fetchSpend asks for
// group=session on the all-sessions ring, so the poll that feeds the strip already
// carries a Series entry per session. A per-row fetch would be one request per
// visible row on a 20s cadence.
//
// priced=false means NO FIGURE, and the caller must render it as blank rather than
// as $0.00 — a table cell has even less room to explain itself than the strip does.
// Three distinct states collapse into it, all of them "unknown" and none of them
// "free": no snapshot yet, a window that priced nothing at all, and a session this
// window has no priced request for. The last is the one worth naming, because
// Snapshot.Priced is WINDOW-wide: a snapshot that priced another session's traffic
// says Priced == true, so trusting that flag alone would render $0.0000 against a
// session whose cost is simply unknown.
func (m *model) sessionCost(id string) (usd float64, priced bool) {
	if m.spend.snap == nil || !m.spend.snap.Priced || id == "" {
		return 0, false
	}
	var micros int64
	var pricedReqs int64
	// Every bucket, not Buckets[0]. The strip asks for a single-bucket resolution
	// but the server negotiates it (see Snapshot.BucketSeconds), so reading the
	// first bucket would report the first slice of the window as the whole of it.
	for _, b := range m.spend.snap.Buckets {
		c, ok := b.Series[id]
		if !ok {
			continue
		}
		micros += c.CostMicros
		pricedReqs += c.PricedRequests
	}
	if pricedReqs == 0 {
		return 0, false
	}
	return float64(micros) / 1e6, true
}

// parseWindowSpan interprets a snapshot's Window string as a duration.
//
// Not every value has to be one. The symbolic windows now exist — usage.WindowToday
// and usage.Window7d are labels no duration parser reads — and while the strip's
// WINDOW chain asks only for fixed lengths, so this succeeds on every label it
// currently sees, the field is free-form on the wire and a server is entitled to
// answer with a span it names rather than measures. Returning false then is what
// lets the caller suppress the burn rate instead of inventing a denominator.
func parseWindowSpan(label string) (time.Duration, bool) {
	d, err := time.ParseDuration(label)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// formatWindowLabel renders a span the way a strip has room for: "1h", not
// "1h0m0s".
//
// time.Duration.String() always emits every non-zero-suffixed unit, so the
// aggregator's own Window field reads "1h0m0s" for a one-hour window — six columns
// where two would do, on a line whose whole design problem is width. No test ever
// saw it because the fixtures in the brief used the tidy form.
func formatWindowLabel(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return d.String()
	}
}

// spendTickIsCurrent reports whether a tick belongs to the live chain.
func (m *model) spendTickIsCurrent(gen uint64) bool { return gen == m.spend.tickGen }

// applySpendLoaded stores a reply unless it is stale.
//
// lastFetch moves only on an accepted reply: advancing it for a discarded one
// would have the strip report the age of data it just threw away.
//
// A failed poll clears snap rather than leaving the previous one in place: once
// the fetch failed we do not know the current spend, and continuing to draw the
// last figure would present a stale number as a current one.
//
// It does NOT render as silence. err is what spendSummary turns into
// spendSummary.Failed, which the strip draws as "cost unavailable". The row is
// reserved on height alone, so a broken endpoint rendering "" would buy a
// permanent blank line above the footer and no diagnostic anywhere on screen.
func (m *model) applySpendLoaded(msg spendLoadedMsg) {
	if msg.req != m.spend.reqSeq {
		return
	}
	m.spend.snap, m.spend.err, m.spend.lastFetch = msg.snap, msg.err, time.Now()
}

// startSpendPolling begins (or restarts) the chain on a clean slate. Fetching
// immediately as well means the strip is current on arrival rather than blank for
// up to spendPollInterval.
//
// invalidate() rather than a bare tickGen++ so that entering a session view can
// never inherit a figure from a previous one. backToPodsPane already invalidates
// on the way OUT, which is what closes the window while the picker is up; this is
// the same guarantee on the way IN, so any future path that starts a chain gets it
// without having to remember. The doubled reqSeq++ (here and in fetchSpend) is
// harmless — the sequence only has to be monotonic.
func (m *model) startSpendPolling() tea.Cmd {
	m.spend.invalidate()
	// Both chains, each on its own generation. The today figure is fetched
	// immediately too rather than waiting out its 5-minute interval: the whole point
	// of the headline is that it is there when the user arrives.
	return tea.Batch(
		m.fetchSpend(), spendTick(m.spend.tickGen),
		m.fetchSpendToday(), spendTodayTick(m.spend.todayTickGen),
	)
}

// spendTick schedules the next poll for the given generation.
func spendTick(gen uint64) tea.Cmd {
	return tea.Tick(spendPollInterval, func(time.Time) tea.Msg { return spendTickMsg{gen: gen} })
}

// fetchSpend requests the all-sessions single-bucket snapshot off the render
// loop. Returns nil before a client exists (picker mode), which keeps the tick
// chain alive without issuing a request — the same shape tickMsg uses.
func (m *model) fetchSpend() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	m.spend.reqSeq++
	req := m.spend.reqSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Session "" is every session, and group=session asks for the per-session
		// breakdown ON that all-sessions ring — which is what makes ONE poll serve
		// both consumers: the strip reads Totals, the sessions table reads one
		// Series entry per row. The alternative is a request per row.
		//
		// Totals is unaffected by grouping (Snapshot sums it from the raw buckets
		// before folding), so the strip's figures are byte-identical to what
		// group=none returned. This asked for group=none until the sessions table
		// existed, on the sound-at-the-time grounds that a breakdown nothing renders
		// is a label map paid for and thrown away.
		snap, err := client.GetUsage(ctx, spendWindow, spendResolution, "", usage.GroupSession)
		return spendLoadedMsg{snap: snap, req: req, err: err}
	}
}

// applyTodayFigure fills in the strip's "today" headline from the ledger-backed
// poll, or leaves it unset.
//
// Two conditions, and both are load-bearing:
//
// The response's window must actually BE "today". A proxy with no durable cost
// ledger — Kubernetes by design, or a local install with it turned off — answers
// window=today from the in-memory ring's maximum span and reports that span as the
// window it served. Setting HasToday from the request rather than from the answer
// would label a six-hour total as a day's, which is a wrong number wearing a right
// label. This is the one case where the honest answer is to show less: the strip
// falls back to its rolling-window figure, which is correctly labelled.
//
// And the answer must be PRICED. renderSpendStrip guards its window figure on
// Priced but renders the today figure whenever HasToday is set, so an unpriced day
// admitted here would print "$0.0000 today" — a settled zero for a cost nobody
// knows, the one thing the strip is forbidden to do. Guarded here rather than there
// because the renderer already handles both fields and this commit is data-only.
func (m *model) applyTodayFigure(out *spendSummary) {
	snap := m.spend.todaySnap
	if snap == nil || m.spend.todayErr != nil {
		return
	}
	if snap.Window != usage.WindowToday {
		return
	}
	if !snap.Priced {
		return
	}
	out.TodayUSD = float64(snap.Totals.CostMicros) / 1e6
	out.HasToday = true
}

// spendTodayTickIsCurrent reports whether a today tick belongs to the live chain.
func (m *model) spendTodayTickIsCurrent(gen uint64) bool { return gen == m.spend.todayTickGen }

// applySpendTodayLoaded stores a today reply unless it is stale.
//
// Does not move lastFetch: that field reports the age of the WINDOW figure, and
// advancing it on a today reply would have the strip claim a freshness the rolling
// figure does not have.
func (m *model) applySpendTodayLoaded(msg spendTodayLoadedMsg) {
	if msg.req != m.spend.todayReqSeq {
		return
	}
	m.spend.todaySnap, m.spend.todayErr = msg.snap, msg.err
}

// spendTodayTick schedules the next today poll for the given generation.
func spendTodayTick(gen uint64) tea.Cmd {
	return tea.Tick(spendTodayPollInterval, func(time.Time) tea.Msg {
		return spendTodayTickMsg{gen: gen}
	})
}

// fetchSpendToday requests the ledger-backed day total off the render loop.
//
// GetUsageWindow rather than GetUsage: GetUsage takes a time.Duration and
// stringifies it, and "today" is a boundary rather than a length, so it cannot be
// expressed that way at all.
//
// group=none, unlike fetchSpend: the strip needs one number from this poll and the
// sessions table reads the window snapshot's series, so asking for a breakdown here
// would be a label map paid for and thrown away. Resolution 0 omits the parameter —
// the ledger serves this as a single bucket and does not read it.
func (m *model) fetchSpendToday() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	m.spend.todayReqSeq++
	req := m.spend.todayReqSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, usage.WindowToday, 0, "", usage.GroupNone)
		return spendTodayLoadedMsg{snap: snap, req: req, err: err}
	}
}
