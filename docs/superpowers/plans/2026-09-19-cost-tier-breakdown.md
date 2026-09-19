# Per-Tier Cost Breakdown Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show an abctl user what each token tier — input, cache-write, cache-read, output — actually cost, on the screen they already press `$` for.

**Architecture:** `pricing.CostWithReason` already computes each tier's cost and discards it one line later; capture it there, publish it on the per-event cost record, aggregate it as four `int64` on `usage.Counts` (which `costledger.Row` embeds, so the ledger and `/v1/usage` follow for free), and render it as a two-column panel. The tiers are always *modelled* while the total may be a gateway's authoritative figure, so the display apportions the authoritative total by the modelled mix rather than printing two totals.

**Tech Stack:** Go 1.26, `charmbracelet/bubbletea` + `bubbles/table` + `lipgloss` for the TUI. Two modules under `authbridge/go.work`: `authbridge/authlib` and `authbridge/cmd/abctl`.

**Spec:** `docs/proposals/cost-tier-breakdown.md` (committed `af35da42`). Read it first — this plan argues from it and does not restate its reasoning.

## Global Constraints

- **DCO is mandatory.** Every commit uses `git commit -s`. Append `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. **Never** `Co-Authored-By` — a `commit-msg` hook rejects it.
- **Never render `$0.00` for a cost that might be unknown.** `emptyCell` (`—`) means "not known here"; a known-nonzero charge shown as zero is the worse lie. Both directions are already enforced by `sessionMoneyCell`.
- **Marker vocabulary, already documented in the `?` overlay:** `~` inexact/estimate, `+` partial/floor, `!` damaged.
- **`AvoidedMicros` may never be added to spend, in either direction.** It is not part of this feature; do not split it by tier.
- **`reasoning` is a subset of `output`, not a fifth tier.** It stays in `abctl cost`'s token line and out of every cost figure.
- **Drop whole figures right-to-left; never clip one.** A truncated number is a wrong number.
- **Do not run `make lint`** — it fails on pre-existing issues and rewrites unrelated files. Use `go vet ./...` plus `golangci-lint run --new-from-rev upstream/main ./...` from the module root.
- **Do not run `gofmt -w .` at a module root.** Scope it to the directories you changed.
- **Commit before mutation-testing.** `git checkout <path>` restores from HEAD and will silently revert uncommitted work, making later controls fail on compile errors that look like passes.
- **Every behavioural claim needs a discriminating test AND a mutation control** verified on three points: the mutation *applied* (grep for a token of the mutated text), it *builds* (a control that fails to compile looks identical to one that passes), and the *intended* assertion failed (grep `^--- FAIL` plus the message, not the exit code).
- **Test commands** run from the module root: `cd authbridge/authlib && go test ./...` and `cd authbridge/cmd/abctl && go test ./...`. If the workspace interferes, prefix `GOWORK=off`.

---

## File Structure

**PR 1 — capture** (`authbridge/authlib`)

| File | Responsibility |
|---|---|
| `pricing/cost.go` | Gains `CostByTier` and `NumTiers`; `CostWithReason` becomes a two-line delegation to it |
| `pricing/cost_tier_test.go` (new) | The per-tier arithmetic, and that delegation preserved every refusal |
| `costing/costing.go` | `modelledCost` returns the tier array; `Settled` gains `TierUSD`/`HasTiers`; the magnitude-refusal clause zeroes them |
| `costevent/costevent.go` | New `TierCost` struct and `Event.Tiers *TierCost` |

**PR 2 — aggregate** (`authbridge/authlib`, `authbridge/cmd/abctl`)

| File | Responsibility |
|---|---|
| `usage/usage.go` | Four `int64` on `Counts` wired into `Add`; `eventCost.tierMicros`; both cost sources populate it; `foldInto` carries it |
| `usage/apportion.go` (new) | `Counts.ApportionTiers` — the one place the mix is applied to the authoritative total |
| `usage/apportion_test.go` (new) | Exact summation including the remainder rule, and the no-mix refusal |
| `cmd/abctl/cmd_cost.go` | `--json` gains the tiers |

**PR 3 — render** (`authbridge/cmd/abctl/tui`)

| File | Responsibility |
|---|---|
| `spend_tiers.go` (new) | `renderTierRows` — pure function, tier label + bar + apportioned figure |
| `spend_band.go` (new) | `renderSpendBand` — the two-line label-over-value KPI band |
| `spend_drawer.go` | Composes band, tier column and model column; owns the line budget |
| `app.go`, `keys.go` | The reservation and its padding, moved together |
| `sessions_pane.go` | Right-aligned numerics, truncated session ID, scope label |

---

## Fixtures the tasks assume

Four test helpers are named in the task bodies. **None of them exists yet** — you write each in the test file that uses it, matching the signature below, by copying the construction from the nearest existing test in that package and changing only the usage split. They are listed here so that no task refers to something undefined.

```go
// Task 2, authlib/costing/costing_tier_test.go
// Runs Settle over one inference request with the given token split, against a rate
// table that prices every tier, and returns the PUBLISHED record.
// Copy the pipeline.Context construction from the nearest TestSettle_* case.
func settleFixtureWithUsage(t *testing.T, input, cacheWrite, cacheRead, output int) costevent.Event

// Task 2, same file. Identical, except the resolver returns pricing.ProvNone for
// every lookup, so nothing can be modelled.
func settleFixtureNoRates(t *testing.T) costevent.Event

// Task 4, authlib/usage/usage_tier_test.go
// Feeds one inference event carrying NO cost record into a fresh Aggregator that HAS a
// rates resolver — the fallback path — and returns the snapshot.
// Copy from the nearest test asserting on SourceUsageFallback.
func fallbackFixtureWithUsage(t *testing.T, input, cacheWrite, cacheRead, output int) *usage.Snapshot

// Task 4, same file. Identical, resolver returns ProvNone.
func fallbackFixtureNoRates(t *testing.T) *usage.Snapshot

// Task 6, cmd/abctl/cmd_cost_test.go
// Runs writeCostJSON over the snapshot and returns stdout as a string.
// Copy from the nearest existing writeCostJSON test.
func jsonForSnapshot(t *testing.T, snap *usage.Snapshot) string
```

**Each fixture must fail loudly if its own premise breaks.** `settleFixtureWithUsage` that silently returns an unpriced record would make Task 2's assertions vacuous — `t.Fatal` inside the helper when the record is not `Settled`, so a broken fixture reads as a broken fixture rather than as a passing test.

---

# PR 1 — Capture

Branch: `feat/cost-tier-capture` off `upstream/main`. No surface changes; ships the per-event data and is testable as pure arithmetic.

### Task 1: `pricing.CostByTier`

**Files:**
- Modify: `authbridge/authlib/pricing/cost.go:194-200` (`Cost`), `:283-337` (`CostWithReason`)
- Test: `authbridge/authlib/pricing/cost_tier_test.go` (new)

**Interfaces:**
- Consumes: existing `Rates{Base [numTiers]float64, Set [numTiers]bool, Thresholds []ContextThreshold}`, `Usage{Input, CacheWrite, CacheRead, Output int}`, `Rates.At(promptTotal int) Rates`, `MicrosFromUSD`, `PlausibleUsage`, `MaxPlausibleRequestCostMicros`, and the `Refusal` constants.
- Produces: `const NumTiers = 4` and
  `func CostByTier(r Rates, u Usage) (tiers [NumTiers]int64, total int64, ok bool, refusal Refusal)`.
  Tier indices are the existing `TierInput=0, TierCacheWrite=1, TierCacheRead=2, TierOutput=3`.

- [ ] **Step 1: Write the failing test**

Create `authbridge/authlib/pricing/cost_tier_test.go`:

```go
package pricing

import "testing"

// The per-tier split is the arithmetic Cost already does, kept instead of summed away.
//
// The fixture is deliberately cache-heavy so TOKENS and MONEY rank differently:
// cache-read is 94% of the tokens and 35% of the cost, output is 3% of the tokens and
// 52% of the cost. A test whose fixture ranked both the same way would pass against an
// implementation that split by tokens, which is the mistake this feature exists to fix.
func TestCostByTier_SplitsTheTotalItAlreadyComputes(t *testing.T) {
	r := Rates{
		Base: [numTiers]float64{TierInput: 3e-06, TierCacheWrite: 3.75e-06, TierCacheRead: 3e-07, TierOutput: 1.5e-05},
		Set:  [numTiers]bool{TierInput: true, TierCacheWrite: true, TierCacheRead: true, TierOutput: true},
	}
	u := Usage{Input: 1000, CacheWrite: 2000, CacheRead: 100000, Output: 3000}

	tiers, total, ok, refusal := CostByTier(r, u)
	if !ok || refusal != RefusalNone {
		t.Fatalf("refused a priceable request: ok=%v refusal=%v", ok, refusal)
	}
	for tier, want := range map[Tier]int64{
		TierInput:      3000,  // 1000 x 3e-06
		TierCacheWrite: 7500,  // 2000 x 3.75e-06
		TierCacheRead:  30000, // 100000 x 3e-07
		TierOutput:     45000, // 3000 x 1.5e-05
	} {
		if tiers[tier] != want {
			t.Errorf("tier %d = %d micros, want %d", tier, tiers[tier], want)
		}
	}
	// The total is the one CostWithReason reports, because it is the same sum.
	wantTotal, wantOK, _ := CostWithReason(r, u)
	if !wantOK || total != wantTotal {
		t.Errorf("total = %d, want %d — CostByTier and CostWithReason disagree", total, wantTotal)
	}
	// A tier that carried no tokens is zero, and zero here means "no tokens", which is
	// why the caller needs a separate "is there a split at all" signal.
	none, _, _, _ := CostByTier(r, Usage{Output: 3000})
	if none[TierInput] != 0 {
		t.Errorf("input tier = %d for a request with no input tokens, want 0", none[TierInput])
	}
}

