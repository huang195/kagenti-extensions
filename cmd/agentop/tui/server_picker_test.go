package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

var (
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
	keyUp    = tea.KeyMsg{Type: tea.KeyUp}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
)

// switchTo drives S on the row labelled label: open, move to the entry named server ("" for
// own choice), ↵, then the write and the pipeline refetch it ends with. It returns the
// footer as it stood while the write was in flight.
func switchTo(t *testing.T, m *model, label, server string) string {
	t.Helper()
	onRow(t, m, label)
	m.handleKey(keyRune('S'))
	if m.serverPicker == nil {
		t.Fatalf("S on %s opened no picker; flash %q", label, m.flash)
	}
	for m.serverPicker.entries[m.serverPicker.cursor].name != server {
		before := m.serverPicker.cursor
		if server == "" {
			m.handleKey(keyUp)
		} else {
			m.handleKey(keyDown)
		}
		if m.serverPicker.cursor == before {
			t.Fatalf("the picker has no entry %q", server)
		}
	}
	cmd := m.handleKey(keyEnter)
	if cmd == nil {
		t.Fatalf("↵ on %q started no write; flash %q", server, m.flash)
	}
	inFlight := m.footerView()
	_, refetch := m.Update(cmd())
	if refetch != nil {
		m.Update(refetch())
	}
	return inFlight
}

// S opens over the pane with the cursor on what the agent has now: its server, or its own
// choice when it is not routed. The entries are own choice, then the servers by name, each
// with its host and mapping.
func TestServerPicker_OpensOnTheCurrentValue(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	onRow(t, m, "claude-code/2.1.270")
	m.handleKey(keyRune('S'))
	p := m.serverPicker
	if p == nil {
		t.Fatalf("no picker; flash %q", m.flash)
	}
	var names []string
	for _, e := range p.entries {
		names = append(names, e.name)
	}
	if strings.Join(names, ",") != ",ete,glm" || p.cursor != 1 {
		t.Fatalf("entries %q, cursor %d; want own choice, ete, glm with the cursor on ete", names, p.cursor)
	}
	view := m.View()
	for _, want := range []string{"New claude-code sessions use", ownChoiceEntry, "ete.example.com", "glm.example.com:8443",
		"uses Claude Code's names", "[↵] choose", "[esc] cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("the picker does not show %q:\n%s", want, view)
		}
	}
	m.serverPicker = nil
	onRow(t, m, "opencode/1.0.3")
	m.handleKey(keyRune('S'))
	if m.serverPicker == nil || m.serverPicker.cursor != 0 {
		t.Fatalf("opencode's picker = %+v, want the cursor on its own choice", m.serverPicker)
	}
}

// ↵ on a server routes the agent's new sessions there, through the same write `agentop server
// use` makes, and the column then shows what the proxy runs.
func TestServerPicker_EnterOnAServerRoutesTheAgent(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	inFlight := switchTo(t, m, "opencode/1.0.3", "glm")
	if !strings.Contains(inFlight, "[switching opencode → glm…]") {
		t.Errorf("the footer did not show the switch in progress:\n%s", inFlight)
	}
	if !strings.Contains(f.config(t), "            claude-code: ete\n            opencode: glm\n") {
		t.Errorf("config:\n%s", f.config(t))
	}
	if m.flash != "New opencode sessions → glm." || m.serverSwitch != nil {
		t.Errorf("flash %q, switch %+v", m.flash, m.serverSwitch)
	}
	if got, _ := agentsCell(t, m, "opencode/1.0.3", "SERVER"); got != "glm" {
		t.Errorf("SERVER = %q after the reload, want glm", got)
	}
}

