package usage

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// mergeSeries sums every bucket's Series into one map, so a test can assert on a
// window total without caring which bucket a record landed in.
//
// Delegates to Counts.Add rather than hand-summing the fields it happens to care
// about: a helper that lists fields is a helper that goes stale the next time one
// is added, which is the bug Add is exported to prevent.
func mergeSeries(buckets []Bucket) map[string]Counts {
	out := map[string]Counts{}
	for _, b := range buckets {
		for k, v := range b.Series {
			cur := out[k]
			cur.Add(v)
			out[k] = cur
		}
	}
	return out
}

func TestSnapshot_GroupModelReturnsTheModelSeries(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("claude-opus-5", 10, 0, 0, 5, 0, 0b1001))
	a.Record("s1", inferenceEvent("claude-haiku-4-5", 20, 0, 0, 7, 0, 0b1001))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupModel)

	series := mergeSeries(snap.Buckets)
	if len(series) != 2 {
		t.Fatalf("series has %d keys (%v), want 2", len(series), series)
	}
	if got := series["claude-opus-5"].InputTokens; got != 10 {
		t.Errorf("claude-opus-5 InputTokens = %d, want 10", got)
	}
	if got := series["claude-haiku-4-5"].InputTokens; got != 20 {
		t.Errorf("claude-haiku-4-5 InputTokens = %d, want 20", got)
	}
}

// PresentKinds has to be right per SERIES ENTRY, not merely in the window total.
// The blank-column case the field exists for is a by-model read: one model that
// reports cache counters and one that does not must carry DIFFERENT flags under the
// same grouping, or a reader cannot tell an empty CACHE-WRITE cell meaning "this
// model wrote no cache" from one meaning "this model never reports it".
//
// It is carried today because foldInto passes `one` whole to addLabel, but nothing
// asserted it at this level: Totals would stay green if a series entry lost the
// field or inherited another entry's bits.
func TestSnapshot_PresentKindsIsPerSeriesEntry(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("reports-cache", 10, 200, 5, 5, 0, 0b1111))
	a.Record("s1", inferenceEvent("no-cache-fields", 10, 0, 0, 5, 0, 0b1001))

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupModel).Buckets)

	if got := series["reports-cache"].PresentKinds; got != 0b1111 {
		t.Errorf("reports-cache PresentKinds = %#b, want %#b", got, 0b1111)
	}
	if got := series["no-cache-fields"].PresentKinds; got != 0b1001 {
		t.Errorf("no-cache-fields PresentKinds = %#b, want %#b — one model must not inherit another's reported kinds", got, 0b1001)
	}
}

func TestSnapshot_GroupMethodIsAnAliasForModel(t *testing.T) {
	// group=method is on the wire today and tui/usage_pane.go's cycleGroup passes
	// it, so it must keep working -- and it must return the SAME series as
	// group=model, because it was already the model series under a wrong name.
	a := New()
	a.Record("s1", inferenceEvent("claude-opus-5", 10, 0, 0, 5, 0, 0b1001))

	byModel := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupModel).Buckets)
	byMethod := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupMethod).Buckets)

	if len(byModel) != len(byMethod) {
		t.Fatalf("model series has %d keys, method series has %d; want identical", len(byModel), len(byMethod))
	}
	for k, v := range byModel {
		if byMethod[k] != v {
			t.Errorf("key %q: model = %+v, method = %+v; want identical", k, v, byMethod[k])
		}
	}
}

func TestParseGroup_AcceptsModelAndEndpointAndStillAcceptsMethod(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Group
	}{
		{"", GroupNone},
		{"none", GroupNone},
		{"model", GroupModel},
		{"method", GroupMethod},
		{"endpoint", GroupEndpoint},
		{"status", GroupStatus},
		{"plugin", GroupPlugin},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseGroup(tc.in)
			if err != nil {
				t.Fatalf("ParseGroup(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseGroup(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseGroup_RejectsUnknownWithoutEchoingInput(t *testing.T) {
	// The message crosses an UNAUTHENTICATED endpoint. Reflecting caller bytes
	// into a response body hands out a reflection primitive, so the error names
	// the valid set instead of quoting what it got.
	const attack = "<script>alert(1)</script>"
	_, err := ParseGroup(attack)
	if err == nil {
		t.Fatal("ParseGroup accepted an unknown group")
	}
	if strings.Contains(err.Error(), attack) || strings.Contains(err.Error(), "script") {
		t.Errorf("error echoes caller input: %q", err.Error())
	}
	for _, want := range []string{"model", "endpoint", "status", "plugin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the valid value %q", err.Error(), want)
		}
	}
}

