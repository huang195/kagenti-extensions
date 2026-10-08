package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
)

// serverConfirm asks before `server add` replaces a server. Its own var, as
// claudeCodeConfirm is, so a test stubbing it cannot disarm another command's prompt.
var serverConfirm = confirm

// writePluginConfig is edit.WritePluginConfig. A var so a test can stand in for a
// proxy that stops answering mid-poll, which the real poller takes about 16s of
// backoff to give up on.
var writePluginConfig = edit.WritePluginConfig

// readServerKey reads a server's API key at a prompt that does not echo, on the
// controlling terminal rather than stdin, so it works when stdin is a pipe and the
// prompt is not written into redirected output. A var so tests can stand in for the
// terminal a test process does not have.
var readServerKey = func(name string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal to read the key from; pipe it in with --key-stdin")
	}
	defer tty.Close()
	fmt.Fprintf(tty, "API key for %s: ", name)
	b, err := term.ReadPassword(tty.Fd())
	fmt.Fprintln(tty)
	if err != nil {
		return "", fmt.Errorf("reading the key: %w", err)
	}
	return string(b), nil
}

// stdinIsTerminal reports whether stdin is a terminal. A var so a test can say it
// is: a test's stdin never is.
var stdinIsTerminal = func(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// readKey is the key for server name: from stdin with --key-stdin, else at the
// hidden prompt. Never from an argument, which would put it in shell history.
func readKey(name string, fromStdin bool, stdin io.Reader) (string, error) {
	if !fromStdin {
		return readServerKey(name)
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	if err != nil {
		return "", fmt.Errorf("reading the key from stdin: %w", err)
	}
	key := strings.TrimRight(string(b), "\r\n")
	if strings.ContainsAny(key, "\r\n") {
		return "", errors.New("stdin holds more than one line; --key-stdin reads the key alone")
	}
	return key, nil
}

func serverAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server add", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	keyStdin := flags.Bool("key-stdin", false, "read the API key from stdin instead of a prompt")
	yes := flags.Bool("yes", false, "replace an existing server without asking")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "agentop server add: want a name and a URL: agentop server add <name> <url>")
		return 2
	}
	name, rawURL := pos[0], pos[1]
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 2
	}
	// ParseURL's error, never rawURL: a refused URL may carry a key anywhere in it,
	// and ParseURL's errors quote no part of the URL.
	ep, err := routerconfig.ParseURL(rawURL)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 2
	}
	// Before anything is asked or read: --key-stdin reads stdin as typed, so on a
	// terminal the key would echo, character by character, onto the screen and into
	// any recording of it. The prompt without the flag reads it without echo.
	if *keyStdin && stdinIsTerminal(stdin) {
		fmt.Fprintln(stderr, "agentop server add: --key-stdin reads the key from stdin, and stdin is a terminal, "+
			"so the key would echo as you typed it. Pipe the key in, or drop --key-stdin to be asked for it without echo.")
		return 1
	}

	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	// servers.ForHost, the rule agentop then names a session's server by, so this refusal and
	// what it protects cannot drift apart. A server replaced under its own name keeps its host.
	if other, ok := servers.ForHost(c, ep.Host); ok && other != name {
		fmt.Fprintf(stderr, "agentop server add: %s is already on %s. Each server needs a host of its own, "+
			"because agentop names a session's server from the host its requests went to.\n", other, ep.Hostname)
		return 1
	}
	old, replacing := c.Servers[name]
	if replacing {
		fmt.Fprintf(stdout, "%s is already configured (%s). Replacing it gives it this URL and key, "+
			"and the sessions on it use them from their next request.\n", name, servers.Host(old))
		if !*yes && !serverConfirm(stdout) {
			return exitDeclined
		}
	}

	key, err := readKey(name, *keyStdin, stdin)
	if err == nil {
		err = routerconfig.CheckKey(key)
	}
	if err == nil && strings.Contains(key, "$") {
		// config.Load expands $NAME and ${NAME} across the whole file, so a literal $
		// in a key would reach the server as something else.
		err = errors.New("the key contains $, which Cortex reads as an environment variable when it loads the config; " +
			"put the key in an environment variable the proxy has, and write key: ${NAME} by hand")
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	if ep.PlaintextRemote() {
		fmt.Fprintf(stderr, "agentop server add: warning: %s is plain http on another machine, so requests routed there "+
			"cross the network decrypted, key and prompts included.\n", ep.URL())
	}

	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"servers", name},
		Value: edit.MapValue("url", ep.URL(), "key", key), CreatePlugin: true}
	done := "Added " + name + "."
	if replacing {
		done = "Replaced " + name + "."
	}
	if len(agentsOn(c, name)) == 0 {
		done += " No agent uses it yet: press S on agentop's agents pane, or run\n  agentop server use " + name + " --agent claude-code"
	}
	return runServerWrite(stdout, stderr, path, statsURL, ch, writeReport{
		done:      done,
		unchanged: name + " already has that URL and key; nothing to change.",
		note:      routerInactive(cfg, path),
	})
}

