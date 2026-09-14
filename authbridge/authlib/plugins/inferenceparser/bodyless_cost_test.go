package inferenceparser

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// A body-less response is not a free response. The gateway reports what it charged
// in a RESPONSE HEADER, so every path that gives up on the body must still settle
// the cost — otherwise a LiteLLM-costed response whose body was empty or
// unrecognised is spend that reaches neither the aggregator nor the budget ledger.
//
// Each of the parser's three no_response_body paths is exercised twice: once with a
// positive header (must charge) and once with no header at all (must publish
// nothing, rather than a settled zero for every body-less response). Both halves
// also assert the Skip row survives — the fix must not trade the diagnostic for the
// charge.

// bodylessRates prices every tier at 1 micro-dollar per token.
//
// A REAL table is injected deliberately, not a nil resolver. With rates in hand the
// "no header" rows prove that publication is suppressed because pricing.Cost refuses
// an all-zero Usage as unpriced, not merely because nothing could price anything.
func bodylessRates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	for _, tier := range []pricing.Tier{
		pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput,
	} {
		r.Base[tier], r.Set[tier] = 1e-6, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{
		{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// publishedCost reads the cost record off pctx, if one was published.
//
// Reads the canonical key rather than the legacy plugin-name alias: costing.Publish
// writes both, and asserting on the concern-named one is what keeps this test honest
// about which key a consumer is meant to read.
func publishedCost(t *testing.T, pctx *pipeline.Context) (costevent.Event, bool) {
	t.Helper()
	raw, ok := pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix]
	if !ok {
		return costevent.Event{}, false
	}
	ev, ok := raw.(costevent.Event)
	if !ok {
		t.Fatalf("cost record has type %T, want costevent.Event", raw)
	}
	return ev, true
}

// skipRows counts the no_response_body Skip invocations recorded on pctx.
func skipRows(pctx *pipeline.Context) int {
	if pctx.Extensions.Invocations == nil {
		return 0
	}
	var n int
	for _, list := range [][]pipeline.Invocation{
		pctx.Extensions.Invocations.Outbound, pctx.Extensions.Invocations.Inbound,
	} {
		for _, iv := range list {
			if iv.Action == pipeline.ActionSkip && iv.Reason == "no_response_body" {
				n++
			}
		}
	}
	return n
}

// bodylessSite is one of the parser's three no_response_body paths, named and
// driven. Named so a failure — including a mutation that neuters exactly one
// settleCost call — says which site is uncovered rather than only that something is.
type bodylessSite struct {
	name string
	// stream is what the REQUEST asked for, which is what selects the arm inside
	// OnResponseFrame. It is not a property of the response.
	stream bool
	// drive delivers a body-less response to the site under test.
	drive func(p *InferenceParser, pctx *pipeline.Context)
}

func bodylessSites() []bodylessSite {
	return []bodylessSite{{
		// plugin.go OnResponse: the legacy buffered hook, reachable from a pipeline
		// that calls OnResponse directly rather than through RunResponse.
		name: "OnResponse/empty-ResponseBody",
		drive: func(p *InferenceParser, pctx *pipeline.Context) {
			p.OnResponse(context.Background(), pctx)
		},
	}, {
		// plugin.go OnResponseFrame, application/json one-shot arm. This is the arm
		// extproc's header-only branch lands on for a non-streaming request.
		name: "OnResponseFrame/json-one-shot-empty-frame",
		drive: func(p *InferenceParser, pctx *pipeline.Context) {
			p.OnResponseFrame(context.Background(), pctx, nil, true)
		},
	}, {
		// plugin.go OnResponseFrame, streaming arm: a stream that finalized with no
		// completion, no finish reason, no usage and no tool calls.
		name:   "OnResponseFrame/empty-stream",
		stream: true,
		drive: func(p *InferenceParser, pctx *pipeline.Context) {
			p.OnResponseFrame(context.Background(), pctx, nil, true)
		},
	}}
}

// bodylessCtx builds a response context with no body and the given cost header.
func bodylessCtx(costHeader string, stream bool) *pipeline.Context {
	h := http.Header{}
	// A header-only reply to a streaming request is not itself an event stream, so
	// application/json is the honest content type on all three sites. It also keeps
	// a zero header meaningful, which text/event-stream would not — see
	// costing.IsEventStream.
	h.Set("Content-Type", "application/json")
	if costHeader != "" {
		h.Set(costing.ResponseCostHeader, costHeader)
	}
	pctx := &pipeline.Context{
		Direction:       pipeline.Outbound,
		Host:            "gw.internal",
		Path:            "/v1/messages",
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:  "claude-opus-5",
			Stream: stream,
		}},
	}
	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)
	return pctx
}

