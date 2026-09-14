package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// spendPollInterval is how often the strip refreshes. Matches the Usage pane's
// cadence: the strip is glanceable chrome, not a live meter, and a faster poll
// would spend requests to move a figure the user is not watching.
const spendPollInterval = 20 * time.Second

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
// The cost is one extra GET /v1/usage per 20s asking for a single bucket. Both
// chains can be alive at once; that is two cheap requests per interval, recorded
// here so it reads as a decision rather than an oversight.
type spendState struct {
	snap      *usage.Snapshot
	err       error
	lastFetch time.Time

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

// spendSummary is what the strip renders.
//
// Optional fields rather than a narrower struct, because two of the four figures
// the spec asks for are not measurable yet: "today" needs the durable cost
// ledger and "saved" needs tool-prune attribution aggregated across requests.
// Later commits set HasToday / HasSaved and the renderer picks them up without
// gaining a branch — and until then the strip shows nothing for them rather than
// a zero, because "saved $0.00" asserts that pruning saved nothing when the
// truth is that nothing measures it yet.
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

	TodayUSD float64 // set once the durable cost ledger exists
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
		return spendSummary{Failed: true}
	}
	snap := m.spend.snap
	if snap == nil {
		return spendSummary{}
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

// sessionSpan is how long a session has been alive: UpdatedAt - CreatedAt.
//
// Both come from session.SessionSummary, which carries them on the wire. Second
// resolution beats the alternative of deriving the span from the usage snapshot's
// buckets, which would be capped at the bucket width and would silently disagree
// with the UPDATED column right beside it.
//
// Note what this measures: elapsed wall time from first to last event, NOT time
// spent active. A session idle for an hour reports an hour. That is the honest
// reading of "how long has this session been going", and it matches what UPDATED
// already implies; a duty-cycle figure would need a different name.
//
// Returns 0 for anything it cannot state, including a non-advancing or backwards
// UpdatedAt. Zero means "no span to show" and the caller renders it blank — a
// literal "0s" would assert that the session began and ended in one instant, and a
// negative span would render as "-3m", which is not a shorter session but a
// broken clock.
//
// A free function rather than a method: it needs no model state, so it is
// table-testable directly.
func sessionSpan(s session.SessionSummary) time.Duration {
	if s.CreatedAt.IsZero() || !s.UpdatedAt.After(s.CreatedAt) {
		return 0
	}
	return s.UpdatedAt.Sub(s.CreatedAt)
}

// parseWindowSpan interprets a snapshot's Window string as a duration.
//
// Not every value has to be one. usage.ParseWindow currently rejects anything
// time.ParseDuration cannot read, so today this always succeeds — but the label is
// a free-form string on the wire, and a future symbolic window ("today", for a
// ledger-backed span) would arrive here as unparseable. Returning false then is
// what lets the caller suppress the rate instead of inventing a denominator.
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
	return tea.Batch(m.fetchSpend(), spendTick(m.spend.tickGen))
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
