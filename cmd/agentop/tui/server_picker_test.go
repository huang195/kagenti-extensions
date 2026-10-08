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
	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
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
		t.Fatalf("S on %s opened no picker; notice %q", label, m.serverNotice)
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
		// The router refuses to route the no-User-Agent row: its requests belong to no one agent.
		// Its SERVER cell is blank for the same reason.
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
			if !strings.Contains(m.serverNotice, tc.want) {
				t.Errorf("notice %q, want it to contain %q", m.serverNotice, tc.want)
			}
			// The reason is the panel, whole, over the pane — never a footer line cut short.
			if view := m.View(); !strings.Contains(view, "[esc] close") {
				t.Errorf("the reason is not shown over the pane:\n%s", view)
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

// A session resumed after a restart that has sent no inference since shows its history's host,
// but no pin holds it there: its next request is decided as a new one's and goes to the new
// choice. So it is not among those that stay — for a switch to a server, or back to own choice.
func TestServerPicker_TheCountLeavesOutASessionNoPinHolds(t *testing.T) {
	for _, server := range []string{"glm", ""} {
		m := serverModel(t, newFakeProxy(t, false))
		m.sessions = []session.SessionSummary{
			{ID: "a", Agent: "claude-code", InferenceHost: "ete.example.com"},
			{ID: "resumed", Agent: "claude-code", InferenceHost: "ete.example.com", InferenceHostFromHistory: true},
		}
		switchTo(t, m, "claude-code/2.1.270", server)
		if !strings.Contains(m.flash, " 1 running session stays where it is.") {
			t.Errorf("switch to %q: flash %q, want the one session a pin holds", server, m.flash)
		}
	}
}

// A refused reload flashes the proxy's error, and the column stays on what the proxy still runs.
func TestServerPicker_ARefusedReloadFlashesItsErrorAndKeepsTheColumn(t *testing.T) {
	f := newFakeProxy(t, true)
	m := serverModel(t, f)
	switchTo(t, m, "claude-code/2.1.270", "glm")
	if !strings.Contains(m.flash, "claude-code stays where it was") || !strings.Contains(m.flash, `configure "inference-router": refused`) {
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
			[]string{"refused", "opencode stays where it was", "put back", "boom"}, []string{"wrote"}},
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

// The footer advertises [S] only on a row where it opens — an agent's, attached to this machine's
// Cortex with a router that routes and no switch in flight — and there still fits 80 columns
// with [?] and [q]. Where S would only explain why not, the hint is not there.
func TestAgentsFooter_AdvertisesSWhereItOpensAndFits(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	m.agents = append(m.agents,
		agentRow{label: otherAgents, Counts: usage.Counts{Requests: 1}},
		agentRow{label: "unknown", Counts: usage.Counts{Requests: 1}})
	m.rebuildAgentsTable()
	onRow(t, m, "claude-code/2.1.270")
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
	for _, row := range []string{"", otherAgents, "unknown"} {
		onRow(t, m, row)
		if strings.Contains(m.helpView(), "[S]") {
			t.Errorf("row %q: the footer offers S, which there only says why not: %q", row, m.helpView())
		}
	}
	onRow(t, m, "claude-code/2.1.270")
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

// squash is s with its whitespace and box-drawing characters removed, so a panel's wrapped text
// can be compared with the string it wraps.
func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(" \n\t│╭╮╰╯─", r) {
			return -1
		}
		return r
	}, s)
}

