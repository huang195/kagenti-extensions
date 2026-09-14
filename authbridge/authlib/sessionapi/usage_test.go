package sessionapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costledger"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// fetchUsage GETs the endpoint and returns status plus raw body.
func fetchUsage(t *testing.T, base, query string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + "/v1/usage" + query)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// Without WithUsage the endpoint must 404, not serve an empty snapshot: zeroed
// buckets would render as a flat chart implying idle traffic, hiding the fact
// that aggregation is not running at all.
func TestHandleUsage_NoAggregator404s(t *testing.T) {
	ts, _ := newTestServer(t)
	status, body := fetchUsage(t, ts.URL, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if !strings.Contains(body, "not enabled") {
		t.Errorf("body should say why: %q", body)
	}
}

func TestHandleUsage_Defaults(t *testing.T) {
	agg := usage.New()
	ts, _ := newTestServer(t, WithUsage(agg))

	status, body := fetchUsage(t, ts.URL, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Buckets) != 10 {
		t.Errorf("default window should be 10 buckets, got %d", len(snap.Buckets))
	}
	if snap.BucketSeconds != 60 {
		t.Errorf("bucketSeconds = %d, want 60", snap.BucketSeconds)
	}
	if snap.Priced {
		t.Error("priced = true when no request carried a cost event")
	}
}

// Query parameters must actually reach Snapshot — a handler that parsed them and
// then ignored them would pass every validation test while serving the default
// view.
func TestHandleUsage_ParamsReachSnapshot(t *testing.T) {
	agg := usage.New()
	ts, store := newTestServer(t, WithUsage(agg))
	store.AddRecorder(agg)

	store.Append("alice", pipeline.SessionEvent{
		At:         time.Now(),
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		StatusCode: 200,
		Duration:   time.Second,
		Inference:  &pipeline.InferenceExtension{Model: "claude-sonnet-5", TotalTokens: 300},
	})

	// resolution: 1h at 5m must fold to 12 buckets, not 60.
	status, body := fetchUsage(t, ts.URL, "?window=1h&resolution=5m")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Buckets) != 12 || snap.BucketSeconds != 300 {
		t.Errorf("1h@5m gave %d buckets at %ds, want 12 at 300s",
			len(snap.Buckets), snap.BucketSeconds)
	}

	// group: the series must be keyed by the requested dimension.
	_, body = fetchUsage(t, ts.URL, "?group=method")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Group != usage.GroupMethod {
		t.Errorf("group = %q, want method", snap.Group)
	}
	found := false
	for _, b := range snap.Buckets {
		if _, ok := b.Series["claude-sonnet-5"]; ok {
			found = true
		}
	}
	if !found {
		t.Error("group=method did not key the series by model")
	}

	// session: scoping must filter, and echo back which session it covers.
	_, body = fetchUsage(t, ts.URL, "?session=alice")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Session != "alice" {
		t.Errorf("session = %q, want alice", snap.Session)
	}
	if snap.Totals.Tokens != 300 {
		t.Errorf("alice tokens = %d, want 300", snap.Totals.Tokens)
	}

	// Fresh value: Counts fields are omitempty, so an all-zero Totals is absent
	// from the JSON entirely. Decoding into the struct reused above would leave
	// alice's 300 in place and the assertion would pass for the wrong reason.
	var bobSnap usage.Snapshot
	_, body = fetchUsage(t, ts.URL, "?session=bob")
	if err := json.Unmarshal([]byte(body), &bobSnap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bobSnap.Totals.Tokens != 0 {
		t.Errorf("bob should have no traffic, got %d tokens", bobSnap.Totals.Tokens)
	}
}

func TestHandleUsage_BadParams400(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	for _, q := range []string{
		"?window=30s",               // finer than a bucket
		"?window=7h",                // beyond retention
		"?window=90s",               // not a multiple
		"?window=nonsense",          // unparseable
		"?group=bogus",              // unknown grouping
		"?window=1h&resolution=90s", // resolution not a multiple
		"?window=1h&resolution=2h",  // resolution wider than the window
		"?resolution=30s",           // finer than storage
	} {
		status, body := fetchUsage(t, ts.URL, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %q)", q, status, body)
		}
		if !strings.Contains(body, `"error"`) {
			t.Errorf("%s: body is not a JSON error: %q", q, body)
		}
	}
}

