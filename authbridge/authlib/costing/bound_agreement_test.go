package costing

import (
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// TestSettle_PricedAgreesWithTheRecordItProduces is the second must-fix from review round 6.
//
// THE PRODUCER AND THE CONSUMER ANSWER THE SAME QUESTION IN TWO PLACES. Settle sets
// Settled.Priced; costevent.Event.Priced() decides whether that record reads as spend. They
// are meant to be the same predicate — costevent's own doctrine is that Micros and Priced must
// agree, or a consumer adds money to a total while counting the request as uncovered — but
// nothing made them agree, and they were validated against different bounds: the header arm
// accepted any finite non-negative float, while Priced() rejects anything the micros unit
// cannot hold. They disagreed over ($9.007 billion, +Inf), and on a PARSED endpoint the
// plausibility cap bails out before it could narrow that.
//
// BOTH HALVES OF THE DISAGREEMENT ARE REACHABLE, and they are reachable at once. Something
// gating on Settled.Priced charges the figure — litellm_budgettrack accumulates it into
// TotalSpend, which drives the HTTP 429 lockout — while everything reading the record through
// costevent files the same request as UNPRICED. Two headers of ~1.8e308 sum to +Inf, and the
// ledger's json.Marshal cannot write that at all: one response poisons the file.
//
// SO THE TEST IS THE AGREEMENT ITSELF, not a state per row. Asserting "1e10 is refused" would
// pin today's answer; asserting "the two predicates agree on every row" pins the invariant, and
// stays honest if a later change moves the bound. The rows exist to make the assertion reach
// the disputed range, from a plausible figure to the largest float there is.
func TestSettle_PricedAgreesWithTheRecordItProduces(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		parsed bool
	}{
		{"an ordinary figure", "0.25", true},
		{"a declared-free zero", "0", true},
		{"a negative figure", "-1", true},
		{"an unparseable string", "not-a-number", true},
		{"past the plausibility cap, unparsed", "1e9", false},
		{"past the plausibility cap, parsed", "1e9", true},
		// THE DISPUTED RANGE. Over the micros bound and under +Inf: finite, non-negative,
		// and on a parsed path the plausibility cap returns early without looking.
		{"past the micros bound, parsed", "1e10", true},
		{"the largest float there is, parsed", "1.7976931348623157e308", true},
		{"past the micros bound, unparsed", "1e10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := unparsedCostCtx(tc.header)
			if tc.parsed {
				pctx.Extensions.Inference = &pipeline.InferenceExtension{
					Model:        "claude-opus-5",
					InputTokens:  1000,
					OutputTokens: 500,
				}
			}

			got := Settle(pctx, rates(t))
			record := NewRecord(got, nil)

			if got.Priced != record.Priced() {
				t.Errorf("Settled.Priced = %v but the record it produced reads Priced() = %v (CostUSD %v): a consumer gating on one of these charges a figure the other files as unpriced",
					got.Priced, record.Priced(), got.CostUSD)
			}
			// AND THE FIGURE ITSELF, because agreeing on the verdict is not enough: a record
			// that says priced must carry a figure the unit can hold, or Micros zeroes it and
			// the two disagree one level down instead.
			if record.Priced() {
				if _, ok := pricing.MicrosFromUSD(record.CostUSD); !ok {
					t.Errorf("the record is priced at %v, which pricing.MicrosFromUSD cannot represent: Micros() returns 0 for it, so every micros consumer reads this charge as free",
						record.CostUSD)
				}
			}
		})
	}
}

