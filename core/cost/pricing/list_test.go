package pricing

import (
	"math"
	"testing"
	"time"
)

// opus55List is a downloaded list naming one model the shipped table does not: Opus 5.5, which
// costs less than the Opus 5 the shipped table's family row would have charged for it.
func opus55List() *List {
	return &List{
		Entries: []Entry{{Host: "*", Model: "claude-opus-5-5", Prov: ProvBundled, Rates: Rates{
			Base: [numTiers]float64{TierInput: 4e-06, TierCacheWrite: 5e-06, TierCacheRead: 2e-07, TierOutput: 2e-05},
			Set:  [numTiers]bool{TierInput: true, TierCacheWrite: true, TierCacheRead: true, TierOutput: true},
		}}},
		FetchedAt: time.Date(2026, 10, 8, 21, 0, 0, 0, time.UTC),
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestBuildWithList_PricesFromTheList(t *testing.T) {
	tab, err := BuildWithList(nil, opus55List())
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	r, p := tab.Resolve("api.anthropic.com", "claude-opus-5-5", 0)
	if p != ProvBundled {
		t.Fatalf("provenance = %s, want bundled", p)
	}
	if got := inputPerMillion(r); !near(got, 4.00) {
		t.Errorf("input rate = %v, want 4.00 from the list, not the shipped table's Opus family row", got)
	}
}

func TestBuildWithList_ReplacesTheShippedModelRows(t *testing.T) {
	// The list is the newer table, so it stands alone. Merged, the shipped table's family rows
	// would sit beside the list's at the same provenance and price an unlisted model from a
	// build that may be months old.
	tab, err := BuildWithList(nil, opus55List())
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5", 0); p != ProvNone {
		t.Errorf("a model only the shipped table names resolved %s, want none", p)
	}
}

func TestBuildWithList_KeepsTheShippedDiscountAndFreeRates(t *testing.T) {
	tab, err := BuildWithList(nil, opus55List())
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	r, _ := tab.Resolve("ete-litellm.ai-models.vpc-int.res.ibm.com", "claude-opus-5-5", 0)
	if got := inputPerMillion(r); !near(got, 3.04) {
		t.Errorf("gateway input rate = %v, want 3.04 (0.76 x the list's 4.00)", got)
	}
	if _, p := tab.Resolve("opencode.ai", "grok-code-free", 0); p != ProvBundled {
		t.Errorf("Zen free model provenance = %s, want bundled: the list carries no free rates", p)
	}
}

func TestBuildWithList_ConfiguredOutranksTheList(t *testing.T) {
	tab, err := BuildWithList(mustYAML(t, `
endpoints:
  - hosts: [api.anthropic.com]
    models:
      claude-opus-5-5:
        input_cost_per_million: 1.00
`), opus55List())
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	r, p := tab.Resolve("api.anthropic.com", "claude-opus-5-5", 0)
	if p != ProvConfigured || !near(inputPerMillion(r), 1.00) {
		t.Errorf("got %s at %v, want configured at 1.00", p, inputPerMillion(r))
	}
}

func TestBuildWithList_BundledFalseIgnoresTheList(t *testing.T) {
	tab, err := BuildWithList(mustYAML(t, "bundled: false\n"), opus55List())
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5-5", 0); p != ProvNone {
		t.Errorf("provenance = %s, want none: bundled: false turns off every list price", p)
	}
}

func TestBuildWithList_NilListIsTheShippedTable(t *testing.T) {
	tab, err := BuildWithList(nil, nil)
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	if _, p := tab.Resolve("api.anthropic.com", "claude-opus-5", 0); p != ProvBundled {
		t.Errorf("provenance = %s, want bundled from the shipped table", p)
	}
	if d := tab.Describe(); d.UpstreamCommit != BundledUpstreamCommit || !d.ListFetchedAt.IsZero() {
		t.Errorf("describe = commit %q fetched %v, want the shipped commit and no fetch time", d.UpstreamCommit, d.ListFetchedAt)
	}
}

func TestBuildWithList_DescribeNamesTheDownload(t *testing.T) {
	// agentop pricing prints where the rates came from. Naming the shipped commit for a table
	// that no longer holds its rows would send a reader to the wrong source.
	l := opus55List()
	tab, err := BuildWithList(nil, l)
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	d := tab.Describe()
	if d.UpstreamCommit != "" {
		t.Errorf("upstream commit = %q, want empty for a downloaded list", d.UpstreamCommit)
	}
	if !d.ListFetchedAt.Equal(l.FetchedAt) {
		t.Errorf("fetched at = %v, want %v", d.ListFetchedAt, l.FetchedAt)
	}
}