func TestSnapshot_GroupEndpointBreaksDownByHost(t *testing.T) {
	a := New()
	e1 := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e1.Host = "gw-a.example.com"
	e2 := inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001)
	e2.Host = "gw-b.example.com"
	a.Record("s1", e1)
	a.Record("s1", e2)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupEndpoint).Buckets)

	if got := series["gw-a.example.com"].InputTokens; got != 10 {
		t.Errorf("gw-a InputTokens = %d, want 10", got)
	}
	if got := series["gw-b.example.com"].InputTokens; got != 20 {
		t.Errorf("gw-b InputTokens = %d, want 20", got)
	}
}

func TestSnapshot_GroupEndpointOmitsEventsWithNoHost(t *testing.T) {
	// Host is empty when the listener did not populate it. An empty-string key in
	// a breakdown table renders as a blank row that looks like a bug.
	a := New()
	e := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e.Host = ""
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupEndpoint).Buckets)

	if _, ok := series[""]; ok {
		t.Error(`series has an "" key; an unknown host must be omitted, not shown as a blank row`)
	}
}

func TestSnapshot_GroupSessionBreaksDownBySession(t *testing.T) {
	a := New()
	a.Record("sess-a", inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001))
	a.Record("sess-b", inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001))

	// The ALL-sessions ring must carry the breakdown: one request has to answer
	// for every session, or a sessions list costs one request per row.
	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)

	if got := series["sess-a"].InputTokens; got != 10 {
		t.Errorf("sess-a InputTokens = %d, want 10", got)
	}
	if got := series["sess-b"].InputTokens; got != 20 {
		t.Errorf("sess-b InputTokens = %d, want 20", got)
	}
}

func TestSnapshot_GroupSessionOmitsAnEmptyID(t *testing.T) {
	a := New()
	a.Record("", inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001))
	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)
	if _, ok := series[""]; ok {
		t.Error(`series has an "" key; an unattributed event must not render as a blank row`)
	}
}

// The per-session ring carries the label too, redundant though it is there. Not
// for its own sake: it pins that foldInto records the id on WHICHEVER ring it is
// folding, so the uniform call site cannot be "optimised" into an all-ring-only
// conditional that a later reader would have to re-derive.
func TestSnapshot_GroupSessionOnAScopedSnapshotNamesOnlyThatSession(t *testing.T) {
	a := New()
	a.Record("sess-a", inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001))
	a.Record("sess-b", inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001))

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "sess-a", GroupSession).Buckets)

	if len(series) != 1 {
		t.Fatalf("scoped series has %d keys (%v), want just sess-a", len(series), series)
	}
	if got := series["sess-a"].InputTokens; got != 10 {
		t.Errorf("sess-a InputTokens = %d, want 10", got)
	}
}

// Session ids are request-derived (the A2A contextId, via
// reverseproxy.inboundSessionID), so the bound that protects every other label map
// has to protect this one. Without it a client varying the contextId every turn
// grows a retained map entry and a retained string per request, in a ring that
// frees a slot only a full lap later.
func TestSnapshot_GroupSessionIsBoundedLikeEveryOtherLabel(t *testing.T) {
	a := New()
	for i := 0; i < maxLabelsPerBucket+50; i++ {
		a.Record(fmt.Sprintf("sess-%03d", i), inferenceEvent("m", 1, 0, 0, 1, 0, 0b1001))
	}

	snap := a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession)
	for _, b := range snap.Buckets {
		if len(b.Series) > maxLabelsPerBucket {
			t.Fatalf("bucket carries %d session labels, want at most %d", len(b.Series), maxLabelsPerBucket)
		}
	}
	series := mergeSeries(snap.Buckets)
	if _, ok := series[overflowLabel]; !ok {
		t.Errorf("no %q key past the cap; the excess was dropped silently instead of being named", overflowLabel)
	}
	// The totals must still reconcile with the sum of the series, overflow
	// included: that is the whole reason overflow is named rather than dropped.
	var sum int64
	for _, c := range series {
		sum += c.Requests
	}
	if sum != snap.Totals.Requests {
		t.Errorf("series requests sum to %d, totals say %d; the overflow key is not absorbing the excess", sum, snap.Totals.Requests)
	}
}

func TestParseGroup_AcceptsSession(t *testing.T) {
	g, err := ParseGroup("session")
	if err != nil {
		t.Fatalf("ParseGroup(\"session\") errored: %v", err)
	}
	if g != GroupSession {
		t.Errorf("ParseGroup(\"session\") = %q, want %q", g, GroupSession)
	}
}

// The error names the valid set instead of echoing the caller's input (see
// ParseGroup). A new grouping the message does not list is a grouping an operator
// cannot discover from the only place the endpoint tells them.
func TestParseGroup_ErrorNamesTheSessionGroup(t *testing.T) {
	_, err := ParseGroup("nonsense")
	if err == nil {
		t.Fatal("ParseGroup accepted a bogus group")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Errorf("error %q does not mention the session group", err)
	}
}

