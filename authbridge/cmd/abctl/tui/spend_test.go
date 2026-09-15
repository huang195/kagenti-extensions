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

// A ledger-backed "today" answer becomes the headline. The strip's whole reason for
// a second poll is that the rolling window cannot answer "what did today cost".
func TestSpendSummary_LedgerBackedTodayBecomesTheHeadline(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: 1_120_000, PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}
	m.spend.todaySnap = &usage.Snapshot{
		Window: "today",
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 318, PriceableRequests: 318},
		Priced: true,
	}

	got := m.spendSummary()

	if !got.HasToday {
		t.Fatal("HasToday = false for a ledger-backed today snapshot")
	}
	if got.TodayUSD != 4.17 {
		t.Errorf("TodayUSD = %v, want 4.17", got.TodayUSD)
	}
	// The window figure is unaffected: today is an additional reading, not a
	// replacement, and the burn rate is still derived from the rolling window.
	if got.WindowUSD != 1.12 {
		t.Errorf("WindowUSD = %v, want the window figure untouched", got.WindowUSD)
	}
}

// The today figure's OWN coverage, which applyTodayFigure used to throw away.
//
// The fixture is the shape every "today" fixture in this file lacked: PricedRequests
// != PriceableRequests. With the counters discarded, this day — one priced request out
// of four hundred, a total of unknown magnitude and certainly far larger — reached the
// strip as a bare "$0.0031 today" with no gap marker anywhere on the line, because the
// only coverage note the strip built came from the 1h ring snapshot.
func TestSpendSummary_TodayCarriesItsOwnCoverageGap(t *testing.T) {
	m := &model{}
	m.spend.todaySnap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 400, CostMicros: 3_100,
			PricedRequests: 1, PriceableRequests: 400,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if !got.HasToday || got.TodayUSD != 0.0031 {
		t.Fatalf("today figure lost: HasToday=%v TodayUSD=%v", got.HasToday, got.TodayUSD)
	}
	if got.TodayPriceable != 400 {
		t.Errorf("TodayPriceable = %d, want 400 — without the denominator the gap is unreadable", got.TodayPriceable)
	}
	if got.TodayUnpriced != 399 {
		t.Errorf("TodayUnpriced = %d, want 399 (priceable minus priced)", got.TodayUnpriced)
	}
}

// A fully priced day must carry NO gap, for the reason a fully priced window carries
// none: a permanent warning with nothing to act on is what teaches an operator to
// ignore the one signal that matters.
func TestSpendSummary_FullyPricedTodayCarriesNoGap(t *testing.T) {
	m := &model{}
	m.spend.todaySnap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 318, PriceableRequests: 318,
		},
		Priced: true,
	}

	if got := m.spendSummary(); got.TodayUnpriced != 0 {
		t.Errorf("TodayUnpriced = %d for a fully priced day, want 0", got.TodayUnpriced)
	}
}

// The two windows' coverage counters must not be copied into each other. This is the
// second verified misread at the data layer: the day was complete and the HOUR had the
// gap, and one shared set of counters is how the day ended up wearing the hour's
// warning.
func TestSpendSummary_TodayCoverageIsNotTheWindowsCoverage(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 40, PriceableRequests: 40, PricedRequests: 0},
		Priced: false,
	}
	m.spend.todaySnap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 318, PriceableRequests: 318,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.Unpriced != 40 || got.Priceable != 40 {
		t.Fatalf("the window's own gap was lost: Unpriced=%d Priceable=%d", got.Unpriced, got.Priceable)
	}
	if got.TodayUnpriced != 0 {
		t.Errorf("TodayUnpriced = %d — the hour's 40-request gap leaked onto the day, which is complete",
			got.TodayUnpriced)
	}
	if got.TodayPriceable != 318 {
		t.Errorf("TodayPriceable = %d, want the DAY's 318, not the hour's 40", got.TodayPriceable)
	}
}