// The result of a switch is the footer's whole line until the next key, cut from the LEFT where
// it must be, so at 80 columns the part a reader acts on survives: the proxy's error after a
// refusal, the URL to check after a timeout, the count after a success.
func TestServerSwitch_TheResultIsReadableAt80Columns(t *testing.T) {
	refused := serverModel(t, newFakeProxy(t, true))
	refused.width = 80
	switchTo(t, refused, "claude-code/2.1.270", "glm")
	line := strings.SplitN(refused.footerView(), "\n", 2)[0]
	for _, want := range []string{"put back", `configure "inference-router": refused`} {
		if !strings.Contains(line, want) {
			t.Errorf("after a refusal the footer at 80 lost %q: %q", want, line)
		}
	}
	if w := lipgloss.Width(line); w > 80 {
		t.Errorf("the footer's first line is %d columns", w)
	}

	ok := serverModel(t, newFakeProxy(t, false))
	ok.width = 80
	ok.sessions = []session.SessionSummary{
		{ID: "a", Agent: "claude-code", InferenceHost: "ete.example.com"},
		{ID: "b", Agent: "claude-code", InferenceHost: "ete.example.com"},
	}
	switchTo(t, ok, "claude-code/2.1.270", "glm")
	if line := strings.SplitN(ok.footerView(), "\n", 2)[0]; !strings.Contains(line, "New claude-code sessions → glm. 2 running sessions stay where they are.") {
		t.Errorf("after a success the footer at 80 is %q", line)
	}

	timedOut := serverModel(t, newFakeProxy(t, false))
	timedOut.width = 80
	timedOut.applyServerSwitched(serverSwitchedMsg{agent: "claude-code", server: "glm", statsURL: timedOut.localStatsURL,
		res: edit.WriteResult{Outcome: edit.WriteReloadTimedOut}})
	if line := strings.SplitN(timedOut.footerView(), "\n", 2)[0]; !strings.Contains(line, timedOut.localStatsURL+"/reload/status") {
		t.Errorf("after a timeout the footer at 80 lost the URL to check: %q", line)
	}

	// Until the next key, which leaves it to the footer's usual state.
	ok.handleKey(keyRune('j'))
	if strings.Contains(ok.footerView(), "running sessions stay") {
		t.Error("the result outlived the next key")
	}
}

// Why S opens nothing is shown whole, wrapped in a panel over the pane, because no footer line
// holds it: the observe reason is two sentences and a file path. At 80 columns, every word of it.
func TestServerNotice_ShowsTheWholeReasonAt80Columns(t *testing.T) {
	reason := servers.Inactive(pipeline.ErrorPolicyObserve, "/Users/someone/a/rather/long/path/to/.cortex/config.yaml")
	panel := renderServerNotice(reason, 80)
	if !strings.Contains(squash(panel), squash(reason)) {
		t.Errorf("the panel does not carry the whole reason:\n%s", panel)
	}
	for i, l := range strings.Split(panel, "\n") {
		if w := lipgloss.Width(l); w > 80 {
			t.Errorf("line %d is %d columns: %q", i, w, l)
		}
	}

	m := serverModel(t, newFakeProxy(t, false))
	m.width = 80
	m.pipeline = observing(routerPipeline(routerRaw))
	m.rebuildAgentsTable()
	onRow(t, m, "claude-code/2.1.270")
	m.handleKey(keyRune('S'))
	if m.serverNotice == "" {
		t.Fatal("S under on_error: observe showed no reason")
	}
	view := m.View()
	for i, l := range strings.Split(view, "\n") {
		if w := lipgloss.Width(l); w > 80 {
			t.Errorf("view line %d is %d columns", i, w)
		}
	}
	if !strings.Contains(view, "[esc] close") {
		t.Errorf("no panel over the pane:\n%s", view)
	}
}

// The panel is modal, as the picker is: esc, q, ctrl+c and ↵ close it, and nothing else acts —
// S included, so pressing it again does not open and close the panel by turns.
func TestServerNotice_ClosesOnEscQCtrlCAndEnterOnly(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyEsc, keyRune('q'), {Type: tea.KeyCtrlC}, keyEnter} {
		m := serverModel(t, newFakeProxy(t, false))
		onRow(t, m, "")
		m.handleKey(keyRune('S'))
		if m.serverNotice == "" {
			t.Fatal("S on All agents showed no reason")
		}
		if cmd := m.handleKey(keyRune('S')); cmd != nil || m.serverNotice == "" {
			t.Fatalf("S inside the panel acted: cmd %v, notice %q", cmd, m.serverNotice)
		}
		if m.handleKey(keyDown); m.agentsTbl.Cursor() != 0 || m.serverNotice == "" {
			t.Fatalf("↓ reached the pane under the panel")
		}
		if cmd := m.handleKey(key); cmd != nil || m.serverNotice != "" {
			t.Errorf("%s: cmd %v, notice %q", key, cmd, m.serverNotice)
		}
	}
}

