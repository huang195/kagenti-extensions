package pricing

import "math"

// MaxCostMicros bounds a single request's cost.
//
// float64 counts every integer exactly only up to 2^53, so a total beyond that cannot
// round-trip through int64 meaningfully even when it fits. $9 billion for one request
// is unreachable by any legitimate traffic and is the point past which a figure is a
// bug rather than a bill.
//
// EXPORTED so the one bound serves every producer of a micros figure. costevent's
// header path accepts any finite non-negative float, and its Micros() conversion was
// unguarded — so a header of 1e13 saturated to MaxInt64 and two such requests wrapped
// usage.Counts.Add to a NEGATIVE total. A second bound declared over there would be
// free to drift from this one; there is only ever one answer to "past which figure is
// this a bug".
const MaxCostMicros = 1 << 53

// MicrosFromUSD converts a dollar figure to integer micros — millionths of a dollar —
// reporting false when the result is not a usable ledger figure.
//
// The one conversion, because there is one bound. Both callers previously wrote
// `int64(math.Round(usd * 1e6))` by hand and only this package's checked the range;
// see MaxCostMicros for what the unchecked one produced.
//
// ok is false for NaN, an infinity, a negative figure, or anything above
// MaxCostMicros. A caller must treat that as UNPRICED and not as a large number: a
// clamped figure is a wrong number wearing a right label, and the ring forgets it in
// six hours while the durable ledger keeps it for thirty days with no repair path.
//
// The SIGN IS CHECKED ON THE INPUT, before rounding, because it does not survive the
// rounding. math.Round(-1e-07 * 1e6) is math.Round(-0.1), which is NEGATIVE ZERO, and
// `-0.0 < 0` is false in Go — so this returned (0, true) for every figure in
// (-5e-07, 0), calling a wrong-signed figure a priced zero and contradicting the
// paragraph above. Anything at or beyond -5e-07 rounded to -1 and was caught, which
// is why the hole was only ever the tiny end.
//
// An input of exactly -0.0 IS ZERO and stays priced: `-0.0 < 0` is false here too, and
// that is correct rather than incidental — negative zero is a value of zero, a settled
// zero is a producer saying the call was free, and refusing it would report a genuine
// free call as unpriced traffic.
//
// NaN does not reach this guard (every comparison against NaN is false) and is still
// caught below, where it always was.
func MicrosFromUSD(usd float64) (int64, bool) {
	if usd < 0 {
		return 0, false
	}
	micros := math.Round(usd * 1e6)
	// No `micros < 0` check: a non-negative input cannot round to a negative figure,
	// so the input guard above subsumes it. Keeping both would leave the impression
	// that the rounded sign is load-bearing, which is the belief that produced the bug.
	if math.IsNaN(micros) || math.IsInf(micros, 0) || micros > MaxCostMicros {
		return 0, false
	}
	return int64(micros), true
}

// Cost prices u at r, returning integer micros — millionths of a dollar.
//
// Micros because usage.Counts.CostMicros is already that unit, and integer
// addition across ring buckets is exact where repeated float addition is not.
//
// ok is false when the request is UNPRICED, which is a different answer from a
// cost of zero. Four ways to be unpriced:
//
//   - A tier that carried tokens has no rate. The invariant: a request is priced
//     only if every tier it used had a rate, so a partial table produces a named
//     gap rather than a total that is quietly too low. This is toolprune's
//     rateFor rule (plugin.go:155-174) promoted — see the plan for the failure it
//     was written against.
//   - No tokens were reported at all. That is unknown usage, not a free request;
//     counting it as priced-zero would dilute the coverage denominator.
//   - A negative count, which is a parser bug or a hostile body. A negative cost
//     would corrode a running total that nothing re-derives.
//   - No rates at all, which is the ProvNone case reaching here directly.
//   - A rate that is negative or non-finite, wherever it came from. Trusting the
//     rate while checking the count would let a hostile or buggy producer emit
//     negative money or MaxInt64 micros.
//
// A tier with no rate but no tokens is fine: toolprune's table has no output rate
// at all, and refusing there would unprice every request it measures.
func Cost(r Rates, u Usage) (int64, bool) {
	if u == (Usage{}) {
		return 0, false
	}
	eff := r.At(u.PromptTotal())
	var usd float64
	for i, n := range u.tokens() {
		if n < 0 {
			return 0, false
		}
		if n == 0 {
			continue
		}
		if !eff.Set[i] {
			return 0, false
		}
		// The RATE is validated here, not only at config time. Cost previously
		// checked the token count and trusted the rate, so a negative rate yielded
		// ok=true with negative micros, +Inf yielded MaxInt64, and NaN was
		// architecture-dependent. Only config.Build validated rates, which leaves
		// every other producer unguarded — including the ProvDiscovered /model/info
		// path, where the numbers come from a remote gateway.
		if r := eff.Base[i]; r < 0 || math.IsNaN(r) || math.IsInf(r, 0) {
			return 0, false
		}
		usd += float64(n) * eff.Base[i]
	}
	// Bound-checked before conversion, in MicrosFromUSD. Each RATE is validated above,
	// but a finite rate times a large token count still accumulates past int64: the
	// conversion would then be undefined and return ok=true with a garbage ledger
	// figure, which is worse than reporting the request unpriced.
	return MicrosFromUSD(usd)
}