// ↵ on its own choice stops routing the agent: the entry goes, and agents: with it when empty.
func TestServerPicker_EnterOnOwnChoiceStopsRoutingTheAgent(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	switchTo(t, m, "claude-code/2.1.270", "")
	if got := f.config(t); strings.Contains(got, "claude-code") || strings.Contains(got, "agents:") {
		t.Errorf("want claude-code's entry and the emptied agents map gone:\n%s", got)
	}
	if m.flash != "New claude-code sessions are no longer routed." {
		t.Errorf("flash %q", m.flash)
	}
	if got, _ := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); got != agentOwnChoice {
		t.Errorf("SERVER = %q after the reload, want %q", got, agentOwnChoice)
	}
}

// esc, and ↵ on the value already in force, write nothing; S inside the picker does nothing,
// since no picker key toggles.
func TestServerPicker_EscTheCurrentValueAndSWriteNothing(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	onRow(t, m, "claude-code/2.1.270")

	m.handleKey(keyRune('S'))
	m.handleKey(keyDown)
	if m.handleKey(keyRune('S')); m.serverPicker == nil || m.serverPicker.cursor != 2 {
		t.Fatalf("S inside the picker changed it: %+v", m.serverPicker)
	}
	if cmd := m.handleKey(keyEsc); cmd != nil || m.serverPicker != nil {
		t.Fatalf("esc: cmd %v, picker %+v", cmd, m.serverPicker)
	}

	m.handleKey(keyRune('S'))
	if cmd := m.handleKey(keyEnter); cmd != nil || m.serverPicker != nil {
		t.Fatalf("↵ on the current value: cmd %v, picker %+v", cmd, m.serverPicker)
	}
	if m.flash != "New claude-code sessions already go to ete." {
		t.Errorf("flash %q", m.flash)
	}
	if f.config(t) != routerYAML || f.polls.Load() != 0 {
		t.Error("the config was written or the reload polled")
	}
}

// S opens nothing where it cannot apply, and says why: on All agents, on Other, without this
// machine's Cortex, and without the router.
func TestServerPicker_RefusesWhereItCannotApply(t *testing.T) {
	for _, tc := range []struct {
		name, row, want string
		setup           func(*model)
	}{
		{"all agents", "", "routing is chosen per agent", nil},
		{"other", otherAgents, "Other pools every client", func(m *model) {
			m.agents = append(m.agents, agentRow{label: otherAgents, Counts: usage.Counts{Requests: 1}})
		}},
		{"not attached locally", "claude-code/2.1.270", "agentop is not attached to it", func(m *model) {
			m.localEndpoint = "http://127.0.0.1:47601"
		}},
		{"no local config", "claude-code/2.1.270", "agentop is not attached to it", func(m *model) { m.localConfigPath = "" }},
		{"no router", "claude-code/2.1.270", "runs no inference-router", func(m *model) {
			m.pipeline = &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{{Name: "inference-parser"}}}
		}},
		// The no-User-Agent row reads "own choice" like any unrouted agent, but the router refuses
		// to route it: its requests belong to no one agent.
		{"the no-User-Agent row", "unknown", `"unknown" cannot be routed`, func(m *model) {
			m.agents = append(m.agents, agentRow{label: "unknown", Counts: usage.Counts{Requests: 1}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := serverModel(t, newFakeProxy(t, false))
			if tc.setup != nil {
				tc.setup(m)
				m.rebuildAgentsTable()
			}
			onRow(t, m, tc.row)
			if cmd := m.handleKey(keyRune('S')); cmd != nil || m.serverPicker != nil {
				t.Fatalf("S opened something: cmd %v, picker %+v", cmd, m.serverPicker)
			}
			if !strings.Contains(m.flash, tc.want) {
				t.Errorf("flash %q, want it to contain %q", m.flash, tc.want)
			}
		})
	}
}

// The success flash says how many of the agent's running sessions stay where they are: its
// resident sessions whose inference last went somewhere other than the new server. An archived
// row, a session with no inference, another agent's and one already on glm are not counted.
func TestServerPicker_TheFlashCountsTheRunningSessionsThatStay(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	archived := false
	m.sessions = []session.SessionSummary{
		{ID: "a", Agent: "claude-code", InferenceHost: "ete.example.com"},
		{ID: "b", Agent: "claude-code", InferenceHost: "ETE.example.com:443"},
		{ID: "c", Agent: "claude-code", InferenceHost: "glm.example.com:8443"},
		{ID: "d", Agent: "claude-code", InferenceHost: "ete.example.com", Resident: &archived},
		{ID: "e", Agent: "claude-code"},
		{ID: "f", Agent: "opencode", InferenceHost: "ete.example.com"},
	}
	switchTo(t, m, "claude-code/2.1.270", "glm")
	if want := "New claude-code sessions → glm. 2 running sessions stay where they are."; m.flash != want {
		t.Errorf("flash %q, want %q", m.flash, want)
	}
}

// A refused reload flashes the proxy's error, and the column stays on what the proxy still runs.
func TestServerPicker_ARefusedReloadFlashesItsErrorAndKeepsTheColumn(t *testing.T) {
	f := newFakeProxy(t, true)
	m := serverModel(t, f)
	switchTo(t, m, "claude-code/2.1.270", "glm")
	if !strings.Contains(m.flash, "keeps claude-code where it was") || !strings.Contains(m.flash, `configure "inference-router": refused`) {
		t.Errorf("flash %q", m.flash)
	}
	// edit.WritePluginConfig put the file back, and the flash says so: a refusal that read as
	// "written" would send the reader looking for an edit that is not there.
	if !strings.Contains(m.flash, "put back") || f.config(t) != routerYAML {
		t.Errorf("flash %q; config:\n%s", m.flash, f.config(t))
	}
	if got, _ := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); got != "ete" {
		t.Errorf("SERVER = %q after a refused reload, want the old ete", got)
	}
}

