// Package costing turns one response's facts into one settled cost.
//
// It exists because that decision was made in two places with two shapes — inside
// litellm-budget-track and again inside the usage aggregator — and the two could disagree
// about the same request. Whichever component owns costing now calls Settle, and there is
// one precedence rule: an authoritative figure the gateway reported beats a figure modelled
// from token counts and a rate table.
//
// Separate from the parser that produces the token counts, and separate from the plugins
// that act on the money, because it is neither: a gateway's cost header is
// vendor-specific knowledge that has no place in a provider-shaped body parser, and a
// ledger has no business deciding what a request cost.
package costing

import (
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// Response cost headers emitted by LiteLLM.
//
// Exported because they are wire names, not internals: a test needs to synthesize them, and
// a future adapter for a gateway that reports cost differently needs to say which of these
// it is standing in for.
//
// MEASURED 2026-09-11 against ete-litellm (LiteLLM 1.85.5), because the semantics are easy
// to state backwards — and stated backwards, they read as though drift detection would
// false-positive on every Anthropic-format request:
//
//	/v1/chat/completions  both headers present, IDENTICAL values
//	/v1/messages          only "-original", same value the other path reports
//
// For 16 input + 4 output tokens of claude-opus-5 both paths reported
// 0.00013680000000000002, which is 0.76 x vendor list (16x$5 + 4x$25 per Mtok = 0.00018).
// So "-original" is the cost the gateway ACTUALLY CHARGED, not a pre-discount list price,
// and falling back to it compares like with like.
//
// "Original" refers to LiteLLM's OWN discount/margin layer, reported alongside in
// X-Litellm-Response-Cost-{Discount,Margin}-Amount: original is the figure before that
// layer is applied. This gateway runs neither (both report 0.0), so the two agree. A
// gateway that DOES configure them would see them diverge, which is why checkDrift
// checks those headers before comparing — see driftComparable.
//
// The fallback itself is load-bearing: without it, budget tracking silently records $0
// for every Anthropic-format request, which is the shape Claude Code sends.
const (
	ResponseCostHeader         = "X-Litellm-Response-Cost"
	ResponseCostOriginalHeader = "X-Litellm-Response-Cost-Original"

	// LiteLLM's own adjustment layer, non-zero only where an operator configured it.
	DiscountAmountHeader = "X-Litellm-Response-Cost-Discount-Amount"
	MarginAmountHeader   = "X-Litellm-Response-Cost-Margin-Amount"
)

// headerCostState says what the gateway's cost header actually told us. A bool
// could not carry this, and collapsing these cases caused a real defect: every
// state below except headerPositive returned (0, true), so "the gateway declared
// this call free" was indistinguishable from "the header was garbage" and from
// "this is a stream, where LiteLLM always stamps 0 as a placeholder". Publishing a
// settled zero for all of them counted unpriced traffic as priced.
type headerCostState int

const (
	// headerAbsent: no cost header at all. Price from token usage.
	headerAbsent headerCostState = iota
	// headerUnusable: a header was present but could not be believed — unparseable,
	// negative, NaN, Inf, or past the micros unit every consumer accumulates in. NOT a
	// declaration of anything, so it must not suppress the usage fallback and must never
	// publish a settled zero.
	headerUnusable
	// headerZero: present, parsed, finite and exactly zero. On a NON-streamed
	// response this is the gateway saying the call was free — a cache hit, or an
	// error it declined to charge for. On a streamed response it means nothing:
	// LiteLLM stamps 0 there by design because the total is unknown when headers
	// are sent.
	headerZero
	// headerPositive: a usable figure.
	headerPositive
	// headerImplausible: present, parsed, finite, positive — and larger than one
	// inference call could plausibly cost, on an endpoint this pipeline could not
	// parse. Refused rather than believed, and refused rather than clamped: see
	// implausibleUnparsedCost. Distinct from headerUnusable because it is DISCLOSED —
	// a figure was on the wire and this process declined it, which is a fact an
	// operator needs and a garbage header is not.
	headerImplausible
)

// headerCost returns the cost the gateway reported and what kind of answer it was.
func headerCost(pctx *pipeline.Context) (cost float64, state headerCostState) {
	costStr := pctx.ResponseHeaders.Get(ResponseCostHeader)
	if costStr == "" {
		// Anthropic /v1/messages (and newer LiteLLM) omit the bare header, so the fallback is
		// what keeps Claude Code's shape from recording $0 for every request.
		//
		// PRE-DISCOUNT ON A GATEWAY THAT RUNS THAT LAYER, which the package doc measures and
		// this line does not qualify. "Original" means the figure BEFORE LiteLLM's own
		// discount/margin layer: on the gateway measured here both are zero so the two headers
		// agree, but where an operator configures them, /v1/messages — the Anthropic shape, with
		// no bare header at all — bills the pre-discount figure while /v1/chat/completions bills
		// the post-discount one. The two paths then disagree about the same model, and the drift
		// check cannot see it because checkDrift compares this figure against the MODELLED one,
		// not against the discount headers it reads. Pre-existing and not introduced here;
		// closing it means subtracting DiscountAmountHeader and MarginAmountHeader when they are
		// non-zero, which needs a gateway configured that way to verify against.
		costStr = pctx.ResponseHeaders.Get(ResponseCostOriginalHeader)
	}
	if costStr == "" {
		return 0, headerAbsent
	}
	c, err := strconv.ParseFloat(costStr, 64)
	if err != nil || math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return 0, headerUnusable
	}
	if c == 0 {
		return 0, headerZero
	}
	if implausibleUnparsedCost(pctx, c) {
		return 0, headerImplausible
	}
	// UNREPRESENTABLE IS THE LAST QUESTION ASKED, and the order is the whole substance of this
	// check. pricing.MicrosFromUSD is the predicate costevent.Event.Priced() applies to this
	// same figure downstream, so calling it here is what makes the producer and the consumer
	// agree BY CONSTRUCTION: written as two independent checks they disagreed over ($9.007
	// billion, +Inf), where this arm set Priced while the record it produced read as unpriced —
	// a budget accumulating a charge the ledger and the aggregate both filed as uncovered, and
	// two headers near the float ceiling summing to a +Inf the ledger's json.Marshal cannot
	// write at all.
	//
	// ASKED AFTER THE PLAUSIBILITY BRANCH, because every figure past the micros unit is also
	// past the plausibility cap, so asking first would swallow the hostile-host case as noise
	// and make the disclosure unreachable for exactly the values most likely to be forged.
	//
	// AND UNUSABLE RATHER THAN IMPLAUSIBLE, which is forced rather than chosen. What reaches
	// here is a PARSED endpoint — the plausibility branch above already claimed the unparsed
	// ones — and a parsed endpoint has usage to model, so Settle prices this response from the
	// rate table. RejectedReason reads as unpriced to every consumer (see Event.Priced), so
	// disclosing the refusal here would refuse the modelled figure along with the header and
	// leave a record whose two halves contradict each other. Unusable keeps the header out and
	// lets the modelled charge stand, which is the outcome that loses no money.
	if _, ok := pricing.MicrosFromUSD(c); !ok {
		return 0, headerUnusable
	}
	return c, headerPositive
}

