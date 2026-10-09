package tui

import (
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// `/` on the sessions pane matches SESSION, TITLE (harvested or served) and AGENT,
// case-insensitively, on listed and cached-only rows alike (#867) — and lists every row
// whatever it matches (#1339).
func TestSessionsSearch_MatchesSessionTitleAndAgent(t *testing.T) {
	newModel := func() *model {
		m := newTitleModel(t, map[string]SessionMetadata{
			"aaa-111":    {Title: "Refactor the parser"},
			"cached-333": {Title: "an old cached session"},
		})
		m.sessions = []session.SessionSummary{
			{ID: "aaa-111", Agent: "claude-code"},
			{ID: "bbb-222", Title: "served name", Agent: "weather-agent"},
		}
		m.events["cached-333"] = []pipeline.SessionEvent{{Host: "api.example.com"}}
		return m
	}
	all := []string{"aaa-111", "bbb-222", "cached-333"}
	cases := []struct {
		search string
		want   []string
	}{
		{"", []string{}},
		{"bbb", []string{"bbb-222"}},
		{"PARSER", []string{"aaa-111"}},
		{"served", []string{"bbb-222"}},
		{"weather", []string{"bbb-222"}},
		{"Claude", []string{"aaa-111"}},
		{"cached session", []string{"cached-333"}},
		{"nomatch", []string{}},
	}
	for _, tc := range cases {
		m := newModel()
		m.search = map[paneID]string{paneSessions: tc.search}
		m.rebuildSessionsTable()
		if !slices.Equal(m.sessionRowIDs, all) {
			t.Errorf("search %q listed %v, want every session %v", tc.search, m.sessionRowIDs, all)
		}
		got := []string{}
		for _, i := range m.sessionsTbl.Matches() {
			got = append(got, m.sessionRowIDs[i])
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("search %q matched %v, want %v", tc.search, got, tc.want)
		}
	}
}
