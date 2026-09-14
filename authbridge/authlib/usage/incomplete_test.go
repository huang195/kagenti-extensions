package usage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// truncatedRespEvent is a response event whose prompt counts landed and whose output
// count never did — a stream that died before message_delta.
//
// PresentKinds carries KindInput|KindOutput with a zero output tally, which is what
// Anthropic really emits, so nothing in these tests can pass by reading the mask.
func truncatedRespEvent(host, model string, input int) *pipeline.SessionEvent {
	return &pipeline.SessionEvent{
		Phase:      pipeline.SessionResponse,
		Host:       host,
		StatusCode: 200,
		Inference: &pipeline.InferenceExtension{
			Model:        model,
			InputTokens:  input,
			PromptTokens: input,
			TotalTokens:  input,
			PresentKinds: 1 | 8, // KindInput | KindOutput
		},
	}
}

// withCostRecord attaches a published cost record to an event, the way the parser does.
func withCostRecord(t *testing.T, e *pipeline.SessionEvent, ev costevent.Event) *pipeline.SessionEvent {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	e.Plugins = map[string]json.RawMessage{costevent.Key: raw}
	return e
}

// An inexact figure is DISCLOSED, not adjusted. The dollars stay in CostMicros and the
// request stays in PricedRequests — both of those are true — and IncompleteRequests
// carries the caveat alongside.
func TestIncomplete_DisclosedNotDeducted(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	a.Record("s1", withCostRecord(t, truncatedRespEvent("gw.internal", "claude-opus-5", 1000),
		costevent.Event{
			CostUSD: 0.005, Source: costevent.SourceUsageFallback, Provenance: "configured",
			Settled: true, Incomplete: true, IncompleteReason: pricing.ReasonOutputUncounted,
		}))

	snap := snapshotOf(a, now)
	if want := int64(5_000); snap.Totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d: the money was really spent and refusing to count it understates spend by more than disclosing the floor does", snap.Totals.CostMicros, want)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1: this request WAS priced, and that counter answers coverage — a different question from exactness", snap.Totals.PricedRequests)
	}
	if snap.Totals.IncompleteRequests != 1 {
		t.Errorf("IncompleteRequests = %d, want 1: without this the total claims an exactness it does not have", snap.Totals.IncompleteRequests)
	}
	// A floor is not a pricing gap: no rate an operator could add would make it go away.
	if len(snap.UnpricedBy) != 0 {
		t.Errorf("UnpricedBy = %v, want empty: naming a floor there would point at a pricing entry that already exists", snap.UnpricedBy)
	}
	// Priced stays true, so a client renders the figure rather than "cost unavailable".
	if !snap.Priced {
		t.Error("Priced = false while CostMicros is non-zero; that breaks the flag's own contract")
	}
}

// THE TRAP this counter exists to avoid: PresentKinds is OR-folded across a bucket, so a
// bucket holding one truncated request and nine complete ones ORs in the other nine's
// Output bit and reads as fully exact. The folded mask answers "did any request here
// expose output", which is NOT "was every figure in this total exact".
//
// So the decision is made per request at settle time and COUNTED. This test is the proof
// that the count survives the fold where the mask cannot.
func TestIncomplete_SurvivesTheFoldWhereTheMaskCannot(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// Nine complete requests, each reporting output.
	for i := 0; i < 9; i++ {
		e := pricedRespEvent("gw.internal", "claude-opus-5", 100, 50)
		e.Inference.PresentKinds = 1 | 8
		e.Inference.FinishReason = "end_turn"
		a.Record("s1", withCostRecord(t, e, costevent.Event{
			CostUSD: 0.001, Source: costevent.SourceUsageFallback,
			Provenance: "configured", Settled: true,
		}))
	}
	// One truncated.
	a.Record("s1", withCostRecord(t, truncatedRespEvent("gw.internal", "claude-opus-5", 1000),
		costevent.Event{
			CostUSD: 0.005, Source: costevent.SourceUsageFallback, Provenance: "configured",
			Settled: true, Incomplete: true, IncompleteReason: pricing.ReasonOutputUncounted,
		}))

	snap := snapshotOf(a, now)
	if snap.Totals.Requests != 10 {
		t.Fatalf("Requests = %d, want 10", snap.Totals.Requests)
	}
	// The mask is now useless for this question, and that is the point.
	if snap.Totals.PresentKinds&8 == 0 {
		t.Fatal("fixture no longer ORs the Output bit; it does not reproduce the trap and the assertion below proves nothing")
	}
	if snap.Totals.IncompleteRequests != 1 {
		t.Errorf("IncompleteRequests = %d, want 1: the OR-folded PresentKinds reads as fully exact here, so a counter is the only thing that can carry this", snap.Totals.IncompleteRequests)
	}
	if snap.Totals.PricedRequests != 10 {
		t.Errorf("PricedRequests = %d, want 10", snap.Totals.PricedRequests)
	}
}

