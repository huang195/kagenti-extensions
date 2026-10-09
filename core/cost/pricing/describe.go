package pricing

import (
	"sort"
	"strings"
	"time"
)

// This file exists because a config file cannot answer the question operators
// actually have.
//
// `pricing:` shows what the operator WROTE. The figures a request is charged come from
// that plus a rate table compiled into the binary plus any shipped gateway discount —
// so reading the config tells you nothing about the two thirds you did not write. And
// storing the table in the config instead would freeze every install at the rates
// current on its install date, silently, because the config is written once and never
// refreshed: exactly the staleness this package was built to remove.
//
// So the table is not stored, it is made inspectable.

// RowView is one rate-table row, in the unit providers publish.
//
// A zero rate field means UNSET, not free: config leaves a zero tier unset (and rejects
// an entry whose every rate is zero), and the generator drops it. Rendered with omitempty so an absent tier reads as absent rather than as a price of
// nothing. The one row whose rates really are zero, a free model shipped at zero (see
// bundledFreeRates), says so in Free instead.
type RowView struct {
	Host       string `json:"host,omitempty"`
	Model      string `json:"model"`
	Provenance string `json:"provenance"`
	// Free marks a row that prices every tier it sets at zero. Its rate fields are omitted
	// like an unset tier's, so this is what tells the two apart.
	Free bool `json:"free,omitempty"`
	// Unit is the currency these rates are denominated in. OMITTED WHEN USD, so a
	// single-currency deployment's document is unchanged and a reader who has never configured a
	// unit is not shown a column of "USD".
	//
	// It is here because `agentop pricing` printed "$/Mtok" as a literal sub-header — an assertion
	// rather than a rendering — and once an endpoint can bill in credits that literal becomes
	// false. A rate quoted in the wrong currency is the silent-wrong-number failure this package
	// exists to remove, arriving through the tool built to inspect it.
	Unit string `json:"unit,omitempty"`

	InputPerMillion      float64 `json:"inputPerMillion,omitempty"`
	CacheWritePerMillion float64 `json:"cacheWritePerMillion,omitempty"`
	CacheReadPerMillion  float64 `json:"cacheReadPerMillion,omitempty"`
	OutputPerMillion     float64 `json:"outputPerMillion,omitempty"`

	Thresholds []ThresholdView `json:"thresholds,omitempty"`
}

// ThresholdView is one long-context override.
type ThresholdView struct {
	AbovePromptTokens    int     `json:"abovePromptTokens"`
	InputPerMillion      float64 `json:"inputPerMillion,omitempty"`
	CacheWritePerMillion float64 `json:"cacheWritePerMillion,omitempty"`
	CacheReadPerMillion  float64 `json:"cacheReadPerMillion,omitempty"`
	OutputPerMillion     float64 `json:"outputPerMillion,omitempty"`
}

// MultiplierView is one endpoint discount rule.
type MultiplierView struct {
	Host       string  `json:"host,omitempty"`
	Factor     float64 `json:"factor"`
	Provenance string  `json:"provenance"`
}

// Description is the whole table, unresolved.
type Description struct {
	// UpstreamCommit is the LiteLLM commit the bundled rates were generated from, so a
	// figure can be traced to its source without reading the binary.
	UpstreamCommit string           `json:"upstreamCommit,omitempty"`
	ListFetchedAt  time.Time        `json:"listFetchedAt,omitzero"`
	Rows           []RowView        `json:"rows"`
	Multipliers    []MultiplierView `json:"multipliers,omitempty"`
}

// EffectiveRates is what one model actually costs at one endpoint, multiplier included.
type EffectiveRates struct {
	Model      string `json:"model"`
	Provenance string `json:"provenance"`
	Unpriced   bool   `json:"unpriced,omitempty"`
	// Free marks a model priced at zero; see RowView.Free.
	Free bool `json:"free,omitempty"`
	// Unit is the currency these rates are denominated in; omitted when USD. See RowView.Unit.
	Unit string `json:"unit,omitempty"`

	// LongContextAbove is the prompt-token count past which DIFFERENT rates apply, or
	// 0 when this model has none.
	//
	// The rates below are the below-threshold ones, because that is what a request of
	// unknown size is charged and EffectiveFor resolves at prompt size 0. Without this
	// field the view quotes a figure a long-context request will not be charged — the
	// same silent-wrong-number failure this package exists to remove, reintroduced by
	// the tool built to inspect it. The bundled table ships four such models.
	LongContextAbove int `json:"longContextAbove,omitempty"`

	// Above* are the rates once LongContextAbove is exceeded, resolved the same way and
	// with the same multiplier applied. Present only when LongContextAbove is.
	//
	// Naming the breakpoint without its prices told an operator that their long sessions
	// cost something other than the figures above, and left them to find out what by
	// reading the raw table and applying the discount by hand — which is the arithmetic
	// this endpoint exists to do for them.
	AboveInputPerMillion      float64 `json:"aboveInputPerMillion,omitempty"`
	AboveCacheWritePerMillion float64 `json:"aboveCacheWritePerMillion,omitempty"`
	AboveCacheReadPerMillion  float64 `json:"aboveCacheReadPerMillion,omitempty"`
	AboveOutputPerMillion     float64 `json:"aboveOutputPerMillion,omitempty"`

	InputPerMillion      float64 `json:"inputPerMillion,omitempty"`
	CacheWritePerMillion float64 `json:"cacheWritePerMillion,omitempty"`
	CacheReadPerMillion  float64 `json:"cacheReadPerMillion,omitempty"`
	OutputPerMillion     float64 `json:"outputPerMillion,omitempty"`
}

