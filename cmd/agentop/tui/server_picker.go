package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// serverPicker is S's picker, modal while the model holds one: where agent's new sessions go.
type serverPicker struct {
	agent string
	// entries[0] is the agent's own choice (not routed); the rest are the servers by name.
	entries []serverChoice
	// cursor is the highlighted entry, and current the one in force when the picker opened.
	cursor, current int
}

// serverChoice is one picker entry. name is "" for the agent's own choice.
type serverChoice struct {
	name, host, mapping string
}

// serverSwitch is a write S started, until its result lands.
type serverSwitch struct {
	agent, server string
}

// serverSwitchedMsg is the result of the write S started. statsURL is where it polled the
// proxy's reload, for a flash that has to point there and for the refetch to check it is still
// what is on screen; staying is sessionsStaying's count as ↵ found it.
//
// SETTLED AT ↵, NOT WHEN IT LANDS: the write can take up to edit.LocalPollDeadline, and a reader
// who leaves for another connection meanwhile would otherwise have that connection's sessions
// counted as this machine's.
type serverSwitchedMsg struct {
	agent, server, statsURL string
	staying                 int
	res                     edit.WriteResult
	err                     error
}

// serverKeyRefusal is why S on the row labelled label opens nothing, or "" when it may open.
// Row-specific reasons first, so the message is about what the reader is pointing at.
func (m *model) serverKeyRefusal(label string) string {
	switch {
	case label == "":
		return "routing is chosen per agent: highlight an agent's row, then press S"
	case label == otherAgents:
		return "Other pools every client Cortex does not recognise; route one by name with agentop server use <server> --agent <agent>"
	}
	// The router's own rule, so S cannot write an agent name the proxy would refuse to reload. It
	// is what turns the no-User-Agent row away: its requests belong to no one agent, so it cannot
	// be routed, and its SERVER cell is blank for the same reason (agentServerCell).
	if err := routerconfig.CheckAgent(pipeline.AgentName(label)); err != nil {
		return err.Error()
	}
	if _, _, ok := m.localCortexTarget(); !ok {
		return "S changes this machine's Cortex, and agentop is not attached to it — or its stats server did not answer at startup"
	}
	// THE ONE PLACE A ROUTER THAT CANNOT TAKE A CHOICE IS REFUSED, and serverKeyOffered asks the
	// same routerStatus, through activeRouter, so the footer never offers S where this refuses it.
	// Absent (or off, which /v1/pipeline cannot tell from absent), under on_error: observe — in the
	// words `agentop server` uses — or a config agentop cannot read.
	if _, why := m.routerStatus(); why != "" {
		return why
	}
	if m.serverSwitch != nil {
		return "a server switch for " + m.serverSwitch.agent + " is still in progress"
	}
	return ""
}

// serverKeyOffered reports whether the footer advertises S: on the highlighted row, S would open.
// The same serverKeyRefusal, so the hint and the key cannot disagree — no [S] on All agents,
// Other or a row the router will not route, away from this machine's Cortex, without a router
// that routes, or while a switch is in flight, where S would only say why not. Per row, as the
// footer's [↵] is.
func (m *model) serverKeyOffered() bool {
	label, ok := m.selectedAgentScope()
	return ok && m.serverKeyRefusal(label) == ""
}

// openServerPicker is S on the AGENTS pane: the picker over the pane, its cursor on the value
// the agent has now — or, in its place, the panel saying why there is none (renderServerNotice).
func (m *model) openServerPicker() tea.Cmd {
	label, ok := m.selectedAgentScope()
	if !ok {
		return nil
	}
	if why := m.serverKeyRefusal(label); why != "" {
		m.serverNotice = why
		return nil
	}
	router, _ := m.activeRouter()
	p := &serverPicker{agent: pipeline.AgentName(label), entries: []serverChoice{{}}}
	for _, name := range slices.Sorted(maps.Keys(router.Servers)) {
		s := router.Servers[name]
		p.entries = append(p.entries, serverChoice{name: name, host: servers.Host(s), mapping: servers.Mapping(s)})
		if router.Agents[p.agent] == name {
			p.current = len(p.entries) - 1
		}
	}
	p.cursor = p.current
	m.serverPicker = p
	return nil
}