// The picker opens on the value the proxy runs, which a hand edit not yet reloaded can leave
// behind the file. A choice the file already holds writes nothing, and says it is the file that
// holds it — not that sessions go there — before the refetch shows what the proxy runs.
func TestServerPicker_AChoiceTheFileAlreadyHoldsSaysSo(t *testing.T) {
	f := newFakeProxy(t, false)
	ahead := strings.Replace(routerYAML, "            claude-code: ete\n", "            claude-code: ete\n            opencode: glm\n", 1)
	if err := os.WriteFile(f.path, []byte(ahead), 0o600); err != nil {
		t.Fatal(err)
	}
	m := serverModel(t, f)
	switchTo(t, m, "opencode/1.0.3", "glm")
	if want := "The config file already routes new opencode sessions to glm; nothing was written."; m.flash != want {
		t.Errorf("flash %q, want %q", m.flash, want)
	}
	if got, _ := agentsCell(t, m, "opencode/1.0.3", "SERVER"); got != "glm" {
		t.Errorf("SERVER = %q after the refetch, want glm", got)
	}
}

// Every way a write can end gets a flash that says what happened to the file and to the
// routing, in `agentop server use`'s terms: refused and put back, not confirmed and put back,
// written and unconfirmed, nothing written, or — the one error past the write — not put back.
func TestServerSwitchedText_SaysWhatHappenedToTheFileAndTheRouting(t *testing.T) {
	const stats = "http://127.0.0.1:47603"
	for _, tc := range []struct {
		name    string
		msg     serverSwitchedMsg
		want    []string
		notWant []string
	}{
		{"reloaded", serverSwitchedMsg{agent: "opencode", server: "glm", res: edit.WriteResult{Outcome: edit.WriteReloaded}},
			[]string{"New opencode sessions → glm. 1 running session stays where it is."}, nil},
		{"reloaded, own choice", serverSwitchedMsg{agent: "opencode", res: edit.WriteResult{Outcome: edit.WriteReloaded}},
			[]string{"New opencode sessions are no longer routed. 1 running session stays where it is."}, nil},
		{"refused", serverSwitchedMsg{agent: "opencode", server: "glm",
			res: edit.WriteResult{Outcome: edit.WriteReloadFailed, ReloadError: "boom", RolledBack: true}},
			[]string{"refused", "keeps opencode where it was", "put back", "boom"}, []string{"wrote"}},
		{"unreachable", serverSwitchedMsg{agent: "opencode", server: "glm",
			res: edit.WriteResult{Outcome: edit.WriteStatusUnreachable, ReloadError: "reload status endpoint unreachable", RolledBack: true}},
			[]string{"not confirmed", "stopped answering", "put back", "check the proxy is running"}, []string{"keeps", "refused"}},
		{"timed out", serverSwitchedMsg{agent: "opencode", server: "glm", statsURL: stats, res: edit.WriteResult{Outcome: edit.WriteReloadTimedOut}},
			[]string{"wrote", "no reload within " + edit.LocalPollDeadline.String(), stats + "/reload/status"}, []string{"put back"}},
		{"not running", serverSwitchedMsg{agent: "opencode", server: "glm", res: edit.WriteResult{Outcome: edit.WriteNotRunning}},
			[]string{"wrote", "next starts"}, nil},
		{"refused before the write", serverSwitchedMsg{agent: "opencode", server: "glm",
			err: errors.New("not writing config.yaml: the result would not load")},
			[]string{"opencode's server is unchanged: not writing config.yaml"}, nil},
		{"refused and not put back", serverSwitchedMsg{agent: "opencode", server: "glm",
			res: edit.WriteResult{Outcome: edit.WriteReloadFailed, ReloadError: "boom"},
			err: errors.New("the proxy refused the change (boom), and config.yaml changed while the proxy was reloading it, so it was left as found")},
			[]string{"opencode: the proxy refused the change (boom)", "left as found"}, []string{"unchanged", "put back"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := serverSwitchedText(tc.msg, 1)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q does not say %q", got, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("%q says %q", got, w)
				}
			}
		})
	}
}

