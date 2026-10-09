package pricing

import (
	"strings"
	"time"
)

// List is a set of vendor list prices obtained at runtime: LiteLLM's public price map,
// downloaded rather than compiled in. See core/cost/pricing/pricelist.
//
// It exists because the shipped table is frozen at build time, and a model released after
// the build is priced by its family row — the newest member the build knew of. When that
// member costs more, every request to the new model is overstated with nothing to say so:
// claude-opus-5-5 was charged at claude-opus-5's rates for nine days, 1.68x what the gateway
// billed, although LiteLLM had listed its price a week before the first request.
type List struct {
	// Produced by the same transform as bundled.go, so the two are interchangeable row for row.
	Entries []Entry
	// FetchedAt is when the list was downloaded, reported by Describe.
	FetchedAt time.Time
}

// BuildWithList is Build with list's rows.
//
// Everything else Build ships still applies — the gateway discounts and the free rates are
// hand-maintained and the list carries neither — and so does `bundled: false`, which turns the
// list off with the rest.
//
// A nil list is the shipped table: Build(cfg) is BuildWithList(cfg, nil).
func BuildWithList(cfg *Config, list *List) (*Table, error) {
	var entries []Entry
	var mults []MultiplierRule
	bundled := cfg.BundledEnabled()
	if bundled {
		listed := map[string]bool{}
		if list != nil {
			entries = append(entries, list.Entries...)
			for _, e := range list.Entries {
				listed[rowKey(e)] = true
			}
		}
		for _, e := range Bundled() {
			if !listed[rowKey(e)] {
				entries = append(entries, e)
			}
		}
		// So do the free models' zero rates, kept out of Bundled() because that is the
		// generated table its golden test pins to the LiteLLM snapshot.
		entries = append(entries, bundledFreeRates()...)
		// Shipped gateway discounts travel with the shipped rates: the rates are
		// vendor list, and for the gateways named here list is a third too high.
		// Disabling the bundled table disables both, which is the right pairing —
		// a multiplier on rates you did not ship scales somebody else's numbers.
		mults = append(mults, bundledMultipliers()...)
	}
	if cfg != nil {
		configured, err := cfg.entries()
		if err != nil {
			return nil, err
		}
		entries = append(entries, configured...)
		mults = append(mults, cfg.multipliers()...)
	}
	tab, err := NewTable(entries, mults...)
	if err != nil {
		return nil, err
	}
	if bundled && list != nil {
		tab.listFetchedAt = list.FetchedAt
	}
	return tab, nil
}

func rowKey(e Entry) string {
	return strings.ToLower(e.Host) + "\x00" + strings.ToLower(e.Model)
}