// implausibleUnparsedCost is the CAP on what an endpoint nobody could parse is allowed to
// charge, and the one thing standing between a hostile host and a poisoned ledger.
//
// THE HOLE IT CLOSES. This header is an unauthenticated string on a response, validated
// here for nothing but its numeric shape, and nothing in the pipeline considers the HOST it
// came from — inference-parser dispatches on the path alone. Cost is now settled on every
// proxied response including the ones with no inference extension, so before this cap ANY
// path on ANY host an agent was proxied to could name its own figure and have it published,
// aggregated, written to the thirty-day ledger, and fed to litellm_budgettrack, which
// denies with HTTP 429 once the daily total passes MaxBudget. One response from one hostile
// site could therefore lock an agent out of all further inference and corrupt durable cost
// reporting. inference-parser is in the default local pipeline, so this is the shipped
// configuration.
//
// GATED ON A NIL EXTENSION, which is exactly the set of responses whose spend was newly
// admitted — an endpoint off the parser's dialect list, or a body it could not read.
//
// WHY THE PARSED PATH IS LEFT ALONE, which is not merely a scope decision. Where the extension
// is present there is a MODELLED figure from the token counters to compare the header against,
// so an implausible header is DETECTABLE there: Settle publishes both and the drift check
// notices them disagreeing (see Settled.HasReported and ModelledUSD). Refusing it instead would
// throw away the one signal that a rate table has gone stale against a gateway — the case the
// drift measurement exists for — to bound a figure that is already visible as wrong.
// TestSettle_ParsedEndpointIsUnaffectedByTheCap pins that.
//
// TWO LIMITS ON THAT ARGUMENT, both of which narrow it rather than restate it:
//
//   - DETECTABLE IS NOT PREVENTED. The drift check logs; it does not withhold. The figure still
//     reaches Publish, the aggregate, the thirty-day ledger and litellm_budgettrack's 429
//     lockout, so on the parsed path the disclosure buys an operator a log line after the
//     damage, not instead of it.
//   - A NON-NIL EXTENSION IS NOT A CORROBORATING FIGURE. The gate reads "the request parsed",
//     and the extension is populated on the REQUEST pass carrying no counts, so it can be
//     present with nothing to compare against: HasModelled is false whenever the resolver has
//     no rates for the model or the usage block is empty. Those responses take this branch and
//     get neither the cap nor the drift signal.
//
// Both are why cortex#1027 is an allowlist of hosts whose header is believed at all, rather
// than a wider cap.
//
// THE RESIDUAL, stated because it is reachable and not closed here: a hostile host serving a
// path the parser DOES recognise can settle a figure up to MaxCostMicros ($9.007 billion),
// which is published, aggregated and written to the ledger while the drift check merely notes
// the disagreement. Bounding it is not the fix — the fix is not believing that host's header at
// all, which is the allowlist tracked in rossoctl/cortex#1027. It also feeds the accumulation
// wrap pricing.MaxCostMicros documents, which the checked accumulate in this series' aggregate
// PR closes.
//
// WHAT IT IS NOT. It is a blast-radius cap, NOT AUTHENTICATION. A forged figure UNDER the
// cap still settles, because a plausible number from an unparsed endpoint is exactly what a
// gateway-priced /v1/embeddings response looks like and refusing it would break the feature
// that opened the hole. The stronger fix is an allowlist of hosts whose cost headers are
// believed at all; it was considered and deprioritised, and this cap was preferred because
// it also bounds a second disclosed gap (see pricing.MaxCostMicros on the aggregate wrap,
// which it moves and does not close).
//
// REFUSED, NOT CLAMPED. Clamping to the cap would invent a $10,000 charge nobody made and
// publish it wearing the same label a real figure wears. The refusal is published instead,
// as costevent.RejectedImplausible on an unpriced record, so the coverage gap stays
// nameable — that is the whole doctrine here: a wrong number wearing a right label is the
// worst available outcome.
func implausibleUnparsedCost(pctx *pipeline.Context, usd float64) bool {
	if pctx.Extensions.Inference != nil {
		return false
	}
	if pricing.PlausibleRequestCostUSD(usd) {
		return false
	}
	warnImplausibleCost(pctx, usd)
	return true
}

