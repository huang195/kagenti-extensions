package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

func TestSpendSummary_DerivesWindowAndBurnRate(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests:          10,
			CostMicros:        1_120_000, // $1.12
			PricedRequests:    10,
			PriceableRequests: 10,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.WindowUSD != 1.12 {
		t.Errorf("WindowUSD = %v, want 1.12", got.WindowUSD)
	}
	if got.WindowLabel != "1h" {
		t.Errorf("WindowLabel = %q, want %q", got.WindowLabel, "1h")
	}
	// $1.12 over 60 minutes.
	if want := 1.12 / 60; got.BurnPerMin < want-1e-9 || got.BurnPerMin > want+1e-9 {
		t.Errorf("BurnPerMin = %v, want %v", got.BurnPerMin, want)
	}
	if !got.Priced {
		t.Error("Priced = false, want true")
	}
}

func TestSpendSummary_NothingPricedIsNotZero(t *testing.T) {
	// The distinction the whole strip rests on: an unknown cost is not a zero one.
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10},
		Priced: false,
	}

	got := m.spendSummary()

	if got.Priced {
		t.Error("Priced = true, want false")
	}
	if got.WindowUSD != 0 {
		t.Errorf("WindowUSD = %v; a caller must read Priced, not a sentinel", got.WindowUSD)
	}
	if got.Unpriced != 10 {
		t.Errorf("Unpriced = %d, want 10", got.Unpriced)
	}
}

func TestSpendSummary_CountsTheCoverageGap(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 306, PriceableRequests: 318,
		},
		Priced: true,
	}

	got := m.spendSummary()

	// Priceable minus priced, NOT requests minus priced: Requests counts every
	// proxied response including MCP and health checks, and using it as the
	// denominator made a correctly configured deployment read a permanent warning.
	if got.Unpriced != 12 {
		t.Errorf("Unpriced = %d, want 12", got.Unpriced)
	}
	if got.Priceable != 318 {
		t.Errorf("Priceable = %d, want 318", got.Priceable)
	}
}

func TestSpendSummary_NoSnapshotYet(t *testing.T) {
	m := &model{}
	got := m.spendSummary()
	if got.Priced {
		t.Error("Priced = true with no snapshot; want false so the strip says nothing yet")
	}
}

func TestSpendSummary_NoSavedFigureUntilItIsMeasured(t *testing.T) {
	// Rendering "saved $0.00" would assert that pruning saved nothing, when the
	// truth is that nothing measures it yet. Same rule as "cost unavailable".
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 100, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}

	if got := m.spendSummary(); got.HasSaved {
		t.Error("HasSaved = true; no Avoided data exists until a later commit")
	}
}

func TestSpendTick_StaleGenerationIsDropped(t *testing.T) {
	// The guard usage_pane.go:74-79 documents: two live chains each rescheduling
	// the other's successor doubles the request rate for the life of the session.
	m := &model{}
	m.spend.tickGen = 2

	if m.spendTickIsCurrent(1) {
		t.Error("a tick from generation 1 was accepted while generation 2 is current")
	}
	if !m.spendTickIsCurrent(2) {
		t.Error("the current generation's tick was dropped")
	}
}

func TestSpendLoaded_StaleReplyIsDropped(t *testing.T) {
	m := &model{}
	m.spend.reqSeq = 5
	fresh := &usage.Snapshot{Window: "1h"}

	m.applySpendLoaded(spendLoadedMsg{req: 4, snap: fresh})
	if m.spend.snap != nil {
		t.Error("a reply from an older request was applied")
	}
	if !m.spend.lastFetch.IsZero() {
		t.Error("a stale reply moved lastFetch; the strip would report data it discarded")
	}

	m.applySpendLoaded(spendLoadedMsg{req: 5, snap: fresh})
	if m.spend.snap != fresh {
		t.Error("the current request's reply was dropped")
	}
	if m.spend.lastFetch.IsZero() {
		t.Error("lastFetch was not recorded for the accepted reply")
	}
}

func TestStartSpendPolling_InvalidatesThePreviousChain(t *testing.T) {
	// Re-entering a session view must not leave the old chain alive: two chains
	// each rescheduling their own successor doubles the request rate permanently.
	m := &model{}
	m.startSpendPolling()
	first := m.spend.tickGen
	m.startSpendPolling()

	if m.spend.tickGen == first {
		t.Errorf("tickGen still %d after a restart; the previous chain's ticks stay current", first)
	}
	if !m.spendTickIsCurrent(m.spend.tickGen) || m.spendTickIsCurrent(first) {
		t.Error("the restart did not make exactly the newest generation current")
	}
}