// The endpoint is unauthenticated, so an error body must never reflect
// caller-supplied bytes back — that is a reflection primitive.
func TestHandleUsage_ErrorsDoNotEchoInput(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	const probe = "<script>alert(1)</script>"

	for _, q := range []string{"?group=" + probe, "?window=" + probe, "?resolution=" + probe} {
		status, body := fetchUsage(t, ts.URL, q)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, status)
		}
		if strings.Contains(body, "script") {
			t.Errorf("error body echoes caller input: %q", body)
		}
	}
}

// An over-long session id is rejected rather than truncated or echoed. Bounded
// at the store's own cap so the two cannot drift.
func TestHandleUsage_SessionIDLengthCap(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	ok := strings.Repeat("a", session.MaxSessionIDLen)
	if status, body := fetchUsage(t, ts.URL, "?session="+ok); status != http.StatusOK {
		t.Errorf("id at exactly the cap should be accepted, got %d: %s", status, body)
	}

	tooLong := strings.Repeat("a", session.MaxSessionIDLen+1)
	status, body := fetchUsage(t, ts.URL, "?session="+tooLong)
	if status != http.StatusBadRequest {
		t.Errorf("over-long id: status = %d, want 400", status)
	}
	if strings.Contains(body, tooLong) {
		t.Error("error body echoes the over-long id back")
	}
}

// inferenceEventForAPI builds a response event carrying an explicit token split.
//
// A local copy of authlib/usage's own inferenceEvent: that one is unexported and
// this is a different package, so there is nothing to import.
func inferenceEventForAPI(model string, in, cacheRead, cacheWrite, out int) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:         time.Now(),
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		StatusCode: 200,
		Host:       "gw.example.com",
		Inference: &pipeline.InferenceExtension{
			Model:            model,
			InputTokens:      in,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
			OutputTokens:     out,
			TotalTokens:      in + cacheRead + cacheWrite + out,
		},
	}
}

// The handler delegates group parsing to usage.ParseGroup, so the axes a cost
// table needs are served with no code change here. This pins that: a future
// refactor that reintroduced a local allow-list would silently drop the new
// groupings while every other test in this file kept passing.
func TestHandleUsage_AcceptsModelAndEndpointGroups(t *testing.T) {
	for _, group := range []string{"model", "endpoint", "method", "status", "plugin", "none", ""} {
		t.Run("group="+group, func(t *testing.T) {
			ts, _ := newTestServer(t, WithUsage(usage.New()))

			status, body := fetchUsage(t, ts.URL, "?group="+group)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", status, body)
			}
			var snap usage.Snapshot
			if err := json.Unmarshal([]byte(body), &snap); err != nil {
				t.Fatalf("decode: %v", err)
			}
		})
	}
}

// A deliberate DUPLICATE of TestHandleUsage_ErrorsDoNotEchoInput's group case, kept
// only so the group parameter's no-reflection guarantee is asserted under a name
// that says so — ParseGroup's error string is the one most likely to be reworded as
// groupings are added, and this is what a reworder will grep for.
//
// It adds no coverage: percent-encoding the probe changes nothing, because both
// tests read through r.URL.Query().Get, which decodes. Delete this rather than the
// broader test if one has to go.
func TestHandleUsage_RejectsUnknownGroupWithoutReflectingIt(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))

	status, body := fetchUsage(t, ts.URL, "?group=%3Cscript%3E")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if strings.Contains(body, "script") {
		t.Errorf("response reflects caller input: %s", body)
	}
}

// The whole point of the schema rule is that a consumer reads the same field
// names the parser published. Assert on the JSON, not the struct.
//
// Presence is a real assertion rather than a trivial one because every split
// field is omitempty: a name only reaches the wire if the aggregate actually
// carried a non-zero value for it. So this fails both if a field is renamed and
// if foldInto stops populating it from the event.
func TestHandleUsage_SplitFieldsAppearOnTheWire(t *testing.T) {
	agg := usage.New()
	ts, store := newTestServer(t, WithUsage(agg))
	store.AddRecorder(agg)

	store.Append("s1", inferenceEventForAPI("claude-opus-5", 10, 2000, 50, 30))

	status, body := fetchUsage(t, ts.URL, "?session=s1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", status, body)
	}
	for _, field := range []string{"inputTokens", "cacheReadTokens", "cacheWriteTokens", "outputTokens"} {
		if !strings.Contains(body, field) {
			t.Errorf("response body has no %q field: %s", field, body)
		}
	}
}