// implausibleWarnOnce keeps the operator-facing warning to ONE per process.
//
// Not once per host, which is the shape a reader will expect and which cannot be safely
// built here: pctx.Host is caller-controlled, so a map keyed on it is an unbounded
// allocation driven by hostile input. Not unconditional either — the warning fires on a
// path an attacker chooses and would be a log-flood amplifier.
//
// THE ARGUMENT IS ABOUT THE ALWAYS-ON LEVEL, and the split is deliberate rather than
// inconsistent: Warn fires once per process, Debug fires per occurrence. An operator turning
// debug on has asked for one line per event and needs every occurrence to be recoverable —
// TestWarnImplausibleCost_NamesTheHost pins that — whereas a Warn per hostile request would
// flood a log nobody opted into. The residual is real and worth naming: with debug enabled this
// path emits a line per attacker-chosen request. The bounded trail that needs no logger at all
// is the per-request record, costevent.RejectedImplausible, which the per-minute accumulator
// caps.
// SWAPPED BY A TEST, WHICH MAKES IT A PACKAGE-WIDE CONSTRAINT. implausible_test.go resets this
// and replaces slog.Default() to observe the one warning, so no test in this package may run in
// parallel with another while that is true. TestNoTestInThisPackageRunsInParallel enforces it,
// because a comment would not survive the first t.Parallel someone adds.
var implausibleWarnOnce sync.Once

// warnImplausibleCost tells an operator enough to FIND THE HOST: host, path, the figure
// that was refused, and the bound it exceeded. A warning saying only "implausible cost
// rejected" would leave the one question that matters unanswerable.
func warnImplausibleCost(pctx *pipeline.Context, usd float64) {
	slog.Debug("costing: refused an implausible cost header",
		"host", pctx.Host, "path", pctx.Path, "reported_usd", usd,
		"max_plausible_usd", float64(pricing.MaxPlausibleRequestCostMicros)/1e6)
	implausibleWarnOnce.Do(func() {
		slog.Warn("costing: refused a cost header larger than any inference call could plausibly be; this endpoint was not parsed, so nothing corroborates the figure and it is recorded as an unpriced coverage gap rather than as spend. Further occurrences are logged at debug level only",
			"host", pctx.Host, "path", pctx.Path, "reported_usd", usd,
			"max_plausible_usd", float64(pricing.MaxPlausibleRequestCostMicros)/1e6)
	})
}