// serverPickerKey owns the keyboard while the picker is up. ↵ applies the highlighted entry, as
// every agentop picker does; esc, q and ctrl+c close it writing nothing, as X's dialog does.
// Every other key is swallowed, S included: no picker key toggles.
func (m *model) serverPickerKey(msg tea.KeyMsg) tea.Cmd {
	p := m.serverPicker
	switch msg.String() {
	case "up", "k":
		p.cursor = max(p.cursor-1, 0)
	case "down", "j":
		p.cursor = min(p.cursor+1, len(p.entries)-1)
	case "home", "g":
		p.cursor = 0
	case "end", "G":
		p.cursor = len(p.entries) - 1
	case "enter":
		return m.applyServerChoice()
	case "esc", "q", "ctrl+c":
		m.serverPicker = nil
	}
	return nil
}

// applyServerChoice closes the picker and writes the highlighted entry: agents.<agent> set to
// the server, or removed for the agent's own choice. The current value writes nothing.
//
// THE SAME WRITE AND THE SAME CHECK AS `agentop server use` AND `reset`: edit.WritePluginConfig
// with servers.Verify, so the TUI can write nothing the command would refuse. In the background,
// because it waits for the proxy's reload; the footer shows it until serverSwitchedMsg lands.
//
// context.Background(), not m.ctx: the write is bounded by edit.LocalPollDeadline, and cutting
// it off when the reader leaves the connection would report a timeout for a write that landed.
func (m *model) applyServerChoice() tea.Cmd {
	p := m.serverPicker
	m.serverPicker = nil
	choice := p.entries[p.cursor]
	// ASKED AGAIN, NOT CARRIED OVER FROM THE OPENING: the pane's rows refetch the pipeline while
	// the picker is up, so the router may have stopped routing since — and then neither a write
	// nor "already go to" below would be true. First, so nothing shown claims a route.
	if why := m.serverKeyRefusal(p.agent); why != "" {
		m.serverNotice = why
		return nil
	}
	if p.cursor == p.current {
		m.setStickyFlash(alreadyRouted(p.agent, choice.name))
		return nil
	}
	path, statsURL, _ := m.localCortexTarget()
	m.serverSwitch = &serverSwitch{agent: p.agent, server: choice.name}
	agent, server := p.agent, choice.name
	staying := m.sessionsStaying(agent, server)
	return func() tea.Msg {
		res, err := edit.WritePluginConfig(context.Background(), edit.ConfigWrite{
			Path:     path,
			StatsURL: statsURL,
			Changes:  []edit.ConfigChange{servers.AgentChange(agent, server)},
			Verify:   servers.Verify,
		})
		return serverSwitchedMsg{agent: agent, server: server, statsURL: statsURL, staying: staying, res: res, err: err}
	}
}

// applyServerSwitched reports a finished switch in one flash, and refetches the pipeline so the
// SERVER columns show what the proxy now runs. After every outcome, a refusal included: what the
// proxy runs is the one thing a flash about an uncertain write cannot settle, and the column can.
// A fetch of its own even when one is in flight: that one may predate the reload.
//
// STICKY, AS A YANK IS: the footer's whole line until the next key, cut from the left where it
// must be. As a timed flash it sat after thirty columns of connection state and was cut from the
// right, so at 80 columns a refusal lost the proxy's error, a timeout the URL to check and a
// success its count — each the part the reader acts on, and each at the end of its line.
//
// THE REFETCH ONLY WHILE THIS MACHINE'S CORTEX IS STILL ON SCREEN, compared by the stats URL the
// write polled: a reader who has left for a pod meanwhile would have that pod's pipeline fetched
// and painted as the reload's result.
func (m *model) applyServerSwitched(msg serverSwitchedMsg) tea.Cmd {
	m.serverSwitch = nil
	m.setStickyFlash(serverSwitchedText(msg, msg.staying))
	if _, statsURL, ok := m.localCortexTarget(); !ok || statsURL != msg.statsURL {
		return nil
	}
	m.pipelineFetching = true
	return m.loadPipelineCmd()
}