// TestHeaderCost_PastTheMicrosBoundIsUnusable pins the state the fix chose, and why that one.
//
// UNUSABLE, NOT IMPLAUSIBLE. The two refusals are different claims. headerImplausible means a
// figure was on the wire, this process understood it, and declined to believe it — a fact an
// operator needs, so it is DISCLOSED on the record as costevent.RejectedImplausible. A figure
// past the micros bound is a different animal: it is not a price this system can represent at
// all, which puts it in the same class as a NaN or a garbage string, and those are noise. The
// header is unauthenticated, so treating unrepresentable input as noise rather than as a
// reportable event also keeps a hostile host from writing rows into an operator's disclosure
// stream at will.
//
// AND THE MODELLED FIGURE THEN WINS, which is the outcome that matters: on a parsed endpoint
// there is usage, so refusing the header does not lose the charge — it replaces a number
// nothing can hold with one the rate table produced. HasReported stays false deliberately, so
// the drift check cannot measure the table against a figure this package already refused.
func TestHeaderCost_PastTheMicrosBoundIsUnusable(t *testing.T) {
	const forged = "1e10"
	pctx := unparsedCostCtx(forged)
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model:        "claude-opus-5",
		InputTokens:  1000,
		OutputTokens: 500,
	}

	cost, state := headerCost(pctx)
	if state != headerUnusable {
		t.Errorf("state = %d for %s, want headerUnusable (%d)", state, forged, headerUnusable)
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0: a refused figure must not travel", cost)
	}

	got := Settle(pctx, rates(t))
	if !got.Priced || got.Source != costevent.SourceUsageFallback {
		t.Errorf("Priced = %v / Source = %q, want true / %q: the usage fallback is what makes refusing the header safe on a parsed path",
			got.Priced, got.Source, costevent.SourceUsageFallback)
	}
	if got.HasReported {
		t.Error("HasReported = true: the drift check would then compare the rate table against a figure this package refused, and report the table as stale")
	}
	if got.RejectedReason != "" {
		t.Errorf("RejectedReason = %q: unrepresentable input is noise, not a disclosure — see TestHeaderCost_ImplausibleIsItsOwnState for the refusal that IS disclosed", got.RejectedReason)
	}
}

// implausibleHalvesRates prices input and output at the highest per-token rate the plausibility
// derivation admits ($0.001), so that maxPlausibleTokens in each of the two tiers makes each
// HALF exactly the $10,000 ceiling and the whole request twice it.
func implausibleHalvesRates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = 1e-3, true
	r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = 1e-3, true
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// TestSettle_HalvesAreRefusedWhenTheirSumIs is suggestion 8 of review round 6.
//
// THE PLAUSIBILITY CAP IS PER Cost CALL, AND Settle MAKES THREE OF THEM — the whole request,
// the prompt half, the output half. So a request whose total is refused as impossible can have
// both of its halves come back priced, each one just under the ceiling, and the record then
// says two things that cannot both be true: unpriced, cost $0, prompt $10,000, output $10,000.
// Anything summing the halves — a request row plus a response row, which is exactly what those
// two fields exist for — charges the $20,000 the total refused.
//
// REFUSED TOGETHER, NOT DISCLOSED. Halving a forged figure does not make it two real ones, and
// there is no per-half reason field to carry a disclosure in, so the honest record is the one
// with no half figures on it at all.
//
// AND ONLY WHEN THEIR SUM IS THE PROBLEM, which is why the guard reads the sum rather than
// "was the whole priced". A single half surviving alone is deliberate and load-bearing: where
// the output tier has tokens and no rate, Cost refuses the whole request, and the prompt half is
// then the only figure anyone can attribute — see the comment above promptOnly.
func TestSettle_HalvesAreRefusedWhenTheirSumIs(t *testing.T) {
	const half = 1e7 * 1e-3 // maxPlausibleTokens at the ceiling rate: $10,000, the cap itself
	if !pricing.PlausibleRequestCostUSD(half) {
		t.Fatal("a half is already implausible on its own; this fixture would then prove nothing about the pair")
	}
	if pricing.PlausibleRequestCostUSD(2 * half) {
		t.Fatal("the pair is plausible; this fixture no longer builds the divergence it is about")
	}
	pctx := ctx(map[string]string{"Content-Type": "application/json"}, 1e7, 1e7)

	got := Settle(pctx, implausibleHalvesRates(t))

	if got.Priced || got.CostUSD != 0 {
		t.Errorf("Priced = %v / CostUSD = %v, want false / 0: the whole request is past the plausibility ceiling",
			got.Priced, got.CostUSD)
	}
	if got.HasPrompt || got.PromptUSD != 0 {
		t.Errorf("PromptUSD = %v (HasPrompt %v), want 0 / false: a request row carrying half of a figure this package refused is that figure, charged", got.PromptUSD, got.HasPrompt)
	}
	if got.HasOutput || got.OutputUSD != 0 {
		t.Errorf("OutputUSD = %v (HasOutput %v), want 0 / false", got.OutputUSD, got.HasOutput)
	}
	record := NewRecord(got, nil)
	if sum := record.PromptUSD + record.OutputUSD; sum != 0 {
		t.Errorf("the record's halves sum to %v while it reads Priced() = %v: a consumer adding the two rows charges what the total refused",
			sum, record.Priced())
	}
}