// IsEventStream reports whether the response is a text/event-stream (SSE) — the
// streamed shape where LiteLLM reports cost 0 in the header, so usage-based
// pricing is the intended fallback.
func IsEventStream(pctx *pipeline.Context) bool {
	ct := pctx.ResponseHeaders.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "text/event-stream")
}

// Comparable reports whether the gateway's figure can be compared against a modelled
// one at all.
//
// False when the bare header is absent AND LiteLLM's own discount/margin layer is active:
// the fallback figure is then pre-adjustment while the modelled figure is what the caller
// pays, so any comparison measures the gateway's adjustment rather than the rate table.
func Comparable(pctx *pipeline.Context) bool {
	if pctx.ResponseHeaders.Get(ResponseCostHeader) != "" {
		return true // the effective figure itself; nothing to reconcile
	}
	for _, h := range []string{DiscountAmountHeader, MarginAmountHeader} {
		v := pctx.ResponseHeaders.Get(h)
		if v == "" {
			continue
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil && f != 0 {
			return false
		}
	}
	return true
}

// Settled is what one response cost, and how that was arrived at.
//
// Carries BOTH figures, not just the winner. A consumer that only needs the number reads
// CostUSD; the drift check needs the pair, and keeping them here means it compares what
// was actually decided rather than recomputing either half and hoping the two agree.
type Settled struct {
	// CostUSD is the figure to charge. Meaningful only when Priced.
	CostUSD float64
	// Source is costevent.SourceGatewayHeader or costevent.SourceUsageFallback.
	Source string
	// Provenance is ProvAuthoritative for a gateway figure, else the rate table's level.
	Provenance pricing.Provenance
	// Priced reports that a figure exists — INCLUDING a settled zero, which is the
	// gateway saying the call was free. Not the same as CostUSD > 0.
	Priced bool
	// DeclaredFree marks a gateway-reported, non-streamed, exactly-zero cost: a genuine
	// free call. Distinguished from an unusable header and from a stream's placeholder
	// zero, because publishing a settled zero for those counted unpriced traffic as
	// priced.
	DeclaredFree bool

	// Incomplete marks CostUSD as not an EXACT total, and IncompleteReason says why —
	// pricing.ReasonOutputUncounted for a figure that is known-low, or
	// pricing.ReasonSplitUnreported for one that is approximate in no known direction.
	//
	// It exists because a truncated stream was priced prompt-only and published as a
	// complete figure. Prompt counts land on Anthropic's message_start and the output
	// count only on message_delta, so a stream that dies in between yields real prompt
	// tokens with output at zero — and Settle priced exactly what it was given, set
	// Priced, and every consumer downstream read the result as an exact figure: the
	// usage aggregator counted it in PricedRequests and CostMicros, and a budget
	// enforced against it. A floor presented as a total understates spend by however
	// much the completion would have cost, which on a long generation is most of it.
	//
	// Honest by DISCLOSURE, not by adjustment. CostUSD keeps the figure and Priced stays
	// TRUE, deliberately and on both counts:
	//
	//   - The figure is the best available. Estimating the missing completion would be
	//     worse than reporting a known-low number and saying it is low.
	//   - The request IS priced, so removing it from a priced count would misuse a
	//     counter that answers a different question — coverage, "did anything price
	//     this" — and would disclose the same fact twice in two vocabularies. It would
	//     also send a consumer's own fallback down a rate table to recompute the
	//     identical prompt-only figure and label THAT one exact.
	//
	// Never set on a gateway figure, including a declared-free zero: a reported cost is
	// what the call actually charged whatever our counters saw, so completeness there is
	// the gateway's assertion rather than an inference from token tallies.
	Incomplete       bool
	IncompleteReason string

	// RejectedReason names a figure that WAS on the wire and was refused —
	// costevent.RejectedImplausible, set by implausibleUnparsedCost.
	//
	// The counterpart to Incomplete, one step further out: Incomplete qualifies a figure
	// that stands, this one records that there is no figure BECAUSE one was declined.
	// Priced is false and CostUSD is zero whenever it is set — a refused figure is not a
	// figure — and it travels onto the record so the gap is nameable instead of silent.
	// Never set alongside HasReported: refusing the figure and then keeping it for the
	// drift check would compare the rate table against a forgery.
	RejectedReason string

	// ReportedUSD is the gateway's own figure, when it gave one.
	ReportedUSD float64
	HasReported bool
	// ModelledUSD is what the rate table says the same usage costs, when it can say.
	// Computed even when a reported figure won, because the comparison is the only
	// signal that a rate table has gone stale.
	ModelledUSD float64
	HasModelled bool
	// ModelledProv is the provenance behind ModelledUSD.
	ModelledProv pricing.Provenance
	// ModelledIncomplete says the MODELLED figure is not an exact total, whichever figure
	// won, and ModelledIncompleteReason is pricing.IncompleteReason's answer for it.
	//
	// SEPARATE FROM Incomplete, which is a claim about the figure that was CHARGED. The two
	// coincide on the usage-fallback arm and diverge whenever a gateway's header wins: the
	// header is exact by assertion, so Incomplete is correctly false, and the knowledge that
	// the modelled figure beside it is a FLOOR was simply dropped. The drift check then
	// divided a known-low modelled figure by an authoritative one and reported the RATE TABLE
	// as stale — for a response whose counters were short. Carried here so the one consumer
	// that compares the pair can tell "the table is wrong" from "the counters were".
	ModelledIncomplete       bool
	ModelledIncompleteReason string

	// PromptUSD is the PROMPT half of the modelled figure — output excluded, tiers
	// weighted. It exists because a request row can show what it cost while the
	// response row shows the total, and the two must not be a sum: a cache read bills
	// at ~0.1x the uncached input rate and a write at ~1.25x, so a flat prompt figure
	// overstates a cache-heavy turn by close to an order of magnitude.
	//
	// Modelled, never authoritative: a gateway reports one total for the call and does
	// not break it down, so this is the table's answer even when the total is the
	// gateway's.
	PromptUSD float64
	HasPrompt bool

	// OutputUSD is the OUTPUT half, the same way round: generated tokens at the output
	// rate, prompt tiers excluded, resolved at the REQUEST's prompt size so a
	// long-context premium reaches the completion too. It exists so a response row can
	// show what THAT row cost rather than what the exchange cost, which is the only
	// reading under which a per-row column is row-local.
	//
	// Specifically NOT CostUSD minus PromptUSD. Those two need not share a source —
	// CostUSD may be the gateway's post-discount figure while PromptUSD is always the
	// table's — so their difference is a gateway-vs-table delta wearing a completion's
	// name, and it can go negative: a gateway charging 3.0652 against a modelled 4.0063
	// prompt differences to -0.94.
	//
	// ModelledUSD minus PromptUSD would in fact be sound — same table, same prompt size,
	// complementary tiers — so the objection is to mixing sources, not to subtraction as
	// such. Pricing directly is still preferred: it keeps HasOutput independent of
	// HasModelled, so a response row can show its own cost on a call where the total came
	// from the gateway and the table has no opinion on the whole.
	//
	// PromptUSD + OutputUSD can differ from ModelledUSD IN THE LAST PLACE, and a caller
	// comparing them needs a tolerance rather than equality. Both halves resolve at the
	// same prompt size over a complementary tier partition, so they agree on the
	// arithmetic — but each is rounded to micros on its own and the whole is rounded
	// separately, so two rounded halves need not sum to the rounded whole. costing_test.go
	// uses a 1e-6 tolerance for exactly this reason.
	//
	// So this is NOT equality "to the micro, by construction" — a tempting way to state it
	// that the rounding above contradicts.
	//
	// Neither sums to CostUSD, which may be the gateway's; comparing their sum against a
	// reported total is a drift measurement, and ModelledUSD is the figure kept for it.
	OutputUSD float64
	HasOutput bool
}

// Settle prices one response.
//
// The precedence, in one place: a positive header figure wins outright. Otherwise the
// token counts are priced through the rate table — but NOT when the gateway declared the
// call free, because inventing a cost for a call it explicitly charged nothing for is how
// a cache hit came to be billed. A stream's zero is a placeholder, not an answer, so it
// falls back like an absent header.
//
// rates may be nil: a binary with no pricing wired reports unpriced rather than crashing.
func Settle(pctx *pipeline.Context, rates pricing.Resolver) Settled {
	var out Settled
	if pctx == nil {
		return out
	}

	cost, state := headerCost(pctx)
	if state == headerPositive {
		out.ReportedUSD, out.HasReported = cost, true
	} else if state == headerZero && !IsEventStream(pctx) {
		out.ReportedUSD, out.HasReported = 0, true
	} else if state == headerImplausible {
		// DISCLOSED and not charged. Deliberately not fed to ReportedUSD: the figure was
		// refused, and keeping it as "the gateway's own figure" would let the drift check
		// measure the rate table against a number nothing corroborates and report the
		// table as stale. Nothing else about this response changes — a modelled figure
		// would still win below if one existed, which on this path it never can, since
		// the cap only applies where the extension is nil and there is therefore no usage
		// to model.
		out.RejectedReason = costevent.RejectedImplausible
	}

	// Modelled alongside, always, so the pair is available for drift even when the
	// gateway's figure wins.
	usage := pricing.UsageFromInference(pctx.Extensions.Inference)
	model := ""
	if pctx.Extensions.Inference != nil {
		model = pctx.Extensions.Inference.Model
	}
	// promptTotal is the request's own prompt size, and every figure below resolves its
	// rates at it — the whole request and both halves alike. That is what makes the two
	// halves a partition of the total instead of three unrelated lookups: a long-context
	// threshold flattens the same way for all three, so no premium can land on one and
	// miss another.
	promptTotal := usage.PromptTotal()
	micros, prov, ok, refusal := modelledCost(rates, pctx.Host, model, usage, promptTotal)
	if ok {
		out.ModelledUSD, out.HasModelled, out.ModelledProv = float64(micros)/1e6, true, prov
	}

	// AN IMPOSSIBLE COUNT REFUSES ALL THREE FIGURES, HERE, BEFORE THE HALVES ARE COMPUTED.
	//
	// The halves are not a partition of the counts — each is another pass over the same usage
	// with some tiers ZEROED — so zeroing is exactly what erases the counter that caused the
	// refusal. A negative Output, or one past what any request could report, makes Cost refuse
	// the whole request and the output half, while promptOnly drops Output to zero and comes
	// back PRICED. Measured at 3.8 micros/token: whole refused, promptOnly $0.0038.
	//
	// And a lone prompt half is enough to publish: settleCost skips only when
	// `!Priced && !HasPrompt && RejectedReason == ""`, and abctl's renderer tests PromptUSD > 0
	// rather than Priced(). So this shipped a figure derived from a count this package had just
	// declared impossible, on a record that said unpriced. Reachable from the wire, where these
	// counters are provider-controlled ints with no floor.
	//
	// SEPARATE FROM THE SUM GUARD BELOW, which keys on both halves existing because a lone
	// survivor is legitimate when a tier has no RATE (see TestSettle_OnePricedHalfSurvivesAlone).
	// An impossible count is the other case: the contract is that such a report is refused
	// whole, because a believed figure beside a refused one in the same row is a breakdown
	// nobody can reconcile.
	if refusal == pricing.RefusalImpossibleCount {
		return withRefusal(out, cost, state, pctx, costevent.RejectedImplausibleUsage)
	}

	// The prompt half, for a request row. Output zeroed rather than subtracted, because
	// Cost refuses to price a tier that carried tokens with no rate — and a partial total
	// presented as a whole one is worse than none.
	promptOnly := usage
	promptOnly.Output = 0
	if micros, _, ok, _ := modelledCost(rates, pctx.Host, model, promptOnly, promptTotal); ok {
		out.PromptUSD, out.HasPrompt = float64(micros)/1e6, true
	}

	// The output half, for a response row, by the same construction: zero the prompt
	// tiers instead of subtracting the prompt figure from the total.
	outputOnly := usage
	outputOnly.Input, outputOnly.CacheWrite, outputOnly.CacheRead = 0, 0, 0
	if micros, _, ok, _ := modelledCost(rates, pctx.Host, model, outputOnly, promptTotal); ok {
		out.OutputUSD, out.HasOutput = float64(micros)/1e6, true
	}

	// THE PAIR IS HELD TO THE PLAUSIBILITY CEILING ITS OWN HALVES ESCAPE. pricing.Cost applies
	// that ceiling per call and this function makes three of them, so a request whose whole is
	// refused as impossible can have both halves come back priced just under it: the record then
	// reads unpriced, cost $0, prompt $10,000, output $10,000, and anything adding the request
	// row to the response row — which is what these two fields are for — charges the total that
	// was refused. Halving a forged figure does not produce two real ones.
	//
	// KEYED ON THE SUM, NOT ON "WAS THE WHOLE PRICED", because a single surviving half is
	// deliberate: where the output tier carried tokens with no rate, Cost refuses the whole and
	// the prompt half is the only figure anyone can attribute. Only the pair can smuggle a figure
	// past a bound neither half broke.
	if out.HasPrompt && out.HasOutput && !pricing.PlausibleRequestCostUSD(out.PromptUSD+out.OutputUSD) {
		out.PromptUSD, out.HasPrompt = 0, false
		out.OutputUSD, out.HasOutput = 0, false
	}

	streamedPlaceholder := state == headerZero && IsEventStream(pctx)
	out.DeclaredFree = state == headerZero && !streamedPlaceholder

	switch {
	case state == headerPositive:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			cost, costevent.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case out.DeclaredFree:
		// A real answer of zero. Nothing is charged, but it must be published as
		// PRICED so no consumer re-prices it from the usage block.
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			0, costevent.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case out.HasModelled:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			out.ModelledUSD, costevent.SourceUsageFallback, out.ModelledProv, true
	}

	// Qualify a MODELLED figure whose counters cannot support an exact total. Nothing is
	// adjusted — see Settled.Incomplete; the figure stands and the claim about it does
	// not.
	//
	// This is the only place in the system that holds both the header state and the
	// usage, so it is the only place that knows WHICH figure won — and therefore the
	// only place that can gate the disclosure on the answer being modelled. Gated on the
	// source rather than on "the header was not positive" because DeclaredFree reaches
	// here as SourceGatewayHeader too: the gateway stating it charged nothing is an
	// exact total, and publishing a floor of zero would be a lower bound on nothing.
	if out.HasModelled {
		if reason := pricing.IncompleteReason(pctx.Extensions.Inference); reason != "" {
			out.ModelledIncomplete, out.ModelledIncompleteReason = true, reason
			// The charged figure is qualified only when the modelled one IS the charged one.
			// Gated on the source rather than on "the header was not positive" because
			// DeclaredFree reaches here as SourceGatewayHeader too: a gateway stating it
			// charged nothing is an exact total, and publishing a floor of zero would be a
			// lower bound on nothing.
			if out.Priced && out.Source == costevent.SourceUsageFallback {
				out.Incomplete, out.IncompleteReason = true, reason
			}
		}
	}

	// A MODELLED FIGURE PAST THE CEILING IS DISCLOSED TOO, on the same rule as a header's.
	// Both say no one inference call could have cost this; the difference is only which
	// number was wrong, and this one's cause is ours — an operator's typo in a rate, or a
	// rate discovered from a gateway — so it unprices every request that rate touches while
	// looking exactly like traffic nobody had rates for.
	//
	// GATED ON NOTHING ELSE HAVING SETTLED, which is not hygiene. RejectedReason reads as
	// unpriced to every consumer, so disclosing a derived figure's refusal on a response the
	// GATEWAY priced would discard an authoritative charge in order to complain about a
	// number that lost anyway. See TestSettle_AHeaderFigureSurvivesAnImpossibleTokenReport.
	if !out.Priced && out.RejectedReason == "" && refusal == pricing.RefusalImplausibleTotal {
		out.RejectedReason = costevent.RejectedImplausible
	}
	return out
}

// withRefusal finishes a Settled whose MODELLED figures were all refused, keeping whatever the
// gateway's header already established.
//
// The header arm still runs, and that ordering is the point: a bad token report says nothing
// about what the call actually charged, so a gateway figure — or a declared-free zero — must
// survive it. The disclosure is attached only when nothing else settled, because a set
// RejectedReason reads as unpriced everywhere.
func withRefusal(out Settled, cost float64, state headerCostState, pctx *pipeline.Context, reason string) Settled {
	switch {
	case state == headerPositive:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			cost, costevent.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case state == headerZero && !IsEventStream(pctx):
		out.DeclaredFree = true
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			0, costevent.SourceGatewayHeader, pricing.ProvAuthoritative, true
	}
	if !out.Priced && out.RejectedReason == "" {
		out.RejectedReason = reason
	}
	return out
}

// modelledCost prices usage through the rate table.
//
// promptTotal is THE REQUEST'S prompt size, passed separately from u rather than derived
// from it. A context threshold is a property of the request — Rates.At puts it that way:
// the premium is priced on how much prompt was sent, not on what came back — and u is
// often not the whole request. It may be one half of it (prompt or output), or a
// counterfactual (the tokens a plugin avoided). Deriving the threshold from u.PromptTotal()
// looked right and silently dropped the premium for every such slice: an output half has no
// prompt tokens at all, so it resolved at At(0) and priced a 641k-token turn's completion at
// the base output rate while the whole-request figure used the above-200k one.
//
// The nil guard is on the INTERFACE, which is the trap: an un-injected consumer holds a
// nil interface and calling a method on it panics, where a nil *pricing.Registry would
// have been safe. tool-prune hit exactly this and its fail-open masked the panic, so
// pruning silently stopped.
func modelledCost(rates pricing.Resolver, host, model string, u pricing.Usage, promptTotal int) (int64, pricing.Provenance, bool, pricing.Refusal) {
	if rates == nil || u == (pricing.Usage{}) {
		// No resolver is a deployment with no rate table at all, and no counters is unknown
		// usage. Neither is a refusal of anything: nothing was on the wire to refuse.
		return 0, pricing.ProvNone, false, pricing.RefusalNoTokens
	}
	r, prov := rates.Resolve(host, model, promptTotal)
	if prov == pricing.ProvNone {
		return 0, pricing.ProvNone, false, pricing.RefusalNoRate
	}
	micros, ok, refusal := pricing.CostWithReason(r, u)
	if !ok {
		return 0, pricing.ProvNone, false, refusal
	}
	return micros, prov, true, pricing.RefusalNone
}

// StateKey is where the full Settled outcome is stashed for the rest of the request.
//
// In-process only — deliberately not on the wire. A consumer that needs the number reads
// the published costevent record; the drift check needs BOTH figures, and putting them in
// the session event would widen a public shape for one diagnostic's benefit. Keeping them
// here also means drift compares what was actually decided instead of recomputing a half
// and hoping the two agree.
const StateKey = "costing.settled"

// Store stashes the outcome for later plugins in the same request.
func Store(pctx *pipeline.Context, s Settled) {
	pipeline.SetState(pctx, StateKey, &s)
}

// Load retrieves the outcome. False means the cost owner's RESPONSE PASS never ran for this
// request — the parser is absent from the pipeline, the request was rejected before the
// response phase, or no listener delivered a terminal frame.
//
// It does NOT mean the request carried no inference, and no longer says anything about the
// traffic's shape. Store now runs on EVERY proxied response, including the ones this parser
// has no dialect for, because a gateway reports its own cost in a response header that needs
// neither a model nor a body — so /v1/embeddings, /mcp, a health check and a CONNECT tunnel
// all Load TRUE. True therefore says only that a decision was reached; whether the decision
// was a figure is Settled.Priced, which is false for most of that traffic.
//
// The old reading — false means no inference at all — held only while Store was reached from
// the parsed paths alone, and it is the reading under which a gateway-priced response the
// parser could not read escaped the ledger entirely.
func Load(pctx *pipeline.Context) (Settled, bool) {
	if s := pipeline.GetState[Settled](pctx, StateKey); s != nil {
		return *s, true
	}
	return Settled{}, false
}

// Publish writes the wire record onto the session event.
//
// Under costevent.Key, and ALSO under the legacy plugin-name key: abctl is a separate
// binary that can lag the proxy, and an older one reads only the legacy key, so writing
// just the new one would blank the cost column for anyone who has not upgraded both. The
// legacy write comes out a release later.
//
// Ledger fields are left zero. They belong to whoever enforces a budget, which is not this
// package's business; litellm-budget-track amends them in place when it is present. That
// amendment is the one sanctioned second write to this key.
func Publish(pctx *pipeline.Context, ev costevent.Event) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix] = ev
	pctx.Extensions.Custom[costevent.PluginName+pipeline.PluginEventSuffix] = ev
}