// An inexact total must reach the renderer AS inexact, from either snapshot.
//
// The fixture is the other shape every "today" fixture on this branch lacked:
// IncompleteRequests > 0. This branch carries a commit titled "Stop publishing a
// truncated stream's floor as an exact total", and spendSummary had no field for the
// counter that says so — so the strip republished the floor as "$4.1700 today" and
// "$1.1200 /1h", exact to four decimal places.
func TestSpendSummary_CarriesTheInexactCountFromBothSnapshots(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 10, CostMicros: 1_120_000,
			PricedRequests: 10, PriceableRequests: 10, IncompleteRequests: 3,
		},
		Priced: true,
	}
	m.spend.todaySnap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 318, PriceableRequests: 318, IncompleteRequests: 4,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.Incomplete != 3 {
		t.Errorf("Incomplete = %d, want the window's 3", got.Incomplete)
	}
	if got.TodayIncomplete != 4 {
		t.Errorf("TodayIncomplete = %d, want the day's 4", got.TodayIncomplete)
	}
	// Disclosed, not deducted: the dollars are real and belong in the total.
	if got.WindowUSD != 1.12 || got.TodayUSD != 4.17 {
		t.Errorf("an inexact figure was withheld instead of qualified: window=%v today=%v",
			got.WindowUSD, got.TodayUSD)
	}
}

// And an exact answer must carry no count, so the marker cannot become permanent
// furniture.
func TestSpendSummary_AnExactTotalReportsNothingInexact(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: 1_120_000, PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}
	m.spend.todaySnap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 318, PriceableRequests: 318},
		Priced: true,
	}

	got := m.spendSummary()

	if got.Incomplete != 0 || got.TodayIncomplete != 0 {
		t.Errorf("an exact pair of snapshots reported inexact counts: window=%d today=%d",
			got.Incomplete, got.TodayIncomplete)
	}
}

// THE case where the honest answer is to show less. A proxy with no durable cost
// ledger — Kubernetes by design — answers window=today from the ring's maximum span
// and reports THAT span. Trusting the request rather than the answer would label a
// six-hour total as a day's.
func TestSpendSummary_DegradedTodayWindowLeavesHasTodayFalse(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: 1_120_000, PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}
	m.spend.todaySnap = &usage.Snapshot{
		Window: usage.MaxWindow.String(), // "6h0m0s" — the ring, not the ledger
		Totals: usage.Counts{Requests: 50, CostMicros: 2_000_000, PricedRequests: 50, PriceableRequests: 50},
		Priced: true,
	}

	got := m.spendSummary()

	if got.HasToday {
		t.Errorf("HasToday = true for a %q window; a 6h total must not be labelled a day's", m.spend.todaySnap.Window)
	}
	if got.TodayUSD != 0 {
		t.Errorf("TodayUSD = %v, want 0 when there is no today figure", got.TodayUSD)
	}
}

// An unpriced today must not become a headline. renderSpendStrip renders the today
// figure whenever HasToday is set, with no Priced guard of its own, so admitting an
// unpriced day here would print "$0.0000 today" — a settled zero for a cost nobody
// knows.
func TestSpendSummary_UnpricedTodayLeavesHasTodayFalse(t *testing.T) {
	m := &model{}
	m.spend.todaySnap = &usage.Snapshot{
		Window: "today",
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10},
		Priced: false,
	}

	got := m.spendSummary()

	if got.HasToday {
		t.Error("HasToday = true for an unpriced today; the strip would render $0.0000")
	}
}

// A failed today poll must not become a zero headline either.
func TestSpendSummary_FailedTodayPollLeavesHasTodayFalse(t *testing.T) {
	m := &model{}
	m.spend.todayErr = context.DeadlineExceeded
	m.spend.todaySnap = &usage.Snapshot{Window: "today", Priced: true}

	if got := m.spendSummary(); got.HasToday {
		t.Error("HasToday = true after a failed today poll")
	}
}

// The today figure survives a window poll that has not answered yet: the two chains
// are independent, and the headline is the figure a user actually wants.
func TestSpendSummary_TodaySurvivesAWindowPollThatHasNotAnswered(t *testing.T) {
	m := &model{}
	m.spend.todaySnap = &usage.Snapshot{
		Window: "today",
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 318, PriceableRequests: 318},
		Priced: true,
	}

	got := m.spendSummary()

	if !got.HasToday || got.TodayUSD != 4.17 {
		t.Errorf("today figure lost with no window snapshot: HasToday=%v TodayUSD=%v", got.HasToday, got.TodayUSD)
	}
	if got.HasSnapshot {
		t.Error("HasSnapshot = true with no window snapshot")
	}
}

