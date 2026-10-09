package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/pricing/pricelist"
)

// priceListInterval is how often a local install asks whether LiteLLM's price list changed.
// An unchanged list answers 304 with no body, so checking hourly costs nothing; a new model's
// price then reaches the proxy within the hour LiteLLM lists it.
const priceListInterval = time.Hour

// priceListFile is where a local install keeps the last downloaded list, under ~/.cortex.
const priceListFile = "price-list.json"

// livePricing keeps the rate table in step with two inputs that change independently: the
// config, which the reloader commits, and LiteLLM's price list, which the downloader applies.
// Either one rebuilds the table from both, under one lock, so neither can undo the other.
type livePricing struct {
	reg *pricing.Registry

	mu        sync.Mutex
	committed bool // a config has been accepted; until then a list only waits
	cfg       *pricing.Config
	list      *pricing.List // nil until one is downloaded; the shipped table is used meanwhile
}

// build prepares a table for a config the reloader has not accepted yet. It applies nothing,
// so a config that is then refused never prices traffic.
func (l *livePricing) build(cfg *pricing.Config) (*pricing.Table, error) {
	l.mu.Lock()
	list := l.list
	l.mu.Unlock()
	return pricing.BuildWithList(cfg, list)
}

// Swap applies an accepted config. prepared is the table build made for it, and is what
// goes live unless a list arrived since: then it is rebuilt with that list, or the reload
// would put an older one back.
//
// Named Swap because that is what the reload closure must do, and what
// TestEveryBinaryInjectsPricing looks for inside it: a binary whose reload never swaps the
// table keeps its boot-time rates forever.
func (l *livePricing) Swap(cfg *pricing.Config, prepared *pricing.Table) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.committed, l.cfg = true, cfg
	tab := prepared
	if fresh, err := pricing.BuildWithList(cfg, l.list); err == nil {
		tab = fresh
	}
	l.reg.Swap(tab)
}

// setList applies a downloaded list to the accepted config.
func (l *livePricing) setList(list *pricing.List) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.committed {
		l.list = list
		return
	}
	tab, err := pricing.BuildWithList(l.cfg, list)
	if err != nil {
		slog.Warn("pricelist: the downloaded price list does not combine with the config; keeping the current prices",
			"error", err)
		return
	}
	l.list = list
	l.reg.Swap(tab)
}

// runPriceList keeps a local install's list prices current until ctx ends. Not run outside a
// local install: a sidecar makes no outbound call it was not configured to make, and keeps the
// shipped table.
func runPriceList(ctx context.Context, l *livePricing) {
	f := &pricelist.Fetcher{}
	if dir, err := defaultCortexDir(); err == nil {
		f.CacheFile = filepath.Join(dir, priceListFile)
	}
	f.Run(ctx, priceListInterval, l.setList)
}