// q and ctrl+c close the picker as esc does: nothing written, nothing polled, agentop not quit.
func TestServerPicker_QAndCtrlCCloseWritingNothing(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyRune('q'), {Type: tea.KeyCtrlC}} {
		f := newFakeProxy(t, false)
		m := serverModel(t, f)
		onRow(t, m, "claude-code/2.1.270")
		m.handleKey(keyRune('S'))
		m.handleKey(keyDown)
		if cmd := m.handleKey(key); cmd != nil || m.serverPicker != nil {
			t.Errorf("%s: cmd %v, picker %+v", key, cmd, m.serverPicker)
		}
		if f.config(t) != routerYAML || m.serverSwitch != nil {
			t.Errorf("%s wrote the config or began a switch", key)
		}
	}
}

// S while a switch is still landing says so, rather than starting a second write behind it.
func TestServerPicker_RefusesWhileASwitchIsInFlight(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	m.serverSwitch = &serverSwitch{agent: "opencode", server: "glm"}
	onRow(t, m, "claude-code/2.1.270")
	if cmd := m.handleKey(keyRune('S')); cmd != nil || m.serverPicker != nil {
		t.Fatalf("S opened something: cmd %v, picker %+v", cmd, m.serverPicker)
	}
	if want := "a server switch for opencode is still in progress"; m.serverNotice != want {
		t.Errorf("notice %q, want %q", m.serverNotice, want)
	}
}

// A message can move the pane under the picker — a clear landing after its dialog was hidden
// sets Sessions — and the next visit to the AGENTS pane must not find it there again.
func TestServerPicker_DoesNotComeBackAfterThePaneMovesUnderIt(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	onRow(t, m, "claude-code/2.1.270")
	m.handleKey(keyRune('S'))
	if m.serverPicker == nil {
		t.Fatalf("no picker; notice %q", m.serverNotice)
	}
	m.Update(clearDoneMsg{res: apiclient.ClearResult{}})
	if m.pane == paneAgents {
		t.Fatal("the clear did not move the pane")
	}
	m.enterAgents(paneSessions)
	if m.serverPicker != nil || strings.Contains(m.View(), "sessions use") {
		t.Errorf("the picker came back: %+v", m.serverPicker)
	}
}

// What the result says is settled when ↵ is pressed, against the Cortex it writes. Leaving for
// another connection while the reload is awaited must neither count that connection's sessions
// nor refetch its pipeline as if it were this machine's.
func TestServerSwitch_ALaterConnectionNeitherCountsNorRefetches(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	m.sessions = []session.SessionSummary{
		{ID: "a", Agent: "claude-code", InferenceHost: "ete.example.com"},
		{ID: "b", Agent: "claude-code", InferenceHost: "ete.example.com"},
	}
	onRow(t, m, "claude-code/2.1.270")
	m.handleKey(keyRune('S'))
	m.handleKey(keyDown)
	cmd := m.handleKey(keyEnter)
	if cmd == nil {
		t.Fatalf("↵ started no write; notice %q", m.serverNotice)
	}
	// Another connection by the time the reload lands: a pod, with sessions of its own.
	m.client = apiclient.New("http://127.0.0.1:1")
	m.sessions = []session.SessionSummary{{ID: "p", Agent: "claude-code", InferenceHost: "api.example.com"}}
	if _, refetch := m.Update(cmd()); refetch != nil {
		t.Error("the result refetched another connection's pipeline")
	}
	if want := "New claude-code sessions → glm. 2 running sessions stay where they are."; m.flash != want {
		t.Errorf("flash %q, want %q", m.flash, want)
	}
}
