package usage

import (
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