// Amend rewrites the published record, for a consumer adding fields it owns.
//
// Returns false when nothing has been published, so a caller cannot resurrect a record for
// a request that was never priced.
func Amend(pctx *pipeline.Context, f func(*costevent.Event)) bool {
	if pctx.Extensions.Custom == nil {
		return false
	}
	ev, ok := pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix].(costevent.Event)
	if !ok {
		return false
	}
	f(&ev)
	Publish(pctx, ev)
	return true
}

// NewRecord builds the wire record from a settled outcome.
//
// Named NewRecord, not Record, because costevent.Record READS a record off a session event
// and these two packages are imported together — the cost owner imports costing, every
// consumer imports costevent. Two functions with one name pointing opposite directions is a
// coin flip at each call site, and the New prefix says which way this one goes.
func NewRecord(s Settled, avoided []costevent.Saving) costevent.Event {
	return costevent.Event{
		CostUSD:    s.CostUSD,
		Source:     s.Source,
		Provenance: s.Provenance.String(),
		Settled:    s.Priced,
		// Carried, not derived. A Settled.Incomplete that NewRecord dropped would be
		// knowledge that reaches nothing — which is exactly the state this fix found the
		// parser's "token counts will be incomplete" log line in.
		Incomplete:       s.Incomplete,
		IncompleteReason: s.IncompleteReason,
		// Carried for the same reason: a refusal that reached no record is a coverage gap
		// nobody can see, which is the state this fix found the implausible-header path
		// in — it published nothing at all and looked identical to a response that
		// reported no cost.
		RejectedReason: s.RejectedReason,
		PromptUSD:      s.PromptUSD,
		OutputUSD:      s.OutputUSD,
		Avoided:        avoided,
	}
}