func TestSpendInvalidate_DropsTheSnapshotAndDisownsInFlight(t *testing.T) {
	// A different pod is a different aggregator. Finding 1: without this the old
	// pod's figure survives the switch and is drawn as the new pod's.
	m := &model{}
	m.spend.snap = &usage.Snapshot{Window: "1h"}
	m.spend.err = errUsageUnsupported
	m.spend.lastFetch = time.Now()
	beforeSeq, beforeGen := m.spend.reqSeq, m.spend.tickGen

	m.spend.invalidate()

	if m.spend.snap != nil {
		t.Error("the old pod's snapshot survived invalidate; the strip would draw it as the new pod's")
	}
	if m.spend.err != nil {
		t.Error("the old pod's error survived invalidate")
	}
	if !m.spend.lastFetch.IsZero() {
		t.Error("lastFetch survived invalidate; the strip would report the old pod's freshness")
	}
	if m.spend.reqSeq == beforeSeq {
		t.Error("reqSeq was not bumped, so an in-flight old-pod reply still passes the staleness guard")
	}
	if m.spend.tickGen == beforeGen {
		t.Error("tickGen was not bumped, so the old chain keeps scheduling")
	}
}

func TestSpendInvalidate_InFlightOldPodReplyIsDropped(t *testing.T) {
	// The half a bare tickGen++ cannot do. GetUsage has a 5s timeout, so a request
	// issued against the old pod can easily land after the switch; it must not be
	// stored, and above all must not be stored with a fresh lastFetch.
	m := &model{}
	m.spend.reqSeq = 7
	inFlight := m.spend.reqSeq // the id the old pod's request carries

	m.spend.invalidate()

	m.applySpendLoaded(spendLoadedMsg{req: inFlight, snap: &usage.Snapshot{Window: "1h"}})

	if m.spend.snap != nil {
		t.Error("an old-pod reply landed after the switch and was stored as the new pod's spend")
	}
	if !m.spend.lastFetch.IsZero() {
		t.Error("an old-pod reply moved lastFetch, presenting a stale number as a current one")
	}
}

func TestBackToPodsPane_InvalidatesTheSpendStrip(t *testing.T) {
	// Finding 1 at the real call site: backToPodsPane resets m.usage but left
	// m.spend untouched, so the figure survived a pod switch.
	m := &model{}
	m.events = map[string][]pipeline.SessionEvent{}
	m.pane = paneEvents
	// backToPodsPane re-derives m.ctx from parentCtx for the next session view; it
	// panics on a nil parent.
	m.parentCtx = context.Background()
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 9_990_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}
	m.spend.lastFetch = time.Now()
	beforeSeq := m.spend.reqSeq

	m.backToPodsPane()

	if m.spend.snap != nil {
		t.Error("the previous pod's spend snapshot survived backToPodsPane")
	}
	if got := m.spendSummary(); got.Priced || got.WindowUSD != 0 {
		t.Errorf("spendSummary still reports the old pod: %+v", got)
	}
	if m.spend.reqSeq == beforeSeq {
		t.Error("backToPodsPane left reqSeq alone, so an in-flight old-pod reply would be accepted")
	}
}

func TestStartSpendPolling_StartsOnACleanSlate(t *testing.T) {
	// The way IN, complementing backToPodsPane's way OUT: entering a session view
	// must never inherit a figure from a previous one.
	m := &model{}
	m.spend.snap = &usage.Snapshot{Window: "1h"}
	m.spend.err = errUsageUnsupported
	m.spend.lastFetch = time.Now()

	m.startSpendPolling()

	if m.spend.snap != nil || m.spend.err != nil || !m.spend.lastFetch.IsZero() {
		t.Errorf("startSpendPolling inherited previous state: snap=%v err=%v lastFetch=%v",
			m.spend.snap, m.spend.err, m.spend.lastFetch)
	}
}

func TestSpendSummary_AFailedPollIsUnknownNotEmpty(t *testing.T) {
	// Finding 2: an errored poll reported as the zero value made the renderer fall
	// through to "nothing to say", so a broken /v1/usage drew an empty strip
	// forever with the row still reserved and no diagnostic anywhere.
	m := &model{}
	m.spend.err = errUsageUnsupported

	got := m.spendSummary()

	if !got.Failed {
		t.Error("Failed = false after an errored poll; the strip cannot tell silence from unavailability")
	}
	if got.Priced {
		t.Error("Priced = true after an errored poll")
	}
	if got.WindowUSD != 0 {
		t.Errorf("WindowUSD = %v after an errored poll, want 0", got.WindowUSD)
	}
}

func TestSpendSummary_NoErrorMeansNotFailed(t *testing.T) {
	// The other half, so Failed cannot be hardwired true: a healthy answer that
	// simply priced nothing is NOT a failure, and the two render differently
	// (the coverage counters are only meaningful for the non-failure case).
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10},
		Priced: false,
	}

	if got := m.spendSummary(); got.Failed {
		t.Error("Failed = true for a successful poll that happened to price nothing")
	}
}