// Effective is the resolved answer for one endpoint.
type Effective struct {
	Host string `json:"host"`
	// Multiplier and MultiplierFrom are surfaced separately from the scaled rates so an
	// operator can see WHY their figures differ from vendor list, rather than being
	// handed numbers that match no published price list.
	Multiplier     float64          `json:"multiplier"`
	MultiplierFrom string           `json:"multiplierFrom,omitempty"`
	Models         []EffectiveRates `json:"models"`
}

func perM(r Rates, t Tier) float64 {
	v, ok := r.For(t)
	if !ok {
		return 0
	}
	return v * tokensPerMillion
}

// Describe renders the raw table: every row as configured or shipped, unscaled.
func (t *Table) Describe() Description {
	out := Description{UpstreamCommit: BundledUpstreamCommit}
	if t == nil {
		return out
	}
	out.ListFetchedAt = t.listFetchedAt
	for i := range t.rows {
		r := &t.rows[i]
		row := RowView{
			Host:       r.host,
			Model:      r.model.pattern,
			Provenance: r.prov.String(),
			Free:       r.rates.free(),
			// Omitted for the default so the common document is byte-identical to before.
			Unit:                 nonDefaultCurrency(r.currency),
			InputPerMillion:      perM(r.rates, TierInput),
			CacheWritePerMillion: perM(r.rates, TierCacheWrite),
			CacheReadPerMillion:  perM(r.rates, TierCacheRead),
			OutputPerMillion:     perM(r.rates, TierOutput),
		}
		for _, th := range r.rates.Thresholds {
			tv := ThresholdView{AbovePromptTokens: th.AbovePromptTokens}
			for tier, dst := range map[Tier]*float64{
				TierInput:      &tv.InputPerMillion,
				TierCacheWrite: &tv.CacheWritePerMillion,
				TierCacheRead:  &tv.CacheReadPerMillion,
				TierOutput:     &tv.OutputPerMillion,
			} {
				if th.Set[tier] {
					*dst = th.Rate[tier] * tokensPerMillion
				}
			}
			row.Thresholds = append(row.Thresholds, tv)
		}
		out.Rows = append(out.Rows, row)
	}
	for i := range t.mults {
		m := &t.mults[i]
		out.Multipliers = append(out.Multipliers, MultiplierView{
			Host: m.host, Factor: m.factor, Provenance: m.prov.String(),
		})
	}
	return out
}