// serverSwitchedText is the flash for a finished switch: what happened to the config file and to
// the routing, in the terms `agentop server use` and `reset` use (runServerWrite). staying is
// sessionsStaying's count, said only when the proxy took the change.
//
// EACH OUTCOME SAYS WHERE THE FILE WAS LEFT, because edit.WritePluginConfig leaves it in three
// different places: put back after a refusal or a proxy that stopped answering, left as written
// after a timeout, and — the one error past the write — left as found when someone changed it
// meanwhile. A flash that said "written" after a put-back would send the reader looking for an
// edit that is not there.
//
// WHAT THE READER ACTS ON GOES LAST, because the footer cuts a line too long for it from the
// left (applyServerSwitched): the proxy's error, the URL to check, the count. The lead-ins are
// kept short so that, at 80 columns, what survives of them still says what happened.
func serverSwitchedText(msg serverSwitchedMsg, staying int) string {
	written := msg.res.Outcome == edit.WriteReloadFailed || msg.res.Outcome == edit.WriteStatusUnreachable
	switch {
	case msg.err != nil && written:
		// The proxy refused or went away, and the file could not be put back. The error says
		// which, and why the file was not restored, with the proxy's reason in it.
		return msg.agent + ": " + msg.err.Error()
	case msg.err != nil:
		// From before the write: nothing was written, and the proxy was not touched.
		return msg.agent + "'s server is unchanged: " + msg.err.Error()
	}
	switch msg.res.Outcome {
	case edit.WriteReloaded:
		return switchedText(msg.agent, msg.server, staying)
	case edit.WriteUnchanged:
		// Not alreadyRouted: the picker opened on what the proxy runs and the choice differed
		// from it, so the FILE is what already holds the choice — a shell's `agentop server use`
		// the last fetch predates, or a hand edit the proxy has not taken. The refetch that
		// follows shows which.
		if msg.server == "" {
			return "The config file already leaves " + msg.agent + " unrouted; nothing was written."
		}
		return "The config file already routes new " + msg.agent + " sessions to " + msg.server + "; nothing was written."
	case edit.WriteReloadFailed:
		return "refused: " + msg.agent + " stays where it was, and the config file was put back: " + msg.res.ReloadError
	case edit.WriteStatusUnreachable:
		// Not a refusal: nothing said no, and whether the proxy took the change before it went
		// away is unknown. So nothing here says where msg.agent's sessions go. Without ReloadError,
		// which here is only the poller's "unreachable" and its hint, both said already: as the
		// tail it would be all that survived at 80 columns, and the put-back would not.
		return "the change to " + msg.agent + " was not confirmed: the proxy stopped answering before it reported the reload, " +
			"so the config file was put back; check the proxy is running and try again"
	case edit.WriteReloadTimedOut:
		return fmt.Sprintf("wrote the change to %s, but the proxy reported no reload within %s; check %s/reload/status",
			msg.agent, edit.LocalPollDeadline, msg.statsURL)
	default:
		// WriteNotRunning: no stats URL to poll. S writes only with one, so this is the
		// command's wording kept for a caller that does not.
		return "wrote the change to " + msg.agent + "; no Cortex answered at its stats address, so it applies when the proxy next starts"
	}
}

// sessionsStaying counts agent's running sessions that a switch to server leaves where they are:
// the ones whose inference traffic last went somewhere other than server's host. For the agent's
// own choice (server ""), every one whose inference last went to any configured server.
//
// RESET'S COUNT IS AN UPPER BOUND, and says only what is true of each session it counts. The
// router acts only on requests a client already addresses to a server's host, so an agent's own
// choice is itself one of the servers — the one its client is pointed at — and its sessions there
// stay where new ones go too. Nothing this side names that server: it is the client's setting,
// and the summary carries where a session's requests went, not where they were addressed. So
// those sessions are counted with the rest. Each counted session does stay where it is; what the
// count cannot promise is that every one of them is somewhere new sessions will not be.
//
// HOW MANY WILL NOT MOVE TO THE NEW CHOICE, NOT HOW MANY MOVE. The router keeps every running
// session where it started — by its pin, or, for one quiet since routing changed, by the server
// its history records — so a switch moves none of them. A session already on server stays put
// too, but it is where new sessions go, so naming it would say nothing; one with no inference
// yet is not anywhere, and its first request goes to the new choice.
//
// RUNNING MEANS RESIDENT: a session the proxy holds in memory has had traffic since it started,
// so the router has pinned it. A row only the archive holds has no pin, and is not counted.
// SessionSummary.Active would not do — it marks the one most recently updated session.
//
// Read with the router as it was: a switch changes agents, not servers, so the hosts are the same.
func (m *model) sessionsStaying(agent, server string) int {
	router, _ := m.activeRouter()
	n := 0
	for _, s := range m.sessions {
		if pipeline.AgentName(s.Agent) != agent || (s.Resident != nil && !*s.Resident) || s.InferenceHost == "" {
			continue
		}
		on, ok := servers.ForHost(router, s.InferenceHost)
		if (server == "" && ok) || (server != "" && on != server) {
			n++
		}
	}
	return n
}