func TestSpendSummary_BurnRateUsesTheSnapshotsWindowNotTheRequested(t *testing.T) {
	// The promoted finding. spendWindow is 1h, but the divisor must come from what
	// the server ANSWERED with. $1.20 over a 30m window is $0.04/min; dividing by
	// the requested 60 minutes would render $0.02/min -- a wrong rate under a label
	// ("/30m") that contradicts it.
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "30m",
		Totals: usage.Counts{
			Requests: 5, CostMicros: 1_200_000,
			PricedRequests: 5, PriceableRequests: 5,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if want := 1.20 / 30; got.BurnPerMin < want-1e-9 || got.BurnPerMin > want+1e-9 {
		t.Errorf("BurnPerMin = %v, want %v (cost / the SNAPSHOT's 30 minutes)", got.BurnPerMin, want)
	}
	if wrong := 1.20 / 60; got.BurnPerMin > wrong-1e-9 && got.BurnPerMin < wrong+1e-9 {
		t.Error("BurnPerMin was computed from the requested spendWindow, not the snapshot's window")
	}
	if got.WindowLabel != "30m" {
		t.Errorf("WindowLabel = %q, want %q", got.WindowLabel, "30m")
	}
}

func TestSpendSummary_UnparseableWindowSuppressesTheRate(t *testing.T) {
	// A rate is a quotient: with no trustworthy denominator there is no honest
	// figure, so it must be suppressed rather than approximated from the requested
	// span. The window figure itself is still known and still shown.
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "today", // a future symbolic, ledger-backed span
		Totals: usage.Counts{
			Requests: 5, CostMicros: 1_200_000,
			PricedRequests: 5, PriceableRequests: 5,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.BurnPerMin != 0 {
		t.Errorf("BurnPerMin = %v for an unparseable window; want 0 (suppressed)", got.BurnPerMin)
	}
	if got.WindowUSD != 1.20 {
		t.Errorf("WindowUSD = %v, want 1.20: the total is still known", got.WindowUSD)
	}
	if got.WindowLabel != "today" {
		t.Errorf("WindowLabel = %q, want the server's own %q preserved verbatim", got.WindowLabel, "today")
	}
}

func TestSpendSummary_TidiesTheAggregatorsWindowString(t *testing.T) {
	// The aggregator sets Window from time.Duration.String(), so a one-hour request
	// comes back "1h0m0s" -- six columns where two would do, on a line whose whole
	// design problem is width. No test saw this because the brief's fixtures used
	// the tidy form.
	for _, tc := range []struct{ wire, want string }{
		{"1h0m0s", "1h"},
		{"6h0m0s", "6h"},
		{"10m0s", "10m"},
		{"30m", "30m"},
		{"1h", "1h"},
		{"90s", "1m30s"}, // not a whole minute or hour: falls back to String()
	} {
		m := &model{}
		m.spend.snap = &usage.Snapshot{Window: tc.wire, Totals: usage.Counts{Requests: 1}}
		if got := m.spendSummary().WindowLabel; got != tc.want {
			t.Errorf("wire %q -> label %q, want %q", tc.wire, got, tc.want)
		}
	}
}

func TestSpendSummary_HasSnapshotSeparatesLookedFromNotLooked(t *testing.T) {
	// Identical in every counter, opposite in meaning.
	var notLooked model
	if got := notLooked.spendSummary(); got.HasSnapshot {
		t.Error("HasSnapshot = true with no snapshot")
	}

	looked := &model{}
	looked.spend.snap = &usage.Snapshot{Window: "1h", Totals: usage.Counts{}}
	if got := looked.spendSummary(); !got.HasSnapshot {
		t.Error("HasSnapshot = false after a poll answered with an empty window")
	}
}

// The one poll has to ask for the per-session breakdown, or the sessions table's
// COST column is blank against a perfectly healthy proxy.
//
// Asserted on the WIRE rather than by reading the constant back, because the
// constant is not the contract: apiclient.GetUsage OMITS the group parameter
// entirely for GroupNone, so a revert to group=none is invisible in the request
// except by its absence. Nothing else in the package would notice — the strip reads
// Totals, which grouping does not affect, so the strip's own tests stay green while
// every row of the sessions table silently loses its figure.
func TestFetchSpend_AsksForThePerSessionBreakdown(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"window":"1h","group":"session","buckets":[],"totals":{}}`))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL)}
	cmd := m.fetchSpend()
	if cmd == nil {
		t.Fatal("fetchSpend returned no command with a client set")
	}
	msg, ok := cmd().(spendLoadedMsg)
	if !ok {
		t.Fatalf("fetchSpend produced %T, want spendLoadedMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	// Parsed rather than substring-matched: "group=session" contains no "session="
	// and "session=x" contains no "group=", so a pair of Contains checks written to
	// cover both parameters would silently cover only one of them.
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("unparseable query %q: %v", gotQuery, err)
	}
	if got := q.Get("group"); got != string(usage.GroupSession) {
		t.Errorf("group = %q, want %q; the sessions table gets no per-row cost without it", got, usage.GroupSession)
	}
	// The breakdown is only useful on the ALL-sessions ring, so a session parameter
	// would collapse it to the one row it scoped to.
	if got := q.Get("session"); got != "" {
		t.Errorf("session = %q; the poll must cover every session", got)
	}
}