// EffectiveFor resolves every model the table names, for one endpoint — except shipped
// rows scoped to a different endpoint, which have nothing to say about this one.
//
// This is the question worth answering: not "what rows exist" but "what will this
// gateway charge me". Resolution applies host scoping, specificity, provenance
// precedence and the multiplier — none of which a reader can carry out by eye.
//
// Glob rows are resolved under their own pattern, which is the honest rendering: a
// pattern's rate is what any model matching it gets, and inventing a concrete model
// name to stand for it would imply a row that does not exist.
func (t *Table) EffectiveFor(host string) Effective {
	out := Effective{Host: host, Multiplier: 1}
	if t == nil {
		return out
	}
	f, fprov := t.multiplierFor(host)
	out.Multiplier = f
	if fprov != ProvNone {
		out.MultiplierFrom = fprov.String()
	}

	seen := map[string]struct{}{}
	var models []string
	for i := range t.rows {
		r := &t.rows[i]
		// A SHIPPED row scoped to another endpoint is that endpoint's business: OpenCode
		// Zen's free models have nothing to say about api.anthropic.com, and listing them
		// there as UNPRICED reads as a gap in that endpoint's rates. A configured row for
		// another host is still listed, so an operator can see what one gateway has that
		// this one lacks.
		if r.prov == ProvBundled && !matchHost(r.host, host) {
			continue
		}
		m := r.model.pattern
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		models = append(models, m)
	}
	sort.Strings(models)

	for _, m := range models {
		rates, prov := t.Resolve(host, m, 0)
		e := EffectiveRates{
			Model: m, Provenance: prov.String(), Unpriced: prov == ProvNone,
			Unit: nonDefaultCurrency(t.CurrencyFor(host, m)),
		}
		// Read from the UNFLATTENED row: Resolve has already folded the applicable
		// threshold into Base, so the thresholds are only visible on the row itself.
		if lo := t.lowestThresholdFor(host, m); lo > 0 {
			e.LongContextAbove = lo
			// lo+1 because a threshold applies to prompts that EXCEED it — a request of
			// exactly lo tokens pays base, per Rates.At.
			above, aprov := t.Resolve(host, m, lo+1)
			if aprov != ProvNone {
				e.AboveInputPerMillion = perM(above, TierInput)
				e.AboveCacheWritePerMillion = perM(above, TierCacheWrite)
				e.AboveCacheReadPerMillion = perM(above, TierCacheRead)
				e.AboveOutputPerMillion = perM(above, TierOutput)
			}
		}
		if prov != ProvNone {
			e.Free = rates.free()
			e.InputPerMillion = perM(rates, TierInput)
			e.CacheWritePerMillion = perM(rates, TierCacheWrite)
			e.CacheReadPerMillion = perM(rates, TierCacheRead)
			e.OutputPerMillion = perM(rates, TierOutput)
		}
		out.Models = append(out.Models, e)
	}
	return out
}

// Describe on the Registry reads the live table. Nil-safe, matching Resolve: a binary
// with no pricing wired reports an empty table rather than crashing a status handler.
func (r *Registry) Describe() Description {
	if r == nil {
		return Description{UpstreamCommit: BundledUpstreamCommit}
	}
	return r.tab.Load().Describe()
}

// EffectiveFor on the Registry reads the live table.
func (r *Registry) EffectiveFor(host string) Effective {
	if r == nil {
		return Effective{Host: host, Multiplier: 1}
	}
	return r.tab.Load().EffectiveFor(host)
}

// lowestThresholdFor reports the smallest long-context breakpoint on the row that would
// win for this endpoint and model, or 0 if it has none.
//
// Selects through Table.bestRow — the same ranking Resolve uses, not a copy of it — then
// reads the thresholds off the row, which is the only place they survive: Resolve
// flattens with At(promptTotal) before returning, so by then the applicable one has been
// folded into Base and the rest are gone.
func (t *Table) lowestThresholdFor(endpoint, model string) int {
	best := t.bestRow(endpoint, model)
	if best == nil {
		return 0
	}
	lowest := 0
	for _, th := range best.rates.Thresholds {
		if th.AbovePromptTokens > 0 && (lowest == 0 || th.AbovePromptTokens < lowest) {
			lowest = th.AbovePromptTokens
		}
	}
	return lowest
}

// nonDefaultCurrency is c unless c is the default, in which case "".
//
// OMITTING USD IS THE POINT. Every deployment today is USD-only, and a document that spelled it
// out on every row would change every existing reader's output to say what its absence already
// says — while a client that finds this field EMPTY may safely print "$". A non-empty value is a
// claim that the figures beside it are not dollars.
//
// FOLDED, so the claim is not made by a spelling. Both callers hand this a value
// currencyOrDefault has already canonicalised, which is where the property is really
// established; folding again costs one comparison and makes the predicate right for a caller
// that has not. Getting it wrong is not a wrong figure but a wrong LABEL — "per Mtok" over a
// table of dollars — the same false claim as the one this commit removes, in the other direction.
func nonDefaultCurrency(c string) string {
	if c == "" || strings.EqualFold(c, CurrencyUSD) {
		return ""
	}
	return c
}

// UnitOf is the unit a figure for (endpoint, model) is denominated in, or "" when that unit is
// the default — including when rates is nil, which is the Kubernetes deployment.
//
// THE ONE RULE EVERY PRODUCER OF A UNIT GOES THROUGH. The ledger writer labels a row with it and
// settle labels a request's record with it, and those two are read by different surfaces — the
// ledger by windowed totals, the record by per-request and per-session figures. Were each to
// spell the rule itself, the same charge could reach the spend band as credits and the sessions
// pane as dollars, with neither visibly wrong on its own. Empty for the default for the reason
// nonDefaultCurrency gives.
func UnitOf(rates Resolver, endpoint, model string) string {
	if rates == nil {
		return ""
	}
	return nonDefaultCurrency(rates.CurrencyFor(endpoint, model))
}