// switchedText is the flash for a switch the proxy reloaded. A count of none says nothing, so it
// is left out rather than spelled as zero.
func switchedText(agent, server string, staying int) string {
	text := "New " + agent + " sessions → " + server + "."
	if server == "" {
		text = "New " + agent + " sessions are no longer routed."
	}
	switch {
	case staying == 1:
		text += " 1 running session stays where it is."
	case staying > 1:
		text += fmt.Sprintf(" %d running sessions stay where they are.", staying)
	}
	return text
}

// alreadyRouted is the flash for a choice that is already in force; `agentop server use` and
// `reset` say the same.
func alreadyRouted(agent, server string) string {
	if server == "" {
		return agent + " is not routed; nothing to change."
	}
	return "New " + agent + " sessions already go to " + server + "."
}

// serverNoticeKey owns the keyboard while S's reason is up, as serverPickerKey does the picker's:
// esc, q, ctrl+c and ↵ close it, and nothing else acts — S included, so pressing it again does
// not open and close the panel by turns.
func (m *model) serverNoticeKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q", "ctrl+c", "enter":
		m.serverNotice = ""
	}
	return nil
}

// closeServerPanels drops S's picker and its reason. Called wherever the AGENTS pane is entered
// and where the connection is left: either is drawn only over that pane, but a message can move
// the pane out from under one (a clear landing sets Sessions), and the next visit must not find a
// picker for a value in force that may since have changed.
func (m *model) closeServerPanels() {
	m.serverPicker = nil
	m.serverNotice = ""
}

// text is the footer's state while the switch is in flight.
func (s *serverSwitch) text() string {
	if s.server == "" {
		return "switching " + s.agent + " to its own choice…"
	}
	return "switching " + s.agent + " → " + s.server + "…"
}

// serverPickerHostMax bounds the picker's host column; a longer host is cut, the name and the
// mapping beside it are what tell the entries apart.
const serverPickerHostMax = 32

// ownChoiceEntry is the picker's first entry.
const ownChoiceEntry = "its own choice (not routed)"

// renderServerNotice draws why S opens nothing, wrapped, as X's dialog wraps its text. A panel and
// not a flash because no footer line holds the longer reasons whole — under on_error: observe it
// is the two sentences `agentop server` prints and the config file's path — and a reason cut
// short is the part of it that says what to do. Returns the panel only; see overlayCenter.
func renderServerNotice(text string, width int) string {
	const wrap = 60
	w := min(wrap, max(width-6, 20))
	body := lipgloss.NewStyle().Width(w).Render(text) + "\n\n" + styleHint.Render("[esc] close")
	box := styleBorder.Padding(0, 2)
	if width > styleBorder.GetHorizontalBorderSize() {
		box = box.MaxWidth(width)
	}
	return box.Render(body)
}

// renderServerPicker draws S's picker. Returns the panel only; see overlayCenter.
func renderServerPicker(p *serverPicker, width int) string {
	nameW, hostW := 0, 0
	for _, e := range p.entries[1:] {
		nameW = max(nameW, lipgloss.Width(e.name))
		hostW = max(hostW, min(lipgloss.Width(e.host), serverPickerHostMax))
	}
	var b strings.Builder
	b.WriteString(styleTitle.Render("New "+p.agent+" sessions use") + "\n\n")
	for i, e := range p.entries {
		marker := "  "
		if i == p.cursor {
			marker = "> "
		}
		line := ownChoiceEntry
		if e.name != "" {
			line = padRight(e.name, nameW) + "   " + styleMuted.Render(padRight(truncRight(e.host, hostW), hostW)) +
				"   " + e.mapping
		}
		b.WriteString(marker + line + "\n")
	}
	b.WriteString("\n" + styleHint.Render("[↵] choose  [esc] cancel"))
	box := styleBorder.Padding(0, 1)
	if width > styleBorder.GetHorizontalBorderSize() {
		box = box.MaxWidth(width)
	}
	return box.Render(b.String())
}