// The aggregator's OWN fallback figure gets the same test. There are two sources of cost
// in this package, and disclosing on one side of the fork only would leave the bug live
// in whichever way a figure happened to arrive.
func TestIncomplete_FallbackFigureIsDisclosedToo(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// No cost record at all: the aggregator prices it from its own table.
	a.Record("s1", truncatedRespEvent("gw.internal", "claude-opus-5", 1000))

	snap := snapshotOf(a, now)
	if want := int64(5_000); snap.Totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d", snap.Totals.CostMicros, want)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	if snap.Totals.IncompleteRequests != 1 {
		t.Error("IncompleteRequests = 0; the fallback priced a prompt-only figure and presented it as exact")
	}
}

// A complete figure must carry no caveat. A permanent warning with nothing to act on is
// what teaches an operator to ignore the one signal that matters.
func TestIncomplete_CompleteFigureCarriesNoCaveat(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	e := pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)
	e.Inference.PresentKinds = 1 | 8
	e.Inference.FinishReason = "end_turn"
	a.Record("s1", e)

	snap := snapshotOf(a, now)
	if snap.Totals.IncompleteRequests != 0 {
		t.Errorf("IncompleteRequests = %d, want 0", snap.Totals.IncompleteRequests)
	}
}

// Counts.Add must carry the new field. A previous copy of this summation in another
// module silently missed PricedRequests when it was added, under a comment explaining
// that every field had to be carried — so the field-by-field sum gets an assertion.
func TestIncomplete_AddCarriesTheCounter(t *testing.T) {
	c := Counts{Requests: 1, CostMicros: 10, PricedRequests: 1, IncompleteRequests: 1}
	c.Add(Counts{Requests: 2, CostMicros: 20, PricedRequests: 2, IncompleteRequests: 1})
	if c.IncompleteRequests != 2 {
		t.Errorf("IncompleteRequests = %d, want 2", c.IncompleteRequests)
	}
	// Summed alongside, never out of, PricedRequests: it is a subset disclosure.
	if c.PricedRequests != 3 {
		t.Errorf("PricedRequests = %d, want 3", c.PricedRequests)
	}
	if c.IncompleteRequests > c.PricedRequests {
		t.Error("IncompleteRequests exceeds PricedRequests; it is a SUBSET of it, and a client subtracting the two would go negative")
	}
}

// A totals-only gateway is approximate rather than partial, and the reason string is what
// lets a client tell a standing property of the gateway from a transient incident.
func TestIncomplete_TotalOnlyGatewayIsDisclosedAsApproximate(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// total_tokens only: no split, no presence bits.
	a.Record("s1", &pipeline.SessionEvent{
		Phase: pipeline.SessionResponse, Host: "gw.internal", StatusCode: 200,
		Inference: &pipeline.InferenceExtension{
			Model: "claude-opus-5", TotalTokens: 1000, FinishReason: "end_turn",
		},
	})

	snap := snapshotOf(a, now)
	if snap.Totals.PricedRequests != 1 {
		t.Fatalf("PricedRequests = %d, want 1: a bare total is still priced, approximately", snap.Totals.PricedRequests)
	}
	if snap.Totals.IncompleteRequests != 1 {
		t.Error("IncompleteRequests = 0; a figure modelled by attributing a bare total wholly to uncached input is not exact")
	}
}
