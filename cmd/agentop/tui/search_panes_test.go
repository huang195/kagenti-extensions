package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/cluster"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

func newPanesModel(t *testing.T) *model {
	t.Helper()
	resetSettingsForTest(t)
	m := newPickerModel(context.Background(), nil, nil)
	m.width, m.height = 160, 40
	m.namespaces = []cluster.AgentNamespace{
		{Name: "team1", Pods: []cluster.Pod{{Name: "weather-agent-7d9", Phase: "Running", Ready: true}}},
		{Name: "team2"},
	}
	m.selectedNamespace = "team1"
	m.pipeline = &apiclient.PipelineView{
		Inbound:  []apiclient.PipelinePlugin{{Name: "jwt-validation", Direction: "inbound", Position: 0}},
		Outbound: []apiclient.PipelinePlugin{{Name: "token-exchange", Direction: "outbound", Position: 0, ReadsBody: true}},
	}
	m.catalog = &apiclient.PluginCatalog{Plugins: []apiclient.PluginCatalogEntry{
		{Name: "alpha", Description: "First plugin"},
		{Name: "beta", Requires: []string{"alpha"}, Description: "Second"},
	}}
	m.agents = []agentRow{{label: "claude-code/2.1"}, {label: "opencode/1.0"}}
	m.layout()
	m.rebuildNamespacesTable()
	m.rebuildPodsTable()
	m.rebuildPipelineTable()
	m.rebuildCatalogTable()
	m.rebuildAgentsTable()
	return m
}

func TestSearch_EveryCellOfEveryOtherTableIsSearchable(t *testing.T) {
	m := newPanesModel(t)
	for _, tc := range []struct {
		pane paneID
		tbl  *table.Model
	}{
		{paneNamespaces, &m.namespacesTbl},
		{panePods, &m.podsTbl},
		{panePipeline, &m.pipelineTbl},
		{paneCatalog, &m.catalogTbl},
		{paneAgents, &m.agentsTbl},
	} {
		t.Run(paneName(tc.pane), func(t *testing.T) {
			assertEveryCellSearchable(t, tc.tbl, func(q string) {
				m.search = map[paneID]string{tc.pane: q}
				m.surface(tc.pane).redraw()
			})
		})
	}
}

// TestSearch_SlashOnThePickers: the pickers handle their keys before anything else did,
// so `/` never reached the search there, and q quit agentop mid-query.
func TestSearch_SlashOnThePickers(t *testing.T) {
	for _, p := range []paneID{paneNamespaces, panePods} {
		m := newPanesModel(t)
		m.pane = p
		typeKeys(m, "/team1q")
		if !m.searching || m.searchInput.Value() != "team1q" {
			t.Fatalf("pane %v: searching=%v value=%q, want the prompt holding team1q", p, m.searching, m.searchInput.Value())
		}
		press(m, tea.KeyBackspace)
		press(m, tea.KeyEnter)
		if got := m.search[p]; got != "team1" {
			t.Errorf("pane %v: committed %q, want team1", p, got)
		}
		if !strings.Contains(stripANSI(m.View()), "[/team1") {
			t.Errorf("pane %v: no search status on screen:\n%s", p, stripANSI(m.View()))
		}
	}
}

// TestSearch_OnAPaneWithNoRows: the catalog before it loads has no table rows at all.
func TestSearch_OnAPaneWithNoRows(t *testing.T) {
	m := newPanesModel(t)
	m.catalog = nil
	m.catalogTbl.SetRows(nil)
	m.pane = paneCatalog
	typeKeys(m, "/x")
	if st := m.searchStatus(paneCatalog); st != "[/x: no match]" {
		t.Errorf("status = %q, want [/x: no match]", st)
	}
	press(m, tea.KeyEnter)
	m.Update(keyRune('n'))
	m.Update(keyRune('N'))
	typeKeys(m, "/y")
	press(m, tea.KeyEsc)
	_ = m.View()
}

// TestSearch_ThePaneChangesUnderTheOpenPrompt: a port-forward finishing moves the picker
// to the sessions pane while the operator is typing. The keys still go to the prompt, and
// the query is committed to the pane it was typed on.
func TestSearch_ThePaneChangesUnderTheOpenPrompt(t *testing.T) {
	m := newPanesModel(t)
	m.pane = panePods
	typeKeys(m, "/wea")
	m.pane = paneSessions
	typeKeys(m, "therq")
	if !m.searching || m.searchInput.Value() != "weatherq" {
		t.Fatalf("searching=%v value=%q, want the prompt holding weatherq", m.searching, m.searchInput.Value())
	}
	press(m, tea.KeyEnter)
	if m.search[panePods] != "weatherq" || m.search[paneSessions] != "" {
		t.Errorf("search = %v, want weatherq on pods only", m.search)
	}
}

func TestSearch_NextAndPreviousOnAnotherTable(t *testing.T) {
	m := newPanesModel(t)
	m.pane = paneCatalog
	typeKeys(m, "/plugin")
	press(m, tea.KeyEnter)
	if c := m.catalogTbl.Cursor(); c != 0 {
		t.Fatalf("cursor = %d, want 0 (alpha's \"First plugin\")", c)
	}
	m.Update(keyRune('n'))
	if c := m.catalogTbl.Cursor(); c != 0 {
		t.Errorf("n with one match moved the cursor to %d", c)
	}
	if got := m.catalogTbl.Matches(); !slices.Equal(got, []int{0}) {
		t.Errorf("Matches = %v, want [0]", got)
	}
}

// TestSearch_SlashOpensOnEveryPane walks every pane, as TestPaneKeysCoverAllPanes does, so a
// pane added later cannot ship without a search.
func TestSearch_SlashOpensOnEveryPane(t *testing.T) {
	m := newPanesModel(t)
	for p := paneID(0); p <= lastPaneID; p++ {
		m.pane = p
		if m.surface(p) == nil {
			t.Errorf("pane %v has no search surface", p)
			continue
		}
		m.Update(keyRune('/'))
		if !m.searching || m.searchPane != p {
			t.Errorf("pane %v: `/` did not open the prompt on it", p)
		}
		press(m, tea.KeyEsc)
	}
	m.pane = paneSessions
	m.Update(keyRune('?'))
	m.Update(keyRune('/'))
	if !m.searching || m.searchPane != targetHelp {
		t.Error("help overlay: `/` did not open the prompt on it")
	}
}

// TestHelp_SearchIsAnAnywhereKey: every pane searches, so the overlay names `/` once, with
// the keys that work everywhere, and no pane names it again.
func TestHelp_SearchIsAnAnywhereKey(t *testing.T) {
	has := func(g keyGroup, key string) bool {
		for _, b := range g.bindings {
			if b.keys == key {
				return true
			}
		}
		return false
	}
	if !has(anywhereKeys, "/") || !has(anywhereKeys, "n / N") {
		t.Error("anywhereKeys does not name / and n / N")
	}
	for p, g := range paneKeys {
		if has(g, "/") || has(g, "n / N") {
			t.Errorf("pane %v names / or n / N itself", p)
		}
	}
}
