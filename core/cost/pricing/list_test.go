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

// listWithOpusFamily is opus55List plus the family glob pricegen emits from its newest Opus.
func listWithOpusFamily() *List {
	l := opus55List()
	glob := l.Entries[0]
	glob.Model = "*claude-*opus-*"
	l.Entries = append(l.Entries, glob)
	return l
}

func TestBuildWithList_AModelTheListDoesNotNameKeepsItsShippedRate(t *testing.T) {
	shipped, err := Build(nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tab, err := BuildWithList(nil, listWithOpusFamily())
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	for _, model := range []string{
		"claude-opus-4-1", // a shipped exact row, which the list's family glob also matches
		"claude-opus-5",
		"claude-sonnet-5", // a shipped exact row nothing in the list matches
		"claude-haiku-9",  // a shipped family glob the list has no counterpart for
	} {
		want, wp := shipped.Resolve("api.anthropic.com", model, 0)
		got, gp := tab.Resolve("api.anthropic.com", model, 0)
		if gp != wp || !near(inputPerMillion(got), inputPerMillion(want)) {
			t.Errorf("%s = %v (%s), want the shipped %v (%s)", model, inputPerMillion(got), gp, inputPerMillion(want), wp)
		}
	}
}

func TestBuildWithList_TheListsRowWinsForAModelBothName(t *testing.T) {
	l := listWithOpusFamily()
	opus5 := l.Entries[0]
	opus5.Model = "Claude-Opus-5" // the table matches model names case-insensitively
	opus5.Rates.Base[TierInput] = 4.5e-06
	l.Entries = append(l.Entries, opus5)
	tab, err := BuildWithList(nil, l)
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	for model, want := range map[string]float64{
		"claude-opus-5": 4.50, // the list's exact row
		"claude-opus-9": 4.00, // the list's family glob
	} {
		if r, p := tab.Resolve("api.anthropic.com", model, 0); p != ProvBundled || !near(inputPerMillion(r), want) {
			t.Errorf("%s = %v (%s), want %v from the list", model, inputPerMillion(r), p, want)
		}
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
	// agentop pricing prints where the rates came from.
	l := opus55List()
	tab, err := BuildWithList(nil, l)
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	d := tab.Describe()
	if d.UpstreamCommit != BundledUpstreamCommit {
		t.Errorf("upstream commit = %q, want %q: the shipped rows the list does not name are still in the table", d.UpstreamCommit, BundledUpstreamCommit)
	}
	if !d.ListFetchedAt.Equal(l.FetchedAt) {
		t.Errorf("fetched at = %v, want %v", d.ListFetchedAt, l.FetchedAt)
	}
}