func serverRemove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server remove", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "agentop server remove: want a server name: agentop server remove <name>")
		return 2
	}
	name := pos[0]
	// First, as add does: every message below echoes name, and a name CheckName
	// accepts carries nothing a terminal would act on. Its own refusal quotes it.
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 2
	}
	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 1
	}
	if _, ok := c.Servers[name]; !ok {
		fmt.Fprintf(stderr, "agentop server remove: no server named %s%s\n", name, configuredServers(c))
		return 1
	}
	agents := agentsOn(c, name)
	// The only server first, since no command removes it. With agents routed to it
	// the refusal names the step that makes it carry no traffic too, rather than
	// naming that step alone and refusing again once it is taken.
	if len(c.Servers) == 1 {
		fmt.Fprintf(stderr, "agentop server remove: %s is the only server, and %s needs one. ", name, routerName)
		if len(agents) > 0 {
			fmt.Fprintf(stderr, "It is %s's server for new sessions; stop routing to it with\n", strings.Join(agents, " and "))
			for _, a := range agents {
				fmt.Fprintf(stderr, "  agentop server reset --agent %s\n", a)
			}
			fmt.Fprint(stderr, "and it changes no traffic. ")
		} else {
			fmt.Fprint(stderr, "With no agent routed to it it changes no traffic. ")
		}
		fmt.Fprintf(stderr, "To take the router out altogether, delete its entry from %s.\n", homeTilde(path))
		return 1
	}
	if len(agents) > 0 {
		var other string
		for _, s := range slices.Sorted(maps.Keys(c.Servers)) {
			if s != name {
				other = s
				break
			}
		}
		fmt.Fprintf(stderr, "%s is %s's server for new sessions. Pick another first:\n", name, strings.Join(agents, " and "))
		for _, a := range agents {
			fmt.Fprintf(stderr, "  agentop server use %s --agent %s\n", other, a)
		}
		return 1
	}
	// Only against a proxy that answered: its session API is the one with the pins.
	sessionsURL := ""
	if statsURL != "" {
		sessionsURL = dialURL(cfg.Listener.SessionAPIAddr)
	}
	// WHICH OF THEM GET THE ERROR, NOT "EACH": the router denies a session pinned to name only
	// on a request addressed to a server that is left, since that is the only request it still
	// handles. One addressed to name's own host is no longer an inference server's request
	// (skip/not_an_inference_server) and goes there untouched, the client's key and all. The
	// count cannot tell the two apart — the summary says where a session's requests went, not
	// where they were addressed — so the warning says what happens to each.
	if n := runningOn(sessionsURL, c, name); n > 0 {
		fmt.Fprintf(stderr, "agentop server remove: warning: %s last sent inference to %s. A session the proxy routed "+
			"there gets an error asking for a new session from its next request to a server that is left, until %s is "+
			"added back; a request addressed to %s's own host is no longer routed, and goes there with the agent's own "+
			"key.\n", runningSessions(n), name, name, name)
	}
	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"servers", name}}
	return runServerWrite(stdout, stderr, path, statsURL, ch, writeReport{
		done: "Removed " + name + ".",
		live: fmt.Sprintf("A session the proxy routed to it now gets an error asking for a new session from its next "+
			"request to a server that is left; a request addressed to %s's own host is no longer routed, and goes "+
			"there with the agent's own key. Adding %s back routes them to it again.", name, name),
	})
}