// ledgerWithOneCostedMinute builds a cost ledger holding exactly one CLOSED minute
// carrying a settled cost, at the instant `at`.
//
// Closed, not open: the writer keeps the open minute in memory on purpose, so a row
// only reaches disk once the minute rolls or Flush is called. A test that skipped
// the flush would assert against an empty ledger and read as a routing bug.
func ledgerWithOneCostedMinute(t *testing.T, at time.Time, host, model string, costUSD float64) *costledger.Writer {
	t.Helper()
	led := newTestLedger(t, at)
	recordCostedMinute(t, led, at, host, model, costUSD)
	if err := led.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return led
}

// newTestLedger opens an empty ledger with its clock pinned to at.
func newTestLedger(t *testing.T, at time.Time) *costledger.Writer {
	t.Helper()
	led, err := costledger.New(t.TempDir(), costledger.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("costledger.New: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return led
}

// recordCostedMinute feeds one settled-cost inference response to a ledger, without
// flushing — so the caller chooses whether the minute is open or closed.
func recordCostedMinute(t *testing.T, led *costledger.Writer, at time.Time, host, model string, costUSD float64) {
	t.Helper()
	rec, err := json.Marshal(costevent.Event{
		CostUSD: costUSD, Settled: true,
		Source: costevent.SourceUsageFallback, Provenance: "bundled",
	})
	if err != nil {
		t.Fatalf("marshal cost record: %v", err)
	}
	led.Record("s1", &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: host,
		Inference: &pipeline.InferenceExtension{
			Model: model, InputTokens: 100, OutputTokens: 50, TotalTokens: 150, PresentKinds: 0b1001,
		},
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	})
}

func TestHandleUsage_TodayIsServedFromTheLedger(t *testing.T) {
	// A closed minute on disk, plus an empty ring, so a non-zero total can only
	// have come from the ledger.
	led := ledgerWithOneCostedMinute(t, time.Now().Add(-2*time.Minute), "gw", "opus", 0.25)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if snap.Window != "today" {
		t.Errorf("window = %q, want \"today\" echoed back", snap.Window)
	}
	if snap.Totals.CostMicros == 0 {
		t.Error("CostMicros = 0; the ledger row did not reach the response")
	}
	if !snap.Priced {
		t.Error("priced = false for a ledger-sourced total")
	}
	// One bucket spanning the window, and BucketSeconds says so, so a client cannot
	// mistake a whole-window total for a fine series.
	if len(snap.Buckets) != 1 {
		t.Errorf("got %d buckets, want 1 spanning the window", len(snap.Buckets))
	}
	if snap.BucketSeconds <= int(usage.BucketWidth.Seconds()) {
		t.Errorf("bucketSeconds = %d, want the whole window's span", snap.BucketSeconds)
	}
}

// The open minute must reach the response. Nothing else supplies it: the day files
// hold closed minutes only, so a session whose whole conversation fit inside one
// minute has zero rows on disk — and "today" would have reported priced:false over
// real spend, which the CLI renders as "cost unavailable".
func TestHandleUsage_TodayIncludesTheStillOpenMinute(t *testing.T) {
	now := time.Now()
	led := newTestLedger(t, now)
	recordCostedMinute(t, led, now, "gw", "opus", 0.25) // no Flush: the minute is open
	// An empty ring, so a non-zero total can only have come from the ledger's own
	// in-memory half rather than from the aggregator.
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if snap.Window != "today" {
		t.Errorf("window = %q, want \"today\"", snap.Window)
	}
	if snap.Totals.CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want 250000 from the open minute", snap.Totals.CostMicros)
	}
	if !snap.Priced {
		t.Error("priced = false while the ledger holds a priced minute in memory")
	}
}

// And it must be counted ONCE. Same spend, asked for either side of the flush that
// moves it from memory to disk: a total that doubled would mean both halves claimed
// the minute.
func TestHandleUsage_TodayCountsTheOpenMinuteOnceAcrossAFlush(t *testing.T) {
	now := time.Now()
	led := newTestLedger(t, now)
	recordCostedMinute(t, led, now, "gw", "opus", 0.25)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	readTotal := func(when string) int64 {
		t.Helper()
		status, body := fetchUsage(t, ts.URL, "?window=today")
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", when, status, body)
		}
		var snap usage.Snapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatalf("%s: decode: %v", when, err)
		}
		return snap.Totals.CostMicros
	}

	open := readTotal("while open")
	if err := led.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	closed := readTotal("after the flush")

	if open != 250_000 || closed != 250_000 {
		t.Errorf("total was %d while open and %d once written; want 250000 both times "+
			"(500000 after the flush would mean the minute was counted twice)", open, closed)
	}
}