// Delegation changed no refusal. CostWithReason keeps its signature and its behaviour;
// this walks every way it can decline so the refactor cannot quietly reorder them.
func TestCostWithReason_RefusalsSurviveTheDelegation(t *testing.T) {
	full := Rates{
		Base: [numTiers]float64{TierInput: 3e-06, TierOutput: 1.5e-05},
		Set:  [numTiers]bool{TierInput: true, TierOutput: true},
	}
	for _, tc := range []struct {
		name string
		r    Rates
		u    Usage
		want Refusal
	}{
		{"no tokens at all", full, Usage{}, RefusalNoTokens},
		{"impossible count", full, Usage{Input: -1}, RefusalImpossibleCount},
		{"a tier with tokens and no rate", full, Usage{Input: 10, CacheRead: 10}, RefusalNoRate},
		{"a negative rate", Rates{
			Base: [numTiers]float64{TierInput: -1},
			Set:  [numTiers]bool{TierInput: true},
		}, Usage{Input: 10}, RefusalNoRate},
		{"priceable", full, Usage{Input: 10}, RefusalNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, got := CostWithReason(tc.r, tc.u); got != tc.want {
				t.Errorf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

```bash
cd authbridge/authlib && go test ./pricing/ -run 'TestCostByTier|TestCostWithReason_Refusals' -count=1
```

Expected: build failure, `undefined: CostByTier`. A build failure is the correct first result here; do not proceed until you have seen it.

- [ ] **Step 3: Add `NumTiers` and `CostByTier`**

In `pricing/rates.go`, beside the existing `const numTiers = 4`:

```go
// NumTiers is numTiers for callers outside this package, so a consumer sizing a
// per-tier array names the constant instead of writing 4 — a literal that would go
// stale silently if a tier were ever added, which is the failure numTiers' own doc
// says arrays exist to prevent.
const NumTiers = numTiers
```

In `pricing/cost.go`, replace the body of `CostWithReason` with a delegation and move its arithmetic — **and its comments, which explain that arithmetic** — into the new function:

```go
// CostByTier is Cost's arithmetic with the per-tier amounts kept instead of summed away.
//
// ONE PASS, ONE `eff`. The alternative is what Settle does for its prompt and output
// halves: call Cost again with the other tiers zeroed. That re-resolves the rate table
// against a MODIFIED prompt total on each call and re-applies the per-request
// plausibility ceiling to each fragment, which is forty lines of guard reasoning in
// costing.go. This returns a decomposition of the same float sum the total is built
// from, so the tiers are mutually consistent by construction and no fragment can pass
// or fail a bound the whole did not.
//
// THE TIERS NEED NOT SUM EXACTLY TO total. Each is converted to micros
// independently, so four truncations can differ from one by a few micros. Callers use
// them as a RATIO — see usage.Counts.ApportionTiers — where truncation is invisible.
// Nothing may present this array as a total.
func CostByTier(r Rates, u Usage) (tiers [NumTiers]int64, total int64, ok bool, refusal Refusal) {
	if u == (Usage{}) {
		return tiers, 0, false, RefusalNoTokens
	}
	if !PlausibleUsage(u) {
		return tiers, 0, false, RefusalImpossibleCount
	}
	eff := r.At(u.PromptTotal())
	var usd float64
	var perTier [NumTiers]float64
	for i, n := range u.tokens() {
		if n == 0 {
			continue
		}
		if !eff.Set[i] {
			return [NumTiers]int64{}, 0, false, RefusalNoRate
		}
		if rate := eff.Base[i]; rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return [NumTiers]int64{}, 0, false, RefusalNoRate
		}
		perTier[i] = float64(n) * eff.Base[i]
		usd += perTier[i]
	}
	micros, ok := MicrosFromUSD(usd)
	if !ok {
		return [NumTiers]int64{}, 0, false, RefusalUnrepresentable
	}
	if micros > MaxPlausibleRequestCostMicros {
		return [NumTiers]int64{}, 0, false, RefusalImplausibleTotal
	}
	for i, v := range perTier {
		// A tier that will not convert leaves THAT tier at zero rather than refusing the
		// request: the whole has already been bounded and accepted above, so the ratio is
		// merely less complete — the case ApportionTiers already handles — and refusing
		// here would unprice a request Cost priced.
		if m, ok := MicrosFromUSD(v); ok {
			tiers[i] = m
		}
	}
	return tiers, micros, true, RefusalNone
}
```

Then:

```go
func CostWithReason(r Rates, u Usage) (int64, bool, Refusal) {
	_, micros, ok, refusal := CostByTier(r, u)
	return micros, ok, refusal
}
```

`Cost` is unchanged — it already delegates to `CostWithReason`.

- [ ] **Step 4: Run the tests and watch them pass**

```bash
cd authbridge/authlib && go test ./pricing/ ./costing/ -count=1
```

Expected: PASS. Run `./costing/` too: it is the only caller of `CostWithReason`, and this step must not have moved its behaviour.

- [ ] **Step 5: Commit**

```bash
cd authbridge/authlib && gofmt -l pricing/ && go vet ./pricing/
git add pricing/cost.go pricing/rates.go pricing/cost_tier_test.go
git commit -s -m "feat(pricing): keep the per-tier amounts Cost already computes

CostWithReason summed four tier products into one float and discarded the parts on
the next line. The parts are the answer to where the money went, and recovering them
by re-pricing with tiers zeroed — the way Settle builds its prompt and output halves —
re-resolves the rate table against a modified prompt total and re-applies the
per-request ceiling to each fragment. Decomposing the sum that already passed those
checks needs neither.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 6: Prove the test can fail**

```bash
cd authbridge/authlib
perl -0pi -e 's/perTier\[i\] = float64\(n\) \* eff\.Base\[i\]/perTier[i] = float64(n)/' pricing/cost.go
grep -c 'perTier\[i\] = float64(n)$' pricing/cost.go   # must print 1
go test ./pricing/ -run TestCostByTier -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- pricing/cost.go
```

Expected: `--- FAIL: TestCostByTier_SplitsTheTotalItAlreadyComputes` with a tier mismatch, and **no** `build failed`. Splitting by tokens instead of by cost is the plausible wrong implementation; this is the control that it would be caught.

### Task 2: publish the split on the cost record

**Files:**
- Modify: `authbridge/authlib/costing/costing.go:625-640` (`modelledCost`), `:431-434` (the whole-request call), `:470` and `:478` (the two halves), `:517-520` (the magnitude-refusal clause), `:345-390` (`Settled`), `~:730-740` (the `costevent.Event` literal)
- Modify: `authbridge/authlib/costevent/costevent.go` — beside `PromptUSD`/`OutputUSD`
- Test: `authbridge/authlib/costing/costing_tier_test.go` (new)

**Interfaces:**
- Consumes: `pricing.CostByTier`, `pricing.NumTiers` from Task 1.
- Produces:
  ```go
  // in costevent
  type TierCost struct {
      Input      float64 `json:"input,omitempty"`
      CacheWrite float64 `json:"cache_write,omitempty"`
      CacheRead  float64 `json:"cache_read,omitempty"`
      Output     float64 `json:"output,omitempty"`
  }
  // on Event
  Tiers *TierCost `json:"tiers,omitempty"`
  // on costing.Settled
  TierUSD  [pricing.NumTiers]float64
  HasTiers bool
  ```
  PR 2 reads `Event.Tiers`. A **nil pointer** means no modelled split exists; a present struct with a zero member means that tier carried no tokens. That distinction is the whole reason it is a pointer.

- [ ] **Step 1: Write the failing test**

Create `authbridge/authlib/costing/costing_tier_test.go`:

```go
package costing

import "testing"

// A settled request carries the modelled split, and a nil Tiers means "no split" rather
// than "every tier free" — the distinction a zero-valued struct would destroy.
func TestSettle_PublishesTheModelledTierSplit(t *testing.T) {
	// Build the same fixture the package's other Settle tests use, priced from the rate
	// table with a cache-heavy usage split, then assert on the published record.
	//
	// IMPLEMENTER: reuse this package's existing Settle test helper rather than
	// constructing a pipeline.Context by hand — grep for the helper that the nearest
	// TestSettle_* case uses and follow it exactly. The assertions below are the point.
	rec := settleFixtureWithUsage(t, /* input */ 1000, /* cacheWrite */ 2000,
		/* cacheRead */ 100000, /* output */ 3000)

	if rec.Tiers == nil {
		t.Fatal("a priced request published no tier split")
	}
	// Output cost exceeds cache-read cost while cache-read tokens exceed output tokens
	// by 33x. A split derived from tokens would invert these two.
	if rec.Tiers.Output <= rec.Tiers.CacheRead {
		t.Errorf("output %.6f is not above cache-read %.6f — the split looks token-derived",
			rec.Tiers.Output, rec.Tiers.CacheRead)
	}
	if rec.Tiers.Input <= 0 || rec.Tiers.CacheWrite <= 0 {
		t.Errorf("a tier that carried tokens has no cost: %+v", *rec.Tiers)
	}
}

// No rate table means no split, and the record must say so by ABSENCE.
func TestSettle_NoRateTableLeavesTiersNil(t *testing.T) {
	rec := settleFixtureNoRates(t) // same shape, resolver returns ProvNone

	if rec.Tiers != nil {
		t.Errorf("Tiers = %+v for a request with no rates; nil is how the record says "+
			"\"no split\", and a zero struct would read as four free tiers", *rec.Tiers)
	}
}
```

Both helpers named above are the package's existing fixtures. If no helper matches, write the minimal one in this file and keep it unexported.

- [ ] **Step 2: Run the test and watch it fail**

```bash
cd authbridge/authlib && go test ./costing/ -run TestSettle_.*Tier -count=1
```

Expected: build failure on `rec.Tiers` being undefined.

- [ ] **Step 3: Add the record field**

In `costevent/costevent.go`, after `OutputUSD`:

```go
// TierCost is the modelled cost of one request, split by rate tier.
//
// Named fields rather than a positional array because this lands in an APPEND-ONLY
// FILE retained for thirty days: `"tiers":[0.003,0.0075,0.03,0.045]` needs the reader
// to know pricing.Tier's declaration order, and that order is an implementation detail
// of another package. Four names cost a few bytes each under omitempty and are
// self-describing to anyone reading the ledger with jq.
//
// MODELLED, NEVER AUTHORITATIVE — the same standing as PromptUSD, and for the same
// reason: a gateway reports one total for the call and never breaks it down. These are
// the rate table's answer even when CostUSD is the gateway's, so they do not sum to it
// and nothing may add them up and present the result as spend.
type TierCost struct {
	Input      float64 `json:"input,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	Output     float64 `json:"output,omitempty"`
}
```

and on `Event`:

```go
	// Tiers is the modelled per-tier split, or NIL when the rate table could not produce
	// one — a gateway-priced request on a model with no rates.
	//
	// A POINTER, deliberately. A zero-valued struct cannot distinguish "no split exists"
	// from "every tier cost nothing", and consumers apportion a real total by these
	// numbers: treating an absent split as four zeros divides by zero, and treating it as
	// a real one would report all traffic as free. Absence is the load-bearing state.
	Tiers *TierCost `json:"tiers,omitempty"`
```

- [ ] **Step 4: Carry it through `Settled`**

In `costing/costing.go`, add to `Settled` beside `OutputUSD`:

```go
	// TierUSD is the modelled cost per rate tier, indexed by pricing.Tier. Modelled with
	// the same standing as PromptUSD above; see costevent.TierCost for why it never sums
	// to CostUSD.
	TierUSD [pricing.NumTiers]float64
	// HasTiers says TierUSD holds a split. False leaves every element zero, and zero must
	// not be read as a free tier — the reason the published field is a pointer.
	HasTiers bool
```

Change `modelledCost` to return the array (line 625). Its two `return 0, pricing.ProvNone, ...` early exits gain a zero array in second position:

```go
func modelledCost(rates pricing.Resolver, host, model string, u pricing.Usage, promptTotal int) (
	int64, [pricing.NumTiers]int64, pricing.Provenance, bool, pricing.Refusal) {
	var none [pricing.NumTiers]int64
	if rates == nil || u == (pricing.Usage{}) {
		return 0, none, pricing.ProvNone, false, pricing.RefusalNoTokens
	}
	r, prov := rates.Resolve(host, model, promptTotal)
	if prov == pricing.ProvNone {
		return 0, none, pricing.ProvNone, false, pricing.RefusalNoRate
	}
	tiers, micros, ok, refusal := pricing.CostByTier(r, u)
	if !ok {
		return 0, none, pricing.ProvNone, false, refusal
	}
	return micros, tiers, prov, true, pricing.RefusalNone
}
```

At the whole-request call site (line 431), capture the split:

```go
	micros, tiers, prov, ok, refusal := modelledCost(rates, pctx.Host, model, usage, promptTotal)
	if ok {
		out.ModelledUSD, out.HasModelled, out.ModelledProv = float64(micros)/1e6, true, prov
		// From the WHOLE request's pricing, not from the two halves below: those re-price
		// with tiers zeroed, which resolves the rate table at a different prompt total.
		for i, m := range tiers {
			out.TierUSD[i] = float64(m) / 1e6
		}
		out.HasTiers = true
	}
```

At lines 470 and 478, the two halves discard it — add `_` in second position:

```go
	if micros, _, _, ok, _ := modelledCost(rates, pctx.Host, model, promptOnly, promptTotal); ok {
```

- [ ] **Step 5: Extend the magnitude-refusal clause**

The clause at line 517 zeroes both halves when the whole is refused for magnitude, on the reasoning that what the ceiling condemns is the rate table. The tiers come from that same table, so they go the same way:

```go
	if refusal.ImpossibleFigure() ||
		out.HasPrompt && out.HasOutput && !pricing.PlausibleRequestCostUSD(out.PromptUSD+out.OutputUSD) {
		out.PromptUSD, out.HasPrompt = 0, false
		out.OutputUSD, out.HasOutput = 0, false
		// SAME REASONING, SAME TABLE. A split drawn from rates the ceiling just condemned
		// would apportion a real total by a discredited shape — and unlike the halves it
		// would do so silently, since a ratio carries no magnitude to look wrong.
		out.TierUSD, out.HasTiers = [pricing.NumTiers]float64{}, false
	}
```

- [ ] **Step 6: Publish it on the event**

In the `costevent.Event` literal (~line 736), beside `PromptUSD`/`OutputUSD`:

```go
		Tiers: tierCostOf(s),
```

and add the helper next to that function:

```go
// tierCostOf publishes the split, or nil when there is none. Nil rather than an empty
// struct: see costevent.Event.Tiers.
func tierCostOf(s Settled) *costevent.TierCost {
	if !s.HasTiers {
		return nil
	}
	return &costevent.TierCost{
		Input:      s.TierUSD[pricing.TierInput],
		CacheWrite: s.TierUSD[pricing.TierCacheWrite],
		CacheRead:  s.TierUSD[pricing.TierCacheRead],
		Output:     s.TierUSD[pricing.TierOutput],
	}
}
```

- [ ] **Step 7: Run the tests**

```bash
cd authbridge/authlib && go test ./costing/ ./costevent/ ./pricing/ -count=1
```

Expected: PASS. If a `Settle` test fails on an unrelated figure, the `modelledCost` signature change dropped a return value at a call site — check all three.

- [ ] **Step 8: Commit**

```bash
cd authbridge/authlib && gofmt -l costing/ costevent/ && go vet ./costing/ ./costevent/
git add costing/costing.go costing/costing_tier_test.go costevent/costevent.go
git commit -s -m "feat(costing): publish the modelled per-tier split on the cost record

Taken from the WHOLE request's pricing rather than from the prompt and output halves:
those re-price with the other tiers zeroed, which resolves the rate table at a
different prompt total, so four figures built that way need not be mutually consistent.

Nil when no split exists, because a zero-valued struct cannot tell \"no split\" from
\"four free tiers\" and a consumer apportioning a real total by these numbers would
divide by zero on the first reading and report all traffic as free on the second.

A magnitude refusal of the whole now refuses the split too — same rate table, same
reasoning already applied to the two halves, and a discredited ratio carries no
magnitude to look wrong.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 9: Prove the nil case can fail**

```bash
cd authbridge/authlib
perl -0pi -e 's/if !s\.HasTiers \{\n\t\treturn nil\n\t\}//' costing/costing.go
grep -c "if !s.HasTiers" costing/costing.go   # must print 0
go test ./costing/ -run TestSettle_NoRateTableLeavesTiersNil -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- costing/costing.go
```

Expected: `--- FAIL: TestSettle_NoRateTableLeavesTiersNil`, no `build failed`. Always publishing a struct is the plausible wrong implementation and the one that breaks PR 2's apportionment.

# PR 2 — Aggregate

Branch: `feat/cost-tier-aggregate`, stacked on PR 1. Ships a machine-readable breakdown with no TUI risk.

**Read this before starting.** There are **two** sources of cost in the aggregate, and wiring one leaves the mix blank for traffic that arrived by the other:

1. the published record — `costevent.Record(resp)`, now carrying `Tiers`;
2. `usage`'s own fallback, `costOf` at `usage/usage.go:762`, which calls `pricing.Cost` directly when no record exists.

Both must populate the tier micros. `TestFoldInto_CarriesEveryCountsField` exercises the record path only, so the fallback needs its own test or it ships dead.

### Task 3: four fields on `Counts`

**Files:**
- Modify: `authbridge/authlib/usage/usage.go:148-153` (beside `CostMicros`), `:319-338` (`Add`)
- Test: the two existing reflection guards, `usage/usage_test.go:151` and `:1048` — they fail until this is done, which is the forcing function

**Interfaces:**
- Produces: `Counts.InputCostMicros`, `Counts.CacheWriteCostMicros`, `Counts.CacheReadCostMicros`, `Counts.OutputCostMicros`, all `int64`, JSON `inputCostMicros` / `cacheWriteCostMicros` / `cacheReadCostMicros` / `outputCostMicros`, all `omitempty`.

- [ ] **Step 1: Run the guards first and watch them pass**

```bash
cd authbridge/authlib && go test ./usage/ -run 'TestCountsAdd_EveryInt64FieldSaturatesAndSaysSo|TestFoldInto_CarriesEveryCountsField' -count=1
```

Expected: PASS. This is the baseline — you need to see these green *before* adding fields so that the red in Step 3 is attributable.

- [ ] **Step 2: Add the fields**

In `usage/usage.go`, immediately after `CostMicros`:

```go
	// The modelled cost of each token tier, summed over the requests in this bucket.
	//
	// ADJACENT TO CostMicros, not filed with the token counters they parallel, because the
	// distinction that matters is not "tokens versus money" but AUTHORITATIVE VERSUS
	// MODELLED. CostMicros may be a gateway's own figure; these four are always the rate
	// table's, so they need not sum to it and a reader who assumes they do is wrong. Put
	// beside the field they qualify, that is visible in one glance.
	//
	// USED AS A RATIO, never as a total — see ApportionTiers. All four zero means no
	// modelled split reached this bucket, which is NOT a claim that the traffic was free.
	InputCostMicros      int64 `json:"inputCostMicros,omitempty"`
	CacheWriteCostMicros int64 `json:"cacheWriteCostMicros,omitempty"`
	CacheReadCostMicros  int64 `json:"cacheReadCostMicros,omitempty"`
	OutputCostMicros     int64 `json:"outputCostMicros,omitempty"`
```

- [ ] **Step 3: Run the guards and watch them fail**

```bash
cd authbridge/authlib && go test ./usage/ -run 'TestCountsAdd_EveryInt64FieldSaturatesAndSaysSo|TestFoldInto_CarriesEveryCountsField' -count=1
```

Expected: both FAIL, naming the four new fields. This is the repo telling you the wiring is incomplete. If they pass, the reflection guards are not seeing the fields and something is wrong with the struct tags — stop and investigate.

- [ ] **Step 4: Wire `Add`**

In `Counts.Add`, after the `CostMicros` line:

```go
	c.addInto(&c.InputCostMicros, o.InputCostMicros)
	c.addInto(&c.CacheWriteCostMicros, o.CacheWriteCostMicros)
	c.addInto(&c.CacheReadCostMicros, o.CacheReadCostMicros)
	c.addInto(&c.OutputCostMicros, o.OutputCostMicros)
```

`addInto` is the checked accumulate; it sets `Saturated` on overflow. Do not use `+=`.

- [ ] **Step 5: Run the saturation guard**

```bash
cd authbridge/authlib && go test ./usage/ -run TestCountsAdd_EveryInt64FieldSaturatesAndSaysSo -count=1
```

Expected: PASS. `TestFoldInto_CarriesEveryCountsField` still fails — Task 4 fixes it.

- [ ] **Step 6: Commit**

```bash
cd authbridge/authlib && gofmt -l usage/ && go vet ./usage/
git add usage/usage.go
git commit -s -m "feat(usage): carry the modelled per-tier cost on Counts

Placed beside CostMicros rather than with the token counters they parallel: the
distinction that matters is authoritative versus modelled, not tokens versus money.
CostMicros may be a gateway's figure while these are always the rate table's, so they
do not sum to it.

costledger.Row embeds Counts, so the durable ledger, /v1/usage and the Fold query path
all gain the fields here.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Task 4: populate the tiers from both cost sources

**Files:**
- Modify: `authbridge/authlib/usage/usage.go:635-660` (`eventCost`), the record-reading branch of `costOf`, the fallback at `:762`, and the `one := Counts{...}` literal at `:1166`
- Test: `authbridge/authlib/usage/usage_tier_test.go` (new), plus the now-failing `TestFoldInto_CarriesEveryCountsField`

**Interfaces:**
- Consumes: `costevent.Event.Tiers *TierCost` and `pricing.CostByTier` from PR 1.
- Produces: `eventCost.tierMicros [pricing.NumTiers]int64`, folded into the four `Counts` fields.

- [ ] **Step 1: Write the failing test**

Create `authbridge/authlib/usage/usage_tier_test.go`:

```go
package usage

import "testing"

// The FALLBACK path prices events itself, and it must produce the same split the record
// path carries. This test exists because TestFoldInto_CarriesEveryCountsField uses a
// published record, so the fallback could ship entirely dead behind a green suite.
func TestAggregator_TheFallbackPathCarriesTheTierSplit(t *testing.T) {
	// IMPLEMENTER: build this with the package's existing fallback fixture — the one used
	// by the nearest test that asserts on SourceUsageFallback pricing, with a rates
	// resolver and an inference event carrying NO cost record. Assertions are the point.
	snap := fallbackFixtureWithUsage(t, /* input */ 1000, /* cacheWrite */ 2000,
		/* cacheRead */ 100000, /* output */ 3000)
	got := snap.Totals

	if got.CostMicros == 0 {
		t.Fatal("setup: the fallback path priced nothing, so this asserts nothing")
	}
	if got.OutputCostMicros <= got.CacheReadCostMicros {
		t.Errorf("output %d is not above cache-read %d — the fallback split looks "+
			"token-derived, or was never populated", got.OutputCostMicros, got.CacheReadCostMicros)
	}
	for name, v := range map[string]int64{
		"input": got.InputCostMicros, "cache-write": got.CacheWriteCostMicros,
		"cache-read": got.CacheReadCostMicros, "output": got.OutputCostMicros,
	} {
		if v == 0 {
			t.Errorf("%s tier is zero on a request that used every tier", name)
		}
	}
}

// A request the rate table cannot price contributes NO mix, and must not contribute
// zeros that would dilute a mix drawn from its neighbours.
func TestAggregator_AnUnpricedRequestAddsNoMix(t *testing.T) {
	// IMPLEMENTER: same fixture, resolver returning ProvNone.
	snap := fallbackFixtureNoRates(t)
	got := snap.Totals

	if got.InputCostMicros != 0 || got.OutputCostMicros != 0 {
		t.Errorf("an unpriced request contributed a mix: %+v", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
cd authbridge/authlib && go test ./usage/ -run 'TestAggregator_.*Tier|TestAggregator_AnUnpriced' -count=1
```

Expected: FAIL — the four fields are zero because nothing populates them yet.

- [ ] **Step 3: Add the field to `eventCost`**

```go
	// tierMicros is the modelled cost of each rate tier, indexed by pricing.Tier.
	//
	// Filled from the published record's Tiers when there is one and from this package's
	// own pricing when there is not. BOTH sources, because a deployment can produce either
	// and a mix populated on one path only is blank for half the traffic while every test
	// on the other path stays green.
	tierMicros [pricing.NumTiers]int64
```

- [ ] **Step 4: Fill it on the record path**

Where `costOf` reads the published record, after the cost figure is taken:

```go
	// Absent Tiers is the normal case for a gateway-priced request on a model with no
	// rates: no split exists, and leaving the array zero is how that is said. See
	// costevent.Event.Tiers for why nil and zero are different states.
	if t := ce.Tiers; t != nil {
		ec.tierMicros = [pricing.NumTiers]int64{
			pricing.TierInput:      pricing.MicrosOrZero(t.Input),
			pricing.TierCacheWrite: pricing.MicrosOrZero(t.CacheWrite),
			pricing.TierCacheRead:  pricing.MicrosOrZero(t.CacheRead),
			pricing.TierOutput:     pricing.MicrosOrZero(t.Output),
		}
	}
```

`MicrosOrZero` does not exist yet. Add it to `pricing` beside `MicrosFromUSD`:

```go
// MicrosOrZero is MicrosFromUSD for a caller that has nothing useful to do with a
// refusal. Used for figures that are a RATIO rather than a total, where an
// unrepresentable component makes the ratio less complete and nothing else.
func MicrosOrZero(usd float64) int64 {
	m, ok := MicrosFromUSD(usd)
	if !ok {
		return 0
	}
	return m
}
```

- [ ] **Step 5: Fill it on the fallback path**

At `usage/usage.go:762`, swap `pricing.Cost` for `pricing.CostByTier` — the total is identical, so nothing else in this branch changes:

```go
	tiers, micros, ok, _ := pricing.CostByTier(rates, u)
	if !ok {
		// ... the existing unpriced return, unchanged ...
	}
	ec := eventCost{micros: micros, priced: 1, priceable: 1, provenance: prov.String()}
	ec.tierMicros = tiers
```

- [ ] **Step 6: Carry it in `foldInto`**

In the `one := Counts{...}` literal at line 1166:

```go
		InputCostMicros:      ec.tierMicros[pricing.TierInput],
		CacheWriteCostMicros: ec.tierMicros[pricing.TierCacheWrite],
		CacheReadCostMicros:  ec.tierMicros[pricing.TierCacheRead],
		OutputCostMicros:     ec.tierMicros[pricing.TierOutput],
```

- [ ] **Step 7: Run the full usage suite**

```bash
cd authbridge/authlib && go test ./usage/ ./costledger/ -count=1
```

Expected: PASS, including both reflection guards and `./costledger/` — `Row` embeds `Counts`, so its round-trip tests cover the new fields for free.

- [ ] **Step 8: Commit**

```bash
cd authbridge/authlib && gofmt -l usage/ pricing/ && go vet ./usage/ ./pricing/
git add usage/usage.go usage/usage_tier_test.go pricing/cost.go
git commit -s -m "feat(usage): populate the tier split from both cost sources

There are two sources of cost in the aggregate — the published record and this
package's own pricing fallback — and wiring one leaves the mix blank for traffic that
arrived by the other. TestFoldInto_CarriesEveryCountsField exercises the record path
only, so the fallback gets its own test rather than shipping dead behind a green suite.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 9: Prove the fallback test can fail**

```bash
cd authbridge/authlib
perl -0pi -e 's/\tec\.tierMicros = tiers\n//' usage/usage.go
grep -c "ec.tierMicros = tiers" usage/usage.go   # must print 0
go test ./usage/ -run TestAggregator_TheFallbackPathCarriesTheTierSplit -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- usage/usage.go
```

Expected: FAIL naming a zero tier, no `build failed`. If it passes, the test is reaching the record path and not the fallback — fix the fixture before continuing, because this control is the only thing standing between you and a dead code path.

### Task 5: `Counts.ApportionTiers`

The one place the modelled mix is applied to the authoritative total. Every surface calls it, so no two can disagree about a figure derived twice.

**Files:**
- Create: `authbridge/authlib/usage/apportion.go`
- Test: `authbridge/authlib/usage/apportion_test.go`

**Interfaces:**
- Produces: `func (c Counts) ApportionTiers() (tiers [pricing.NumTiers]int64, ok bool)`. `ok` false means no mix exists; callers render `emptyCell`. When true, the four values sum **exactly** to `c.CostMicros`.

- [ ] **Step 1: Write the failing test**

Create `authbridge/authlib/usage/apportion_test.go`:

```go
package usage

import (
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// The four tiers sum EXACTLY to CostMicros, which is the property the whole display
// rests on: a reader checks the column against the headline above it first.
//
// The totals are chosen so integer division leaves a remainder — 100003 against a mix
// of 3 is the case a test using round numbers would miss entirely, and the remainder
// rule is the only reason the assertion can hold.
func TestApportionTiers_SumsToTheAuthoritativeTotalExactly(t *testing.T) {
	for _, total := range []int64{1, 7, 999, 100003, 1234567, 9999999999} {
		c := Counts{
			CostMicros:           total,
			InputCostMicros:      3000,
			CacheWriteCostMicros: 7500,
			CacheReadCostMicros:  30000,
			OutputCostMicros:     45000,
		}
		tiers, ok := c.ApportionTiers()
		if !ok {
			t.Fatalf("total %d: refused a Counts with a mix", total)
		}
		var sum int64
		for _, v := range tiers {
			sum += v
			if v < 0 {
				t.Errorf("total %d: negative tier %d", total, v)
			}
		}
		if sum != total {
			t.Errorf("total %d: tiers sum to %d, off by %d", total, sum, sum-total)
		}
	}
}

// The mix is the RATIO, so the biggest modelled tier is the biggest apportioned one.
func TestApportionTiers_PreservesTheRanking(t *testing.T) {
	c := Counts{
		CostMicros:           1_000_000,
		InputCostMicros:      3000,
		CacheWriteCostMicros: 7500,
		CacheReadCostMicros:  30000,
		OutputCostMicros:     45000, // the largest, and the remainder lands here
	}
	tiers, ok := c.ApportionTiers()
	if !ok {
		t.Fatal("refused a Counts with a mix")
	}
	if tiers[pricing.TierOutput] <= tiers[pricing.TierCacheRead] {
		t.Errorf("output %d did not stay above cache-read %d",
			tiers[pricing.TierOutput], tiers[pricing.TierCacheRead])
	}
	// Roughly 52% of the total, since output is 45000 of an 85500 mix.
	if got := tiers[pricing.TierOutput]; got < 520_000 || got > 530_000 {
		t.Errorf("output apportioned to %d, want ~526000 (45000/85500 of 1000000)", got)
	}
}

// No mix means no answer. Not four zeros, which a caller would render as four free
// tiers, and not a divide by zero.
func TestApportionTiers_RefusesWithNoMix(t *testing.T) {
	if _, ok := (Counts{CostMicros: 500_000}).ApportionTiers(); ok {
		t.Error("apportioned a total with no modelled mix at all")
	}
	// And a mix with no total is nothing to apportion either.
	if _, ok := (Counts{InputCostMicros: 3000}).ApportionTiers(); ok {
		t.Error("apportioned a zero total")
	}
}

// A mix covering a tiny fraction of the spend still apportions. This is the positive
// control for having NO coverage threshold: any reintroduced floor blanks this case,
// and the spec's reasoning is that no particular floor is defensible.
func TestApportionTiers_ASmallMixStillApportions(t *testing.T) {
	c := Counts{CostMicros: 30_935_000, InputCostMicros: 12} // 12 micros of mix, $30.93 of spend
	tiers, ok := c.ApportionTiers()
	if !ok {
		t.Fatal("a small mix was refused — has a coverage floor come back?")
	}
	if tiers[pricing.TierInput] != 30_935_000 {
		t.Errorf("input = %d, want the whole total: it is the only tier in the mix",
			tiers[pricing.TierInput])
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
cd authbridge/authlib && go test ./usage/ -run TestApportionTiers -count=1
```

Expected: build failure, `c.ApportionTiers undefined`.

- [ ] **Step 3: Implement it**

Create `authbridge/authlib/usage/apportion.go`:

```go
package usage

import "github.com/rossoctl/cortex/authbridge/authlib/pricing"

// ApportionTiers splits CostMicros across the rate tiers by the modelled mix.
//
// THE MIX IS THE RATE TABLE'S, THE MAGNITUDE IS THE GATEWAY'S. A request priced from a
// response header has an authoritative total and no breakdown, because no gateway
// publishes one — so the modelled figures cannot be displayed as dollars beside it
// without putting two disagreeing totals on screen. Used as a ratio they answer the
// question anyway, and the column adds up to the headline a reader checks first.
//
// ok is false when there is no mix (nothing to apportion by) or no total (nothing to
// apportion). Callers render emptyCell — NOT four zeros, which would report the
// traffic as free, and not a guess.
//
// NO COVERAGE THRESHOLD, deliberately. A mix drawn from one request of forty is a weak
// key, but every particular floor is a number nobody can defend — 50% and 10% are
// equally arbitrary — and a constant whose value is unjustifiable is worse than the
// behaviour it guards. The display marks every figure with `~` instead, and a reader
// who wants coverage has `abctl cost`, which reports priced against priceable already.
func (c Counts) ApportionTiers() (tiers [pricing.NumTiers]int64, ok bool) {
	mix := [pricing.NumTiers]int64{
		pricing.TierInput:      c.InputCostMicros,
		pricing.TierCacheWrite: c.CacheWriteCostMicros,
		pricing.TierCacheRead:  c.CacheReadCostMicros,
		pricing.TierOutput:     c.OutputCostMicros,
	}
	var mixTotal int64
	for _, v := range mix {
		if v > 0 {
			mixTotal += v
		}
	}
	if mixTotal <= 0 || c.CostMicros <= 0 {
		return tiers, false
	}

	// THE RATIO IN FLOAT, THE RESULT BOUNDED BY THE TOTAL. The integer form,
	// CostMicros*mix[i]/mixTotal, overflows int64 on the multiply for a large total
	// against a large mix; scaling instead keeps every intermediate under CostMicros,
	// a quantity already known to fit.
	var sum int64
	largest := 0
	for i, v := range mix {
		if v <= 0 {
			continue
		}
		tiers[i] = int64(float64(c.CostMicros) * (float64(v) / float64(mixTotal)))
		sum += tiers[i]
		if mix[i] > mix[largest] {
			largest = i
		}
	}
	// THE REMAINDER GOES TO THE LARGEST TIER. Four truncations need not sum to one
	// total, and the display's whole claim is that they do. Largest because that is
	// where a micro is proportionally smallest and cannot flip a rank — assigning it
	// to the smallest tier could reorder the bars, which is a visible lie in service
	// of an invisible one.
	if r := c.CostMicros - sum; r != 0 {
		tiers[largest] += r
	}
	return tiers, true
}
```

- [ ] **Step 4: Run the tests**

```bash
cd authbridge/authlib && go test ./usage/ -count=1
```

Expected: PASS, all four cases.

- [ ] **Step 5: Commit**

```bash
cd authbridge/authlib && gofmt -l usage/ && go vet ./usage/
git add usage/apportion.go usage/apportion_test.go
git commit -s -m "feat(usage): apportion the authoritative total by the modelled mix

One helper, called by every surface, so the drawer, the text command and the JSON
cannot disagree about a figure derived twice.

The ratio is computed in float and applied to the total: the integer form overflows
int64 on the multiply. The truncation remainder goes to the largest tier, where a micro
is proportionally smallest and cannot flip a rank — without that rule the column does
not sum to the headline, which is the property the display rests on.

No coverage threshold. A mix from one request of forty is a weak key, but every
particular floor is indefensible and the ~ marker already says the figure is inexact.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 6: Two mutation controls**

```bash
cd authbridge/authlib
# (a) drop the remainder rule — the exact-sum property must break
perl -0pi -e 's/\tif r := c\.CostMicros - sum; r != 0 \{\n\t\ttiers\[largest\] \+= r\n\t\}//' usage/apportion.go
grep -c "tiers\[largest\] += r" usage/apportion.go   # must print 0
go test ./usage/ -run TestApportionTiers_SumsToTheAuthoritativeTotalExactly -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- usage/apportion.go

# (b) reintroduce a coverage floor — the small-mix control must break
perl -0pi -e 's/if mixTotal <= 0 \|\| c\.CostMicros <= 0 \{/if mixTotal*2 < c.CostMicros || c.CostMicros <= 0 {/' usage/apportion.go
grep -c "mixTotal\*2 < c.CostMicros" usage/apportion.go   # must print 1
go test ./usage/ -run TestApportionTiers_ASmallMixStillApportions -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- usage/apportion.go
```

Expected: (a) FAIL with "off by" a small number; (b) FAIL with "has a coverage floor come back?". Neither may print `build failed`.

### Task 6: expose it in `abctl cost --json`

**Files:**
- Modify: `authbridge/cmd/abctl/cmd_cost.go` — the `costJSON` struct and its construction
- Test: `authbridge/cmd/abctl/cmd_cost_test.go`

**Interfaces:**
- Consumes: `usage.Counts.ApportionTiers` from Task 5.
- Produces: a `tiers` object on the JSON output, with `input` / `cacheWrite` / `cacheRead` / `output` micros. Absent entirely when `ok` is false.

- [ ] **Step 1: Write the failing test**

Add to `authbridge/cmd/abctl/cmd_cost_test.go`:

```go
// The JSON carries the apportioned split, and omits it rather than emitting zeros when
// there is no mix — a scripted consumer summing four zeros would report free traffic.
func TestCostJSON_CarriesTheApportionedTiersOrOmitsThem(t *testing.T) {
	// IMPLEMENTER: follow the nearest existing writeCostJSON test for the harness; these
	// two assertions are the point.
	withMix := jsonForSnapshot(t, &usage.Snapshot{
		Window: "1h", Priced: true,
		Totals: usage.Counts{
			Requests: 35, CostMicros: 4_546_200,
			InputCostMicros: 3000, CacheWriteCostMicros: 7500,
			CacheReadCostMicros: 30000, OutputCostMicros: 45000,
		},
	})
	if !strings.Contains(withMix, `"tiers"`) {
		t.Errorf("no tiers in the JSON for a snapshot with a mix:\n%s", withMix)
	}
	if !strings.Contains(withMix, `"cacheRead"`) {
		t.Errorf("tiers present but cacheRead missing:\n%s", withMix)
	}

	noMix := jsonForSnapshot(t, &usage.Snapshot{
		Window: "1h", Priced: true,
		Totals: usage.Counts{Requests: 35, CostMicros: 4_546_200},
	})
	if strings.Contains(noMix, `"tiers"`) {
		t.Errorf("tiers emitted for a snapshot with no modelled mix:\n%s", noMix)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
cd authbridge/cmd/abctl && go test ./ -run TestCostJSON_CarriesTheApportioned -count=1
```

Expected: FAIL — no `tiers` key.

- [ ] **Step 3: Add the field**

In `cmd_cost.go`, beside the existing money fields on `costJSON`:

```go
	// Tiers is the apportioned per-tier split, or nil when no modelled mix reached this
	// window. OMITTED rather than zeroed: a consumer summing four zeros would report the
	// traffic as free, which is the same lie the human surface refuses with emptyCell.
	Tiers *costTiersJSON `json:"tiers,omitempty"`
```

```go
// costTiersJSON mirrors the human column: micros, apportioned, inexact by construction.
type costTiersJSON struct {
	Input      int64 `json:"input"`
	CacheWrite int64 `json:"cacheWrite"`
	CacheRead  int64 `json:"cacheRead"`
	Output     int64 `json:"output"`
}
```

and where the struct is built:

```go
	if tiers, ok := snap.Totals.ApportionTiers(); ok {
		out.Tiers = &costTiersJSON{
			Input:      tiers[pricing.TierInput],
			CacheWrite: tiers[pricing.TierCacheWrite],
			CacheRead:  tiers[pricing.TierCacheRead],
			Output:     tiers[pricing.TierOutput],
		}
	}
```

- [ ] **Step 4: Run the tests**

```bash
cd authbridge/cmd/abctl && go test ./ -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd authbridge/cmd/abctl && gofmt -l . && go vet ./...
git add cmd_cost.go cmd_cost_test.go
git commit -s -m "feat(abctl): report the apportioned tier split in cost --json

Omitted rather than zeroed when no modelled mix reached the window: a consumer summing
four zeros would report the traffic as free, the same lie the human surface refuses
with an em-dash.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

# PR 3 — Render

Branch: `feat/cost-tier-render`, stacked on PR 2. **This is the risky PR.** This surface has already produced a resize panic, a five-row overflow and a six-row under-fill, all in the reservation arithmetic Task 9 touches. Tasks 7 and 8 are pure functions with no reservation involvement; do them first and commit them separately, so that if Task 9 goes wrong the bisect lands on the right change.

### Task 7: the tier rows

**Files:**
- Create: `authbridge/cmd/abctl/tui/spend_tiers.go`
- Test: `authbridge/cmd/abctl/tui/spend_tiers_test.go`

**Interfaces:**
- Consumes: `usage.Counts.ApportionTiers`, and the existing `emptyCell`, `inexactMarker`, `formatUSDCell`, `negativeCost` from this package.
- Produces: `func renderTierRows(c usage.Counts, width int) []string` — always exactly `numTierRows` lines, padded with blanks. `const numTierRows = 4`.

- [ ] **Step 1: Write the failing test**

Create `authbridge/cmd/abctl/tui/spend_tiers_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

func tierCounts() usage.Counts {
	return usage.Counts{
		Requests: 35, CostMicros: 4_546_200,
		InputCostMicros: 3000, CacheWriteCostMicros: 7500,
		CacheReadCostMicros: 30000, OutputCostMicros: 45000,
	}
}

// Ranked by MONEY, descending — and the fixture's token order is the opposite, so a
// renderer ordering by pricing.Tier declaration or by token count fails here. Ordering
// both the same way would let either implementation pass.
func TestRenderTierRows_RanksByCostNotByDeclarationOrder(t *testing.T) {
	lines := renderTierRows(tierCounts(), 60)
	if len(lines) != numTierRows {
		t.Fatalf("lines = %d, want exactly %d: %q", len(lines), numTierRows, lines)
	}
	want := []string{"output", "cache-read", "input", "cache-write"}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("row %d = %q, want %q (full: %q)", i, lines[i], w, lines)
		}
	}
	// pricing.Tier declares input first; if that order leaked through, row 0 says input.
	if strings.Contains(lines[0], "input") {
		t.Errorf("row 0 is the input tier, so the rows follow declaration order, not cost")
	}
}

// Every figure wears the inexact marker, because the split is modelled throughout.
func TestRenderTierRows_MarksEveryFigureInexact(t *testing.T) {
	for _, line := range renderTierRows(tierCounts(), 60) {
		if line == "" {
			continue
		}
		if !strings.Contains(line, inexactMarker+"$") {
			t.Errorf("row %q carries a figure with no %q marker", line, inexactMarker)
		}
	}
}

// No mix means em-dashes, never $0.00 and never a guess.
func TestRenderTierRows_NoMixRendersTheUnknownCell(t *testing.T) {
	lines := renderTierRows(usage.Counts{Requests: 35, CostMicros: 4_546_200}, 60)
	if len(lines) != numTierRows {
		t.Fatalf("lines = %d, want %d even with no mix", len(lines), numTierRows)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, emptyCell) {
		t.Errorf("no mix but no %q cell:\n%s", emptyCell, joined)
	}
	if strings.Contains(joined, "$0.00") {
		t.Errorf("rendered $0.00 for an unknown tier:\n%s", joined)
	}
}

// Reasoning is never a bar and never a figure here.
//
// It is a SUBSET of output — tokenSplit already labels it "reasoning (of output)" — so a
// fifth row would double-count the same money. The fixture reports reasoning tokens
// precisely so that a renderer enumerating token kinds instead of rate tiers fails: there
// are five token kinds on Counts and four tiers, and that difference is the whole point.
func TestRenderTierRows_ReasoningIsNotATier(t *testing.T) {
	c := tierCounts()
	c.ReasoningTokens = 12_000
	c.OutputTokens = 42_000
	lines := renderTierRows(c, 60)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "reasoning") {
		t.Errorf("reasoning appears as a tier row, double-counting output:\n%s", joined)
	}
	if len(lines) != numTierRows {
		t.Errorf("lines = %d, want %d — reasoning added a row", len(lines), numTierRows)
	}
}

// The line count is IDENTICAL across every state. The panel sits in a fixed
// reservation, and a renderer whose height depends on its data is what produced the
// overflow and under-fill defects already fixed on this surface.
func TestRenderTierRows_HeightIsConstant(t *testing.T) {
	for name, c := range map[string]usage.Counts{
		"full mix":   tierCounts(),
		"no mix":     {Requests: 35, CostMicros: 4_546_200},
		"empty":      {},
		"one tier":   {CostMicros: 4_546_200, OutputCostMicros: 45000},
		"negative":   {CostMicros: -5, OutputCostMicros: 45000},
	} {
		for _, w := range []int{20, 40, 60, 100, 200} {
			if got := len(renderTierRows(c, w)); got != numTierRows {
				t.Errorf("%s at width %d: %d lines, want %d", name, w, got, numTierRows)
			}
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
cd authbridge/cmd/abctl && go test ./tui/ -run TestRenderTierRows -count=1
```

Expected: build failure, `undefined: renderTierRows`.

- [ ] **Step 3: Implement it**

Create `authbridge/cmd/abctl/tui/spend_tiers.go`:

```go
package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// numTierRows is the height of the tier column, in every state.
//
// FOUR, FIXED, because the panel lives in a reservation layout() computes from the
// terminal height and paneView pads to. A renderer whose height follows its data is
// exactly what overflowed this surface by five rows and under-filled it by six, so the
// count is a constant and the no-data case pads rather than shortens.
//
// Four and not five: reasoning is a subset of output, not a sibling tier, so including
// it would double-count. It stays in `abctl cost`'s token line.
const numTierRows = 4

// tierBarWidth is the widest a bar may be. Bars are decoration over a figure that is
// already printed, so they yield their space first as the terminal narrows.
const tierBarWidth = 12

var tierLabels = map[pricing.Tier]string{
	pricing.TierInput:      "input",
	pricing.TierCacheWrite: "cache-write",
	pricing.TierCacheRead:  "cache-read",
	pricing.TierOutput:     "output",
}

// renderTierRows is the "where the money went" column: one row per rate tier, ranked by
// what it cost, with a proportional bar and the apportioned figure.
//
// SOLID BLOCKS, not the letter marks usage_stacked.go uses. That file rejected shaded
// blocks because adjacent segments of one stacked bar were indistinguishable at a
// glance; here each bar occupies its own line beside its own label, so there is no
// neighbour to disambiguate from. Colour stays decoration — the label carries the
// identity, so this survives a monochrome terminal.
//
// EVERY FIGURE WEARS inexactMarker. The mix is the rate table's while the total may be
// the gateway's, so no figure here is exact even though the column sums to one that is.
func renderTierRows(c usage.Counts, width int) []string {
	out := make([]string, 0, numTierRows)
	tiers, ok := c.ApportionTiers()

	order := []pricing.Tier{
		pricing.TierInput, pricing.TierCacheWrite,
		pricing.TierCacheRead, pricing.TierOutput,
	}
	// Ranked by cost, descending, ties broken on the label so two equal tiers do not
	// swap places between polls and make the panel flicker. Same rule the model column
	// follows; see rankSeriesByCost.
	sort.SliceStable(order, func(i, j int) bool {
		a, b := tiers[order[i]], tiers[order[j]]
		if a != b {
			return a > b
		}
		return tierLabels[order[i]] < tierLabels[order[j]]
	})

	var peak int64
	for _, v := range tiers {
		if v > peak {
			peak = v
		}
	}
	for _, tier := range order {
		label := tierLabels[tier]
		if !ok {
			// NOT KNOWN HERE, which is what emptyCell means — never $0.00, and never a
			// figure apportioned from a mix that does not exist.
			out = append(out, fmt.Sprintf("%-12s %s", label, emptyCell))
			continue
		}
		bar := tierBar(tiers[tier], peak, barBudget(width))
		out = append(out, fmt.Sprintf("%-12s %-*s %s%s",
			label, barBudget(width), bar,
			inexactMarker, formatUSDCell(float64(tiers[tier])/1e6)))
	}
	for len(out) < numTierRows {
		out = append(out, "")
	}
	return out
}

// barBudget is how many cells a bar may use at this width: the full width on a roomy
// terminal, nothing at all when the figures themselves are the priority.
func barBudget(width int) int {
	switch {
	case width >= 46:
		return tierBarWidth
	case width >= 34:
		return tierBarWidth / 2
	default:
		return 0
	}
}

// tierBar is a proportional bar, with a partial block so a small non-zero share is
// visible rather than rounding away to nothing — the same reason sessionMoneyCell skips
// a rung that would render a real charge as zero.
func tierBar(v, peak int64, budget int) string {
	if budget <= 0 || peak <= 0 || v <= 0 {
		return ""
	}
	eighths := int(float64(v) / float64(peak) * float64(budget) * 8)
	full, rem := eighths/8, eighths%8
	if full > budget {
		full, rem = budget, 0
	}
	bar := strings.Repeat("█", full)
	if full < budget && rem > 0 {
		bar += string([]rune("▏▎▍▌▋▊▉")[rem-1])
	}
	if bar == "" {
		bar = "▏"
	}
	return bar
}
```

- [ ] **Step 4: Run the tests**

```bash
cd authbridge/cmd/abctl && go test ./tui/ -run TestRenderTierRows -count=1
```

Expected: PASS, all four.

- [ ] **Step 5: Commit**

```bash
cd authbridge/cmd/abctl && gofmt -l tui/ && go vet ./tui/
git add tui/spend_tiers.go tui/spend_tiers_test.go
git commit -s -m "feat(abctl): render the per-tier cost column

Solid blocks rather than the letter marks usage_stacked.go uses: that file rejected
shading because adjacent segments of one stacked bar were indistinguishable, and here
each bar sits on its own line beside its own label with no neighbour to confuse it with.

Fixed at four rows in every state, padded rather than shortened, because the panel lives
in a reservation and a data-dependent height is what overflowed this surface by five
rows and under-filled it by six.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 6: Two mutation controls**

```bash
cd authbridge/cmd/abctl
# (a) rank by declaration order instead of by cost
perl -0pi -e 's/\tsort\.SliceStable\(order, func\(i, j int\) bool \{/\tsort.SliceStable(order, func(i, j int) bool { return false\n\t_ = func(i, j int) bool {/' tui/spend_tiers.go
go test ./tui/ -run TestRenderTierRows_RanksByCost -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- tui/spend_tiers.go

# (b) shorten the column when there is no mix
perl -0pi -e 's/\tfor len\(out\) < numTierRows \{\n\t\tout = append\(out, ""\)\n\t\}//' tui/spend_tiers.go
grep -c "for len(out) < numTierRows" tui/spend_tiers.go   # must print 0
go test ./tui/ -run TestRenderTierRows_HeightIsConstant -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- tui/spend_tiers.go
```

Expected: (a) FAIL on row 0 being the input tier — if it prints `build failed`, rewrite the mutation by hand, since the point is to prove the ordering assertion bites; (b) FAIL with a line count below four. Verify each applied with the `grep -c` before trusting the result.

### Task 8: the KPI band

**Files:**
- Create: `authbridge/cmd/abctl/tui/spend_band.go`, `tui/spend_band_test.go`
- Read but do not change yet: `tui/spend_strip.go:284` (`renderSpendStrip`) — the band reuses its `spendSummary` and its figure-dropping ladder

**Interfaces:**
- Consumes: the existing `spendSummary` struct from `tui/spend.go:371`.
- Produces: `func renderSpendBand(s spendSummary, width int) []string` — exactly `spendBandLines` (2) lines: labels, then values, column-aligned.

- [ ] **Step 1: Write the failing test**

Create `authbridge/cmd/abctl/tui/spend_band_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func bandSummary() spendSummary {
	return spendSummary{
		TodayUSD: 3.8402, HasToday: true,
		WindowUSD: 4.5462, HasWindow: true, WindowLabel: "1h",
		SavedUSD: 0.2091, HasSaved: true,
		CacheHitPct: 93, HasCacheHit: true,
		Tokens: 5_600_000, Priced: true,
	}
}

// Labels above values, and each value starts at its label's column. The alignment IS
// the feature: interleaved "$3.8402 today" is what made the old strip unreadable.
func TestRenderSpendBand_AlignsValuesUnderTheirLabels(t *testing.T) {
	lines := renderSpendBand(bandSummary(), 78)
	if len(lines) != spendBandLines {
		t.Fatalf("lines = %d, want %d: %q", len(lines), spendBandLines, lines)
	}
	labels, values := lines[0], lines[1]
	for _, pair := range []struct{ label, value string }{
		{"TODAY", "$3.8402"},
		{"LAST 1H", "$4.5462"},
		{"SAVED", inexactMarker + "$0.2091"},
		{"CACHE HIT", "93%"},
	} {
		li, vi := strings.Index(labels, pair.label), strings.Index(values, pair.value)
		if li < 0 {
			t.Errorf("label %q missing from %q", pair.label, labels)
			continue
		}
		if vi < 0 {
			t.Errorf("value %q missing from %q", pair.value, values)
			continue
		}
		if li != vi {
			t.Errorf("%q starts at column %d but %q starts at %d — the columns do not line up",
				pair.label, li, pair.value, vi)
		}
	}
}

// The saving keeps its marker and never joins the spend figures.
func TestRenderSpendBand_SavingStaysMarkedAndSeparate(t *testing.T) {
	lines := renderSpendBand(bandSummary(), 78)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, inexactMarker+"$0.2091") {
		t.Errorf("the saving lost its %q marker:\n%s", inexactMarker, joined)
	}
	// 3.8402 + 0.2091
	if strings.Contains(joined, "$4.0493") {
		t.Errorf("the saving was added to today's spend:\n%s", joined)
	}
}

// Two lines at every width, and whole figures drop rather than clipping.
func TestRenderSpendBand_HeightIsConstantAndFiguresDropWhole(t *testing.T) {
	for _, w := range []int{20, 30, 40, 60, 78, 120, 200} {
		lines := renderSpendBand(bandSummary(), w)
		if len(lines) != spendBandLines {
			t.Fatalf("width %d: %d lines, want %d", w, len(lines), spendBandLines)
		}
		for i, line := range lines {
			if n := len([]rune(line)); n > w {
				t.Errorf("width %d: line %d is %d runes: %q", w, i, n, line)
			}
		}
		// Whatever survives, TODAY survives: it is the figure the pane exists to show.
		if w >= 20 && !strings.Contains(lines[0], "TODAY") {
			t.Errorf("width %d: TODAY dropped before everything else: %q", w, lines[0])
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
cd authbridge/cmd/abctl && go test ./tui/ -run TestRenderSpendBand -count=1
```

Expected: build failure, `undefined: renderSpendBand`.

- [ ] **Step 3: Implement it**

Create `tui/spend_band.go`:

```go
package tui

import (
	"fmt"
	"strings"
)

// spendBandLines is the band's height: labels, then values. Constant in every state,
// for the same reason numTierRows is — layout() reserves it from the terminal height
// and paneView pads to it.
const spendBandLines = 2

// bandCell is one labelled figure. Label and value travel together because the whole
// point of the band is that they sit in the same column.
type bandCell struct{ label, value string }

// renderSpendBand is the always-on KPI band: labels ABOVE values, column-aligned.
//
// The strip it replaces interleaved the two — "$3.8402 today" — which reads as a CSV
// line and is why the pane looked unreadable. Stacking costs one row and is the single
// change that fixes legibility.
//
// WHOLE CELLS DROP, RIGHT TO LEFT, and nothing is ever clipped: the strip's own rule,
// because a truncated figure is a wrong figure. Today survives longest — it is the
// figure the pane exists to show.
func renderSpendBand(s spendSummary, width int) []string {
	var cells []bandCell
	if s.HasToday {
		cells = append(cells, bandCell{"TODAY", formatUSDCell(s.TodayUSD)})
	}
	if s.HasWindow {
		cells = append(cells, bandCell{
			"LAST " + strings.ToUpper(s.WindowLabel), formatUSDCell(s.WindowUSD)})
	}
	if s.HasSaved {
		// The marker rides on the value, and the value never joins the spend figures:
		// costevent.Event.Avoided forbids any consumer adding it, in either direction.
		cells = append(cells, bandCell{"SAVED", inexactMarker + formatUSDCell(s.SavedUSD)})
	}
	if s.HasCacheHit {
		cells = append(cells, bandCell{"CACHE HIT", fmt.Sprintf("%.0f%%", s.CacheHitPct)})
	}
	if s.Tokens > 0 {
		cells = append(cells, bandCell{"TOKENS", humanizeCount(s.Tokens)})
	}

	const gutter = 2
	fits := func(n int) bool {
		total := 0
		for _, c := range cells[:n] {
			total += cellWidth(c) + gutter
		}
		return total <= width
	}
	keep := len(cells)
	for keep > 0 && !fits(keep) {
		keep--
	}
	cells = cells[:keep]

	var labels, values strings.Builder
	for _, c := range cells {
		w := cellWidth(c) + gutter
		fmt.Fprintf(&labels, "%-*s", w, c.label)
		fmt.Fprintf(&values, "%-*s", w, c.value)
	}
	// TrimRight so a line never exceeds width through trailing padding alone, and pad to
	// spendBandLines unconditionally: an empty band is two blank lines, not zero.
	return []string{
		strings.TrimRight(labels.String(), " "),
		strings.TrimRight(values.String(), " "),
	}
}

// cellWidth is how much room a pair needs: the wider of its two halves, since they
// share a column.
func cellWidth(c bandCell) int {
	if n := len([]rune(c.value)); n > len([]rune(c.label)) {
		return n
	}
	return len([]rune(c.label))
}
```

Style, once the test passes: render the label line through `styleMuted` and leave the value line at default weight, so the hierarchy comes from the palette already in `styles.go` rather than from spacing alone. Do this **after** the alignment test is green — lipgloss escape sequences change `strings.Index` offsets, so the assertions must be written against the unstyled strings and the styling applied at the call site in `paneView`, not inside `renderSpendBand`.

- [ ] **Step 4: Run, commit, and prove it**

```bash
cd authbridge/cmd/abctl && go test ./tui/ -run TestRenderSpendBand -count=1
gofmt -l tui/ && go vet ./tui/
git add tui/spend_band.go tui/spend_band_test.go
git commit -s -m "feat(abctl): render spend as a label-over-value band

Interleaving labels with values — \$3.8402 today — is why the strip read as a CSV line.
Stacking them costs one row and is the change that fixes legibility.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"

# control: break the alignment
perl -0pi -e 's/max\(len\(label\), len\(value\)\)/len(label)/' tui/spend_band.go
grep -c "len(label)) + 2\|len(label)+2" tui/spend_band.go   # must be >= 1
go test ./tui/ -run TestRenderSpendBand_AlignsValues -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- tui/spend_band.go
```

Expected: FAIL with "the columns do not line up".

### Task 9: compose the panel and move the reservation

**The dangerous task.** Read `spend_drawer.go:39-58`, `keys.go:1066` (`layout`) and `app.go`'s `paneView` in full before editing.

**Files:**
- Modify: `tui/spend_drawer.go` — `spendDrawerSeries`, `spendDrawerLines`, `spendDrawerMinHeight`, `renderSpendDrawer`
- Modify: `tui/keys.go:1066` (`layout`) and `tui/app.go` (`paneView`) — **in the same commit**
- Test: `tui/spend_drawer_test.go`

**Interfaces:**
- Consumes: `renderTierRows` (Task 7), `renderSpendBand` (Task 8), the existing `drawerLabels()`, `spendDrawerHost()`, `spendDrawerVisible()`, `spendStripReservesRow()`.
- Produces: a two-column `renderSpendDrawer` whose line count is `spendDrawerLines` in every state.

- [ ] **Step 1: Write the failing tests**

Add to `tui/spend_drawer_test.go`:

```go
// Two columns, headed, with the tier column left and the model column right.
func TestRenderSpendDrawer_ShowsBothColumnsWithHeaders(t *testing.T) {
	lines := renderSpendDrawer(drawerSnap(), usage.GroupModel, "1h", 100)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"WHERE IT WENT", "BY MODEL", "cache-read", "claude-opus-5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the panel is missing %q:\n%s", want, joined)
		}
	}
}

// The figure the band already shows is not repeated here. With one model the old drawer
// restated the window total and the saving verbatim, which is what made it useless.
func TestRenderSpendDrawer_DoesNotRestateTheBandsFigures(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "1h", Group: usage.GroupModel, Priced: true,
		Totals: usage.Counts{Requests: 35, CostMicros: 4_546_200, AvoidedMicros: 209_100},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 35, CostMicros: 4_546_200, AvoidedMicros: 209_100,
				PricedRequests: 35, PriceableRequests: 35, OutputCostMicros: 45000},
		}}},
	}
	joined := strings.Join(renderSpendDrawer(snap, usage.GroupModel, "1h", 100), "\n")
	if strings.Count(joined, "$4.5462") > 1 {
		t.Errorf("the window total appears %d times in the panel:\n%s",
			strings.Count(joined, "$4.5462"), joined)
	}
	if strings.Contains(joined, inexactMarker+"$0.2091") {
		t.Errorf("the panel restates the band's saving:\n%s", joined)
	}
}

// The tree glyph is gone: it implied a parent row that does not exist.
func TestRenderSpendDrawer_HasNoOrphanTreeGlyph(t *testing.T) {
	joined := strings.Join(renderSpendDrawer(drawerSnap(), usage.GroupModel, "1h", 100), "\n")
	for _, glyph := range []string{"└", "├"} {
		if strings.Contains(joined, glyph) {
			t.Errorf("panel still draws %q, which implies a parent row:\n%s", glyph, joined)
		}
	}
}

// Too narrow for two columns and the TIER column yields — the addition gives way to the
// existing contract, so the panel degrades to what shipped before this feature.
func TestRenderSpendDrawer_NarrowDropsTheTierColumnNotTheModels(t *testing.T) {
	joined := strings.Join(renderSpendDrawer(drawerSnap(), usage.GroupModel, "1h", 44), "\n")
	if !strings.Contains(joined, "claude-opus-5") {
		t.Errorf("the model column dropped at 44 columns:\n%s", joined)
	}
	if strings.Contains(joined, "cache-read") {
		t.Errorf("both columns drawn at 44 columns:\n%s", joined)
	}
}
```

Extend the existing `TestRenderSpendDrawer_LineCountMatchesWhatTheRendererEmits` table (or add a case) so it varies **coverage** as well as width: a snapshot with a full mix, one with no mix, and `nil`. The line count must be `spendDrawerLines` in all of them.

- [ ] **Step 2: Run and watch them fail**

```bash
cd authbridge/cmd/abctl && go test ./tui/ -run TestRenderSpendDrawer -count=1
```

- [ ] **Step 3: Change the constants**

```go
	// spendDrawerLines is the panel's height: one header row plus the taller of the two
	// columns. Both columns are four rows — numTierRows on the left, three ranked series
	// plus an (other) band on the right — so the header plus four is the whole panel, and
	// the key hints ride on the model column's last line rather than taking a row.
	spendDrawerLines = 1 + numTierRows
```

Raise `spendDrawerMinHeight` by the band's extra row. Keep `spendDrawerSeries = 3`.

- [ ] **Step 4: Compose the two columns**

Add to `spend_drawer.go`:

```go
// spendDrawerTwoColumnMin is the width below which the panel shows one column.
//
// The TIER column is the one that yields, so the panel degrades to exactly the
// per-model drawer that shipped before this feature: the addition gives way to the
// existing contract, never the reverse.
const spendDrawerTwoColumnMin = 72

// tierColumnWidth is the left column's share. Fixed rather than proportional so the
// model labels to its right do not reflow every time a tier figure changes width.
const tierColumnWidth = 34

// composePanel joins the tier column and the series column row by row.
//
// EXACTLY spendDrawerLines LINES, always: header plus the taller column, padded. The
// reservation is computed from the terminal height in layout() and cannot consult this
// function, so a data-dependent height here is an overflow or an under-fill there.
func composePanel(left, right []string, window string, axis usage.Group, width int) []string {
	twoCol := width >= spendDrawerTwoColumnMin && len(left) > 0
	out := make([]string, 0, spendDrawerLines)
	if twoCol {
		out = append(out, fmt.Sprintf("  %-*s%s",
			tierColumnWidth, "WHERE IT WENT · "+window, "BY MODEL · "+window))
	} else {
		out = append(out, fmt.Sprintf("  BY MODEL · %s", window))
	}
	rows := len(right)
	if twoCol && len(left) > rows {
		rows = len(left)
	}
	for i := 0; i < rows; i++ {
		var l, r string
		if twoCol && i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if twoCol {
			out = append(out, fmt.Sprintf("  %-*s%s", tierColumnWidth, l, r))
		} else {
			out = append(out, "  "+r)
		}
	}
	for len(out) < spendDrawerLines {
		out = append(out, "")
	}
	return out[:spendDrawerLines]
}
```

In `renderSpendDrawer`, replace the body that emitted `├`/`└`-prefixed rows with:

```go
	left := renderTierRows(snap.Totals, tierColumnWidth)
	right := seriesRows(snap, spendDrawerSeries, width) // the existing ranked rows,
	// with the tree glyphs REMOVED: they implied a parent row that does not exist, and
	// the column header above them is what names the grouping now.
	return composePanel(left, right, window, axis, width)
```

The key hints move onto the model column's **last** row rather than taking a line of their own — append them to `right`'s final entry — so the panel's height stays `1 + numTierRows`.

- [ ] **Step 5: Move the reservation and its padding together**

In `layout()` (`keys.go:1066`), where the strip reserved one row:

```go
	if m.spendStripReservesRow() {
		bodyH -= spendBandLines
	}
	if m.spendDrawerReservesRows() {
		bodyH -= spendDrawerLines
	}
```

In `paneView` (`app.go`), fill **both** reservations — this is the shape already there from the under-fill fix, widened from one strip row to `spendBandLines`:

```go
	if m.spendStripReservesRow() {
		band := make([]string, spendBandLines)
		if m.spendStripVisible() {
			band = renderSpendBand(m.spendSummary(), m.width)
		}
		for len(band) < spendBandLines {
			band = append(band, "")
		}
		rows = append(rows, styleMuted.Render(band[0]), band[1])
		if m.spendDrawerReservesRows() {
			var lines []string
			if m.spendDrawerVisible() {
				axis, window := m.drawerLabels()
				lines = renderSpendDrawer(m.spend.snap, axis, window, m.width)
			}
			for len(lines) < spendDrawerLines {
				lines = append(lines, "")
			}
			for _, line := range lines {
				rows = append(rows, styleMuted.Render(line))
			}
		}
	}
```

Note `band[0]` is styled muted and `band[1]` is not: that is the label-versus-value hierarchy from Task 8, applied at the call site so the renderer's own output stays assertable.

**These two edits go in one commit.** Splitting them is what produced the six-row under-fill: `layout()` runs only from the `WindowSizeMsg` handler, so a reservation that the view does not fill leaves the footer floating, and a view that fills more than the reservation overflows the terminal.

- [ ] **Step 6: Run the whole suite**

```bash
cd authbridge/cmd/abctl && go test ./... -count=1
```

Expected: PASS. `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` fails on machines without `~/.cortex/ca/bundle.crt` and is unrelated to this work — confirm it also fails at `main` before dismissing it.

- [ ] **Step 7: Commit and prove it**

```bash
cd authbridge/cmd/abctl && gofmt -l tui/ && go vet ./tui/
git add tui/spend_drawer.go tui/keys.go tui/app.go tui/spend_drawer_test.go
git commit -s -m "feat(abctl): compose the spend panel as two headed columns

With one model in the window the old drawer restated the strip's own window total and
saving verbatim and added nothing; a tier column says something in that case, which is
the common one on a single-model deployment.

The reservation and its padding move together, deliberately: splitting them is what
overflowed this pane by five rows and under-filled it by six.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"

# control: the reservation and the render must agree
perl -0pi -e 's/spendDrawerLines = 1 \+ numTierRows/spendDrawerLines = numTierRows/' tui/spend_drawer.go
grep -c "spendDrawerLines = numTierRows" tui/spend_drawer.go   # must print 1
go test ./tui/ -run 'TestPaneView_FitsTheTerminal|TestRenderSpendDrawer_LineCount' -count=1 2>&1 | grep -E '^--- FAIL|build failed'
git checkout -- tui/spend_drawer.go
```

Expected: FAIL on a line-count or slack mismatch. A green result here means the height is not actually pinned and the overflow can come back.

### Task 10: the table fixes

**Files:**
- Modify: `tui/sessions_pane.go` — `sessionsColumns`, the row builder in `rebuildSessionsTable`
- Test: `tui/sessions_money_test.go`

**Interfaces:** consumes the existing `sessionsShowMoney`, `sessionsColumnWidth`, `sessionMoneyCell`, `sessionMoneyCellMin`, `emptyCell`.

- [ ] **Step 1: Write the failing tests**

```go
// Numerics are right-aligned, so digits line up and the column can be read as a column.
func TestSessionsRows_RightAlignNumericCells(t *testing.T) {
	m := &model{width: 200}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{
		{ID: "aaa", UpdatedAt: time.Now(), EventCount: 5, TotalTokens: 1_000, CostMicros: 1_000_000},
		{ID: "bbb", UpdatedAt: time.Now(), EventCount: 105, TotalTokens: 7_000_000, CostMicros: 5_834_400},
	}
	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	cols := m.sessionsTbl.Columns()
	for ci, col := range cols {
		switch col.Title {
		case "EVENTS", "TOKENS", "COST", "SAVED":
			for ri, row := range rows {
				if cell := row[ci]; cell != "" && strings.HasSuffix(cell, " ") {
					t.Errorf("row %d %s cell %q is left-aligned; numerics right-align",
						ri, col.Title, cell)
				}
			}
		}
	}
}

// A 36-character UUID is truncated: it is the least readable thing on the row and was
// taking 42 of the terminal's columns.
func TestSessionsRows_TruncateTheSessionID(t *testing.T) {
	m := &model{width: 100}
	m.sessionsTbl = newSessionsTable()
	full := "ecb7387f-bffd-4172-adc4-8da23e992e9a"
	m.sessions = []session.SessionSummary{{ID: full, UpdatedAt: time.Now(), EventCount: 1}}
	m.rebuildSessionsTable()
	cell := m.sessionsTbl.Rows()[0][0]
	if cell == full {
		t.Errorf("the full UUID is in the cell: %q", cell)
	}
	if !strings.HasPrefix(cell, "ecb7387f") {
		t.Errorf("cell %q lost the identifying prefix", cell)
	}
}
```

- [ ] **Step 2: Run, implement, run, commit**

Right-align by padding each numeric cell to its fitted column width with `fmt.Sprintf("%*s", w, cell)`. Truncate the ID with the existing ellipsis helper the README sample already shows (`ctx-abc-1234…`). Add the scope note — `lifetime totals` — to the SESSIONS region so a session's lifetime `$5.8344` beside the band's `$3.8402` today no longer reads as a bug.

```bash
cd authbridge/cmd/abctl && go test ./tui/ -count=1
gofmt -l tui/ && go vet ./tui/
git add tui/sessions_pane.go tui/sessions_money_test.go
git commit -s -m "fix(abctl): right-align numerics and truncate the session id

Left-aligned numbers are the main reason the table read as ragged, and a 36-character
UUID was taking 42 columns to say less than its first eight do.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

- [ ] **Step 3: Final gate before opening the PRs**

```bash
cd authbridge/authlib && go test ./... -count=1
cd ../cmd/abctl && go test ./... -count=1
golangci-lint run --new-from-rev upstream/main ./...
git log --format='%an <%ae>%n%s' -1   # verify the identity links to a real account
```

Open the three PRs against `main`, each based on `main` so GitHub shows cumulative diffs, and note the stack order in each description. Footer: `Assisted-By: Claude Code`.

---

## Notes for the executor

- **The plan's test bodies are the specification; the prose around them is context.** Where a step says `IMPLEMENTER:`, the surrounding fixture must come from this package's existing helpers — grep for the nearest similar test and follow it. Do not invent a fixture shape.
- **Three mutation controls in this plan have already caught a real defect class on this surface**: a renderer pinned while its call site was not, a helper pinned while `paneView` was not, and a flag pinned while the pane was not. When a control passes unexpectedly, suspect the test is one layer away from the code, or that the test's own setup disabled the path — `m.filtering = true` once silently skipped an entire key handler and made two assertions unreachable.
- **`usage.Counts` has two reflection guards that fail by design** when a field is added and not wired. Treat their red as the instruction it is.