// The picker belongs to the connection it was opened on: leaving that connection closes it, as
// it closes the column picker, so the next one does not open with a stale picker owning keys.
func TestServerPicker_ClosesWithItsConnection(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	m.parentCtx = context.Background()
	onRow(t, m, "claude-code/2.1.270")
	m.handleKey(keyRune('S'))
	if m.serverPicker == nil {
		t.Fatalf("no picker; flash %q", m.flash)
	}
	m.backToPodsPane()
	if m.serverPicker != nil {
		t.Errorf("the picker outlived its connection: %+v", m.serverPicker)
	}
}

// The footer advertises [S] only where it opens, and still fits 80 columns with [?] and [q].
func TestAgentsFooter_AdvertisesSWhereItOpensAndFits(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	for _, above := range []bool{false, true} {
		m.agentsAboveSessions = above
		m.parentCtx = context.Background()
		got := fitHintLine(m.helpView(), 80)
		for _, want := range []string{"[S] server", "[?] keys", "[q] quit"} {
			if !strings.Contains(got, want) {
				t.Errorf("above sessions %v: the footer at 80 lost %q: %q", above, want, got)
			}
		}
		if w := lipgloss.Width(got); w > 80 {
			t.Errorf("above sessions %v: the footer is %d columns", above, w)
		}
	}
	m.serverSwitch = &serverSwitch{agent: "opencode", server: "glm"}
	if strings.Contains(m.helpView(), "[S]") {
		t.Errorf("the footer offers S while a switch is in flight: %q", m.helpView())
	}
	m.serverSwitch = nil
	m.localConfigPath = ""
	if strings.Contains(m.helpView(), "[S]") {
		t.Errorf("the footer offers S with no local Cortex: %q", m.helpView())
	}
}

func TestHelpOverlay_ListsS(t *testing.T) {
	body := helpBodyLines(paneAgents, helpWide)
	if !strings.Contains(body, "choose the inference server this agent's new sessions use") {
		t.Errorf("the overlay does not list S:\n%s", body)
	}
}