// runningOn counts the sessions the proxy at sessionsURL holds in memory whose inference
// traffic last went to server name: the ones a remove can leave asking for a new session.
//
// NOT THE LISTENER'S SYNTHETIC SESSIONS — the default bucket and the pending:<agent> ones.
// Each holds many conversations, so the router never pins one; their requests follow the
// agent's current server, which cannot be name, since remove refuses a server an agent is
// routed to. None of them can get the error, whatever host it last sent to.
//
// IN MEMORY, NOT IN THE ARCHIVE, AND BY A REQUEST THIS PROXY SAW: the router pins a session by
// the first request it routes and keeps the pin in memory, so a session only the archive holds
// has no pin left to break, and neither has a resident one whose host is only its history's.
// SessionSummary.Active would not do — it marks the one most recently updated session.
//
// 0 when the proxy does not answer, which is the quiet side for a warning: the remove itself
// is what matters, and it goes ahead either way.
func runningOn(sessionsURL string, c routerconfig.Config, name string) int {
	if sessionsURL == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	list, err := apiclient.New(sessionsURL).ListSessions(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, s := range list {
		// Its host only its history's: resumed after a restart, nothing sent since, so no pin
		// survived to hold it to name (see SessionSummary.InferenceHostFromHistory).
		if s.InferenceHostFromHistory || s.ID == session.DefaultSessionID || strings.HasPrefix(s.ID, session.PendingPrefix) {
			continue
		}
		if on, ok := servers.ForHost(c, s.InferenceHost); ok && on == name {
			n++
		}
	}
	return n
}

// runningSessions is "1 running session" or "n running sessions".
func runningSessions(n int) string {
	if n == 1 {
		return "1 running session"
	}
	return fmt.Sprintf("%d running sessions", n)
}

func serverUse(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server use", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	agent := flags.String("agent", "", "the agent whose new sessions go to the server, such as claude-code")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 || *agent == "" {
		fmt.Fprintln(stderr, "agentop server use: want a server and an agent: agentop server use <name> --agent <agent>")
		return 2
	}
	name := pos[0]
	// First, as add does: see serverRemove.
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 2
	}
	if err := routerconfig.CheckAgent(*agent); err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 2
	}
	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 1
	}
	if _, ok := c.Servers[name]; !ok {
		fmt.Fprintf(stderr, "agentop server use: no server named %s%s. Add it first:\n  agentop server add %s <url>\n",
			name, configuredServers(c), name)
		return 1
	}
	ch := servers.AgentChange(*agent, name)
	return runServerWrite(stdout, stderr, path, statsURL, ch, writeReport{
		done:      fmt.Sprintf("New %s sessions → %s.", *agent, name),
		live:      runningSessionsStay,
		unchanged: fmt.Sprintf("New %s sessions already go to %s.", *agent, name),
		note:      routerInactive(cfg, path),
	})
}

func serverReset(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server reset", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	agent := flags.String("agent", "", "the agent to stop routing, such as claude-code")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 0 || *agent == "" {
		fmt.Fprintln(stderr, "agentop server reset: want an agent: agentop server reset --agent <agent>")
		return 2
	}
	if err := routerconfig.CheckAgent(*agent); err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 2
	}
	cfg, path, statsURL, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 1
	}
	if c.Agents[*agent] == "" {
		fmt.Fprintf(stdout, "%s is not routed; nothing to change.\n", *agent)
		return 0
	}
	ch := servers.AgentChange(*agent, "")
	return runServerWrite(stdout, stderr, path, statsURL, ch, writeReport{
		done:      fmt.Sprintf("New %s sessions are no longer routed.", *agent),
		live:      runningSessionsStay,
		unchanged: fmt.Sprintf("%s is not routed; nothing to change.", *agent),
	})
}