// TestBodylessResponse_PositiveCostHeaderIsCharged is the regression proper: every
// no_response_body path must publish the gateway's figure.
//
// One subtest per site, so neutering one settleCost call fails exactly one row and
// the row's name says which site lost its charge.
func TestBodylessResponse_PositiveCostHeaderIsCharged(t *testing.T) {
	for _, site := range bodylessSites() {
		t.Run(site.name, func(t *testing.T) {
			p := NewInferenceParser()
			p.SetPricingResolver(bodylessRates(t))
			pctx := bodylessCtx("0.25", site.stream)

			site.drive(p, pctx)

			ev, ok := publishedCost(t, pctx)
			if !ok {
				t.Fatal("no cost record published: a positive gateway cost header on a body-less response is real spend and went unrecorded")
			}
			if ev.CostUSD != 0.25 {
				t.Errorf("CostUSD = %v, want the header's 0.25", ev.CostUSD)
			}
			if ev.Source != costevent.SourceGatewayHeader {
				t.Errorf("Source = %q, want %q", ev.Source, costevent.SourceGatewayHeader)
			}
			if !ev.Settled {
				t.Error("Settled = false; an authoritative gateway figure is settled")
			}
			if !ev.Priced() {
				t.Error("Priced() = false; the aggregator would fall through to its own rate table")
			}
			// The charge must not cost us the diagnostic. Exactly one Skip row: the
			// response row abctl pairs with the request row.
			if n := skipRows(pctx); n != 1 {
				t.Errorf("no_response_body Skip rows = %d, want 1", n)
			}
		})
	}
}

// TestBodylessResponse_NoCostHeaderPublishesNothing is the other half of the fix:
// settling on these paths must not manufacture a settled zero for every body-less
// response, which would count unpriced traffic as priced.
//
// The suppression is load-bearing on pricing.Cost refusing an all-zero Usage — rates
// ARE injected here, so nothing could price this except a rule that declines to.
func TestBodylessResponse_NoCostHeaderPublishesNothing(t *testing.T) {
	for _, site := range bodylessSites() {
		t.Run(site.name, func(t *testing.T) {
			p := NewInferenceParser()
			p.SetPricingResolver(bodylessRates(t))
			pctx := bodylessCtx("", site.stream)

			site.drive(p, pctx)

			if ev, ok := publishedCost(t, pctx); ok {
				t.Errorf("published %+v; a body-less response with no cost header has nothing to say, and a settled zero would report unpriced traffic as free", ev)
			}
			// Whatever settling decided, the pairing row is still owed.
			if n := skipRows(pctx); n != 1 {
				t.Errorf("no_response_body Skip rows = %d, want 1", n)
			}
		})
	}
}

// TestBodylessResponse_ZeroUsageIsUnpriced pins the property the fix above relies on,
// at the level it actually holds: pricing.Cost treats an all-zero Usage as UNPRICED,
// not as a cost of zero.
//
// Asserted directly rather than inferred from the parser's behaviour, because it is
// the reason settling on a body-less path is safe at all. If this ever became
// (0, true), every body-less response would publish a settled zero and the aggregate
// would count unpriced traffic as priced — so this test is the tripwire for that
// change, wherever it is made.
func TestBodylessResponse_ZeroUsageIsUnpriced(t *testing.T) {
	var r pricing.Rates
	for _, tier := range []pricing.Tier{
		pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput,
	} {
		r.Base[tier], r.Set[tier] = 1e-6, true
	}
	if micros, ok := pricing.Cost(r, pricing.Usage{}); ok {
		t.Errorf("pricing.Cost(rates, zero usage) = (%d, true), want ok=false: unknown usage is not a free request", micros)
	}
}