// The today chain must ask for window=today, which no time.Duration can express —
// the reason GetUsageWindow exists at all.
func TestFetchSpendToday_AsksForTheSymbolicWindow(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"window":"today","buckets":[],"totals":{}}`))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL)}
	cmd := m.fetchSpendToday()
	if cmd == nil {
		t.Fatal("fetchSpendToday returned no command with a client set")
	}
	msg, ok := cmd().(spendTodayLoadedMsg)
	if !ok {
		t.Fatalf("fetchSpendToday produced %T, want spendTodayLoadedMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse %q: %v", gotQuery, err)
	}
	if q.Get("window") != "today" {
		t.Errorf("window = %q, want \"today\"", q.Get("window"))
	}
	// session= alongside a symbolic window is refused by the server, because the
	// ledger holds no session ids. Asking for one would 400 every poll.
	if q.Has("session") {
		t.Errorf("today poll sent session=%q; the server refuses that with a symbolic window", q.Get("session"))
	}
	// No breakdown: the strip needs one number from this poll, and the sessions table
	// reads the window snapshot's series instead.
	if q.Has("group") && q.Get("group") != string(usage.GroupNone) {
		t.Errorf("group = %q, want none", q.Get("group"))
	}
	// resolution is omitted rather than sent as "0s", which the server rejects as
	// finer than its storage bucket.
	if q.Has("resolution") {
		t.Errorf("resolution = %q; the ledger serves one bucket and does not read it", q.Get("resolution"))
	}
}

// A today reply must not be accepted under the window chain's sequence, and vice
// versa: they ask different questions on different cadences, and a shared counter
// would let the fast chain invalidate the slow one's request forever.
func TestApplySpendTodayLoaded_UsesItsOwnSequence(t *testing.T) {
	m := &model{}
	m.spend.reqSeq = 7
	m.spend.todayReqSeq = 2
	fresh := &usage.Snapshot{Window: "today", Priced: true, Totals: usage.Counts{CostMicros: 1, PricedRequests: 1}}

	// The window chain's current sequence is not the today chain's.
	m.applySpendTodayLoaded(spendTodayLoadedMsg{req: 7, snap: fresh})
	if m.spend.todaySnap != nil {
		t.Error("a reply carrying the window chain's sequence was accepted as today's")
	}
	m.applySpendTodayLoaded(spendTodayLoadedMsg{req: 2, snap: fresh})
	if m.spend.todaySnap != fresh {
		t.Error("the today chain's own sequence was rejected")
	}
	// lastFetch reports the age of the WINDOW figure and must not move for a today
	// reply, or the strip claims a freshness the rolling figure does not have.
	if !m.spend.lastFetch.IsZero() {
		t.Errorf("lastFetch moved on a today reply: %v", m.spend.lastFetch)
	}
}

// invalidate must disown the today chain too. A different pod is a different day
// total, and its reply outlives the switch by the fetch timeout.
func TestSpendInvalidate_DisownsTheTodayChain(t *testing.T) {
	s := &spendState{
		todaySnap:    &usage.Snapshot{Window: "today"},
		todayErr:     context.DeadlineExceeded,
		todayReqSeq:  3,
		todayTickGen: 4,
	}

	s.invalidate()

	if s.todaySnap != nil || s.todayErr != nil {
		t.Errorf("today state survived invalidate: snap=%v err=%v", s.todaySnap, s.todayErr)
	}
	if s.todayReqSeq != 4 {
		t.Errorf("todayReqSeq = %d, want 4 — an in-flight reply must be disowned", s.todayReqSeq)
	}
	if s.todayTickGen != 5 {
		t.Errorf("todayTickGen = %d, want 5 — the old chain must stop scheduling", s.todayTickGen)
	}
}

// The today poll is deliberately far slower than the window poll: the figure only
// grows, by one turn at a time, and it is the more expensive answer to compute.
func TestSpendTodayPollInterval_IsMuchSlowerThanTheWindowPoll(t *testing.T) {
	if spendTodayPollInterval <= spendPollInterval {
		t.Fatalf("today interval %v is not slower than the window interval %v",
			spendTodayPollInterval, spendPollInterval)
	}
	if spendTodayPollInterval < 5*time.Minute {
		t.Errorf("today interval %v is faster than the 5m the comment claims is ample",
			spendTodayPollInterval)
	}
}