// TestSettle_OnePricedHalfSurvivesAlone is the control for the guard above, and the reason it
// reads the sum instead of the whole.
//
// A table with no OUTPUT rate is not a hypothetical: tool-prune ships one. Cost refuses the
// whole request there — a tier carried tokens and had no rate, so any total would be quietly
// low — and the prompt half is then the only figure that can be attributed to anything. A
// guard keyed on "the whole was refused" would delete it and take the one honest number in the
// record with it.
func TestSettle_OnePricedHalfSurvivesAlone(t *testing.T) {
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = 1e-6, true
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	pctx := ctx(map[string]string{"Content-Type": "application/json"}, 1000, 500)

	got := Settle(pctx, pricing.NewRegistry(tab))

	if got.HasModelled {
		t.Fatal("HasModelled = true with no output rate over 500 output tokens; the premise of this control is that Cost refuses that whole")
	}
	if !got.HasPrompt || got.PromptUSD != 1000*1e-6 {
		t.Errorf("PromptUSD = %v (HasPrompt %v), want %v / true: the prompt half is the only attributable figure here and must survive",
			got.PromptUSD, got.HasPrompt, 1000*1e-6)
	}
	if got.HasOutput {
		t.Errorf("HasOutput = true at %v with no output rate", got.OutputUSD)
	}
}

// TestSettle_SplitUnreportedReachesTheRecord closes the gap review round 6 named in item 11:
// this reason was pinned only where pricing RETURNS it, never once at the level that publishes
// it.
//
// The two levels are different claims, and the string is load-bearing at both. Settle decides
// whether to attach a reason at all — it gates the call on the usage-fallback arm, so a gateway
// header would suppress it — and NewRecord decides whether the reason survives onto the wire,
// which is where a consumer reads it. Dropping it at either point leaves a figure that is
// approximate in no known direction being rendered as exact: a total-only gateway's number is
// attributed wholly to uncached input, so it over-prices a cache-heavy turn and under-prices a
// generation-heavy one, and nothing about it looks unusual.
//
// TotalTokens alone with no per-kind split is the shape that produces it, which is a standing
// property of a gateway rather than an incident — see pricing.ReasonSplitUnreported.
func TestSettle_SplitUnreportedReachesTheRecord(t *testing.T) {
	pctx := ctx(map[string]string{"Content-Type": "application/json"}, 0, 0)
	pctx.Extensions.Inference.TotalTokens = 1700

	got := Settle(pctx, rates(t))

	if !got.Priced {
		t.Fatalf("Priced = false for a total-only gateway (%+v); the approximation is a caveat on a figure, so there has to be a figure", got)
	}
	if !got.Incomplete || got.IncompleteReason != pricing.ReasonSplitUnreported {
		t.Errorf("Incomplete = %v / reason = %q, want true / %q: without it an approximate total is published as exact",
			got.Incomplete, got.IncompleteReason, pricing.ReasonSplitUnreported)
	}
	record := NewRecord(got, nil)
	if !record.Incomplete || record.IncompleteReason != pricing.ReasonSplitUnreported {
		t.Errorf("record Incomplete = %v / reason = %q, want true / %q: a caveat that reaches no record reaches no consumer",
			record.Incomplete, record.IncompleteReason, pricing.ReasonSplitUnreported)
	}
}