func TestParseWindowSpec_FixedLengths(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	for _, in := range []string{"10m", "1h", "6h"} {
		t.Run(in, func(t *testing.T) {
			got, err := ParseWindowSpec(in, now)
			if err != nil {
				t.Fatalf("ParseWindowSpec(%q): %v", in, err)
			}
			if got.Symbolic() {
				t.Errorf("%q reported Symbolic; want a fixed length", in)
			}
			if got.Label != in {
				t.Errorf("Label = %q, want %q", got.Label, in)
			}
		})
	}
}

// testZone is a fixed non-UTC zone for the day-boundary tests.
//
// Not time.Local, which is what the first version of these tests used on both sides
// of the assertion. On a host where time.Local IS UTC — the default in most CI
// containers — that made the guard vacuous: an implementation reading
// time.Date(..., time.UTC) passed, because the expectation was computed in the same
// zone as the input. Pinning a zone seven hours off UTC means midnight here is 07:00
// UTC, so a UTC reading lands on the wrong instant on every machine.
var testZone = time.FixedZone("test", -7*3600)

func TestParseWindowSpec_TodayIsLocalMidnightToNow(t *testing.T) {
	// The caller's zone, not UTC. A laptop crossing a timezone must not have its day
	// reset mid-afternoon, and a UTC day would do exactly that.
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, testZone)
	got, err := ParseWindowSpec("today", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if !got.Symbolic() {
		t.Fatal("today reported a fixed length; want symbolic")
	}
	wantFrom := time.Date(2026, 9, 14, 0, 0, 0, 0, testZone)
	if !got.From.Equal(wantFrom) {
		t.Errorf("From = %v, want midnight in the caller's zone %v", got.From, wantFrom)
	}
	// The span is the load-bearing assertion, because it is the one a UTC reading gets
	// wrong: 15:30 minus midnight is 15h30m in the caller's zone and 22h30m if the
	// boundary is taken in UTC.
	if d := got.To.Sub(got.From); d != 15*time.Hour+30*time.Minute {
		t.Errorf("span = %v, want 15h30m; a UTC day boundary would give 22h30m", d)
	}
	if !got.To.Equal(now) {
		t.Errorf("To = %v, want now %v", got.To, now)
	}
	if got.Label != "today" {
		t.Errorf("Label = %q, want \"today\"", got.Label)
	}
}

// Just after midnight in a non-UTC zone is where a UTC boundary is not merely a
// different length but a different DAY: 00:30 at UTC-7 is 07:30 UTC on the same date,
// so a UTC reading reports seven and a half hours of "today" — most of it yesterday
// evening's spend.
func TestParseWindowSpec_TodayJustAfterMidnightInANonUTCZone(t *testing.T) {
	now := time.Date(2026, 9, 14, 0, 30, 0, 0, testZone)
	got, err := ParseWindowSpec("today", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if d := got.To.Sub(got.From); d != 30*time.Minute {
		t.Errorf("span = %v, want 30m; a UTC boundary would give 7h30m of someone else's day", d)
	}
}

func TestParseWindowSpec_TodayJustAfterMidnightIsAShortWindow(t *testing.T) {
	// The boundary case: at 00:05, "today" is five minutes, not 24 hours. A
	// fixed-length reading would report yesterday evening's spend as today's.
	now := time.Date(2026, 9, 14, 0, 5, 0, 0, time.Local)
	got, err := ParseWindowSpec("today", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if d := got.To.Sub(got.From); d != 5*time.Minute {
		t.Errorf("span = %v, want 5m", d)
	}
}

func TestParseWindowSpec_SevenDaysIsRollingNotCalendar(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	got, err := ParseWindowSpec("7d", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if d := got.To.Sub(got.From); d != 7*24*time.Hour {
		t.Errorf("span = %v, want exactly 7x24h (rolling, not calendar)", d)
	}
	if !got.Symbolic() {
		t.Error("7d reported a fixed length; the ring cannot serve it, only the ledger can")
	}
	if got.Label != "7d" {
		t.Errorf("Label = %q, want \"7d\"", got.Label)
	}
}

func TestParseWindowSpec_RejectsUnknownWithoutEchoingInput(t *testing.T) {
	const attack = "<script>alert(1)</script>"
	_, err := ParseWindowSpec(attack, time.Now())
	if err == nil {
		t.Fatal("accepted an unknown window")
	}
	if strings.Contains(err.Error(), "script") {
		t.Errorf("error echoes caller input: %q", err.Error())
	}
}

func TestParseWindow_StillRejectsSymbolicWindows(t *testing.T) {
	// A duration caller cannot express "today". Refusing beats silently
	// substituting a length, which would report a number for a span nobody asked
	// for.
	for _, in := range []string{"today", "7d"} {
		if _, err := ParseWindow(in); err == nil {
			t.Errorf("ParseWindow(%q) succeeded; want an error", in)
		}
	}
}