func TestHandleUsage_TodayWithoutALedgerDegradesAndSaysSo(t *testing.T) {
	// Kubernetes has no ledger by design. A 400 would make the abctl cost view
	// fail there rather than showing what IS available, so the handler serves the
	// ring's maximum window and reports the window it actually served.
	ts, _ := newTestServer(t, WithUsage(usage.New())) // no ledger

	status, body := fetchUsage(t, ts.URL, "?window=today")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if snap.Window == "today" {
		t.Error("window = \"today\" but no ledger exists; the response must name the window actually served")
	}
	if snap.Window != usage.MaxWindow.String() {
		t.Errorf("window = %q, want the ring maximum %q", snap.Window, usage.MaxWindow)
	}
}

func TestHandleUsage_SevenDaysIsServedFromTheLedger(t *testing.T) {
	// Same shape as today, over a span the ring cannot cover at all: six hours of
	// buckets can never answer for a row written two days ago.
	led := ledgerWithOneCostedMinute(t, time.Now().Add(-48*time.Hour), "gw", "opus", 1.50)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=7d")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if snap.Window != "7d" {
		t.Errorf("window = %q, want \"7d\"", snap.Window)
	}
	if snap.Totals.CostMicros != 1_500_000 {
		t.Errorf("CostMicros = %d, want 1500000 from a two-day-old row", snap.Totals.CostMicros)
	}
}

func TestHandleUsage_UnknownWindowStillDoesNotEchoInput(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	status, body := fetchUsage(t, ts.URL, "?window=%3Cscript%3E")

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if strings.Contains(body, "script") {
		t.Errorf("response reflects caller input: %s", body)
	}
}

func TestHandleUsage_LedgerBackedWindowGroupsByModel(t *testing.T) {
	// The Cost pane's by-model table must work over "today", not only over the
	// ring's windows — otherwise the breakdown silently covers a different span
	// from the total above it.
	led := ledgerWithOneCostedMinute(t, time.Now().Add(-2*time.Minute), "gw", "opus", 0.25)
	ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

	status, body := fetchUsage(t, ts.URL, "?window=today&group=model")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var snap usage.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(snap.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(snap.Buckets))
	}
	got := snap.Buckets[0].Series["opus"]
	if got.CostMicros != 250_000 {
		t.Errorf("series[opus].CostMicros = %d, want 250000", got.CostMicros)
	}
	// The breakdown accounts for the total it sits under.
	if got.CostMicros != snap.Totals.CostMicros {
		t.Errorf("series sums to %d but totals is %d", got.CostMicros, snap.Totals.CostMicros)
	}
}

// A symbolic window with session= is refused, not quietly answered with
// all-sessions data under a session label. See the guard in handleUsage for why the
// refusal is unconditional.
func TestHandleUsage_SessionWithSymbolicWindowIsRejected(t *testing.T) {
	led := ledgerWithOneCostedMinute(t, time.Now().Add(-2*time.Minute), "gw", "opus", 0.25)
	for _, srvName := range []string{"with ledger", "without ledger"} {
		opts := []Option{WithUsage(usage.New())}
		if srvName == "with ledger" {
			opts = append(opts, WithCostLedger(led))
		}
		ts, _ := newTestServer(t, opts...)
		for _, window := range []string{"today", "7d"} {
			status, body := fetchUsage(t, ts.URL, "?window="+window+"&session=s1")
			if status != http.StatusBadRequest {
				t.Errorf("%s, window=%s: status = %d, want 400: %s", srvName, window, status, body)
			}
			if !strings.Contains(body, "session") {
				t.Errorf("%s, window=%s: error does not name the problem: %s", srvName, window, body)
			}
		}
	}
}

// A duration window with session= keeps working. The rejection above must not have
// widened into "session is unsupported".
func TestHandleUsage_SessionWithDurationWindowStillWorks(t *testing.T) {
	ts, _ := newTestServer(t, WithUsage(usage.New()))
	status, body := fetchUsage(t, ts.URL, "?window=10m&session=s1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
}
