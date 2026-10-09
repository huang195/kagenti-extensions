package main

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
)

// opus55 is a downloaded list pricing a model newer than any build's shipped table.
func opus55(input float64) *pricing.List {
	all := [pricing.NumTiers]bool{}
	for i := range all {
		all[i] = true
	}
	var base [pricing.NumTiers]float64
	base[pricing.TierInput] = input / 1e6
	base[pricing.TierOutput] = 5 * input / 1e6
	base[pricing.TierCacheRead] = input / 20 / 1e6
	base[pricing.TierCacheWrite] = 1.25 * input / 1e6
	return &pricing.List{
		Entries:   []pricing.Entry{{Host: "*", Model: "claude-opus-5-5", Prov: pricing.ProvBundled, Rates: pricing.Rates{Base: base, Set: all}}},
		FetchedAt: time.Date(2026, 10, 8, 21, 0, 0, 0, time.UTC),
	}
}

// override prices claude-sonnet-5 on api.anthropic.com, so a test can see the config survive.
func override() *pricing.Config {
	return &pricing.Config{Endpoints: []pricing.EndpointConfig{{
		Hosts:  []string{"api.anthropic.com"},
		Models: map[string]pricing.ModelConfig{"claude-sonnet-5": {TierRates: pricing.TierRates{InputCostPerMillion: 1}}},
	}}}
}

func inputRate(t *testing.T, reg *pricing.Registry, model string) (float64, pricing.Provenance) {
	t.Helper()
	r, p := reg.Resolve("api.anthropic.com", model, 0)
	return r.Base[pricing.TierInput] * 1e6, p
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// commitConfig does what a reload does: prepare a table, then swap it in.
func commitConfig(t *testing.T, l *livePricing, cfg *pricing.Config) {
	t.Helper()
	tab, err := l.build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	l.Swap(cfg, tab)
}

func TestLivePricing_ADownloadedListIsAppliedWithTheConfig(t *testing.T) {
	l := &livePricing{reg: pricing.NewRegistry(nil)}
	commitConfig(t, l, override())
	l.setList(opus55(4))

	if got, p := inputRate(t, l.reg, "claude-opus-5-5"); p != pricing.ProvBundled || !near(got, 4) {
		t.Errorf("claude-opus-5-5 = %v (%s), want 4 from the downloaded list", got, p)
	}
	if got, p := inputRate(t, l.reg, "claude-sonnet-5"); p != pricing.ProvConfigured || !near(got, 1) {
		t.Errorf("claude-sonnet-5 = %v (%s), want the configured 1: a download must not drop the config", got, p)
	}
}

func TestLivePricing_AReloadDoesNotPutBackAnOlderList(t *testing.T) {
	l := &livePricing{reg: pricing.NewRegistry(nil)}
	commitConfig(t, l, nil)
	l.setList(opus55(4))

	// A reload prepares its table, a download lands, then the reload commits.
	prepared, err := l.build(override())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	l.setList(opus55(3))
	l.Swap(override(), prepared)

	if got, _ := inputRate(t, l.reg, "claude-opus-5-5"); !near(got, 3) {
		t.Errorf("claude-opus-5-5 = %v after the reload, want 3: the list that arrived during it", got)
	}
	if _, p := inputRate(t, l.reg, "claude-sonnet-5"); p != pricing.ProvConfigured {
		t.Errorf("claude-sonnet-5 provenance = %s, want configured from the reloaded config", p)
	}
}

func TestLivePricing_AListBeforeTheFirstConfigWaitsForIt(t *testing.T) {
	// Applied on its own, the list would be priced with no config at all — every
	// configured rate missing until the first reload.
	l := &livePricing{reg: pricing.NewRegistry(nil)}
	l.setList(opus55(4))
	if _, p := inputRate(t, l.reg, "claude-opus-5-5"); p != pricing.ProvNone {
		t.Errorf("provenance before any config = %s, want none: nothing applied yet", p)
	}
	commitConfig(t, l, override())
	if got, _ := inputRate(t, l.reg, "claude-opus-5-5"); !near(got, 4) {
		t.Errorf("claude-opus-5-5 = %v after the first config, want 4 from the waiting list", got)
	}
}