// runningSessionsStay is what use and reset mean for sessions already running,
// said once the running proxy has the change. It holds a session by its pin or by
// the request to a server its history records. One that has sent nothing since the
// proxy started has neither, and a quiet one the store has evicted (session.max_sessions,
// least recently used first, or a configured session.ttl) has no history, so either
// looks new unless the router pinned it; hence "can be".
const runningSessionsStay = "Sessions already running stay where they are, except one that has sent nothing " +
	"since the proxy last started, or that the proxy has dropped from memory (it keeps the most recently used, " +
	"100 by default), which can be treated as a new one."

// writeReport is what runServerWrite says about a change, by how it landed.
type writeReport struct {
	// done is the change, said once the proxy has it or will load it at start.
	done string
	// live is what the change means for sessions already running. Said only when
	// the running proxy reloaded it: that proxy's pins and history are what hold a
	// running session where it is, and a proxy that starts later has neither.
	live string
	// unchanged is said when there was nothing to write.
	unchanged string
	// note, when set, follows any of those: why the router will route nothing
	// whatever was changed (see routerInactive).
	note string
}

// runServerWrite makes one change to the router entry and reports how it landed.
func runServerWrite(stdout, stderr io.Writer, path, statsURL string, ch edit.ConfigChange, r writeReport) int {
	res, err := writePluginConfig(context.Background(), edit.ConfigWrite{
		Path: path, StatsURL: statsURL, Changes: []edit.ConfigChange{ch}, Verify: servers.Verify,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agentop server: %v\n", err)
		return 1
	}
	say := func(lines ...string) {
		for _, l := range lines {
			if l != "" {
				fmt.Fprintln(stdout, l)
			}
		}
	}
	shown := homeTilde(path)
	switch res.Outcome {
	case edit.WriteUnchanged:
		say(r.unchanged, r.note)
	case edit.WriteReloaded:
		say(r.done, r.live, r.note)
	case edit.WriteNotRunning:
		next := fmt.Sprintf("Written to %s. No Cortex answered at its stats address, so this applies when the proxy next starts.", shown)
		if r.live != "" {
			// Not r.live: a proxy that starts holds no pins and no history.
			next += " A proxy that starts knows where no session is, so it treats every session it sees as a new one, " +
				"the ones running now included."
		}
		say(r.done, next, r.note)
	case edit.WriteReloadFailed:
		// WritePluginConfig has put the file back, so it does not hold the change;
		// saying it was written would send the user looking for an edit that is not
		// there. Where it could not, it returned an error saying so, handled above.
		fmt.Fprintf(stderr, "agentop server: the change was not applied: the proxy refused it and keeps its previous configuration, "+
			"so %s was put back as it was:\n  %s\n", shown, res.ReloadError)
		return 1
	case edit.WriteStatusUnreachable:
		// Not a refusal: nothing said no. The proxy went away, and whether it took
		// the change before it did is unknown, so the file was put back to what it
		// last ran, which is what it starts from if it was stopped.
		fmt.Fprintf(stderr, "agentop server: the change was not confirmed: the proxy stopped answering at %s before it reported the reload, "+
			"so %s was put back as it was; check the proxy is running and try again:\n  %s\n", statsURL, shown, res.ReloadError)
		return 1
	case edit.WriteReloadTimedOut:
		fmt.Fprintf(stderr, "agentop server: wrote %s, but the proxy reported no reload within %s; check %s/reload/status\n",
			shown, edit.LocalPollDeadline, statsURL)
		return 1
	}
	return 0
}

// configuredServers is "; configured: a, b" for an error naming a missing server,
// or "" when there are none.
func configuredServers(c routerconfig.Config) string {
	if len(c.Servers) == 0 {
		return ""
	}
	return "; configured: " + strings.Join(slices.Sorted(maps.Keys(c.Servers)), ", ")
}
