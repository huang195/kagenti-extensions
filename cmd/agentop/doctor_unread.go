package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// fetchUnreadReport fetches the running proxy's /tls-bridge/unread report. A var so the
// doctor scene can stop its tests reaching whatever proxy runs on this machine.
var fetchUnreadReport = fetchUnread

func fetchUnread(base string) (tlsbridge.UnreadReport, bool) {
	c := &http.Client{Timeout: localProbeTimeout}
	resp, err := c.Get(base + "/tls-bridge/unread") //nolint:noctx // bounded by Timeout
	if err != nil {
		return tlsbridge.UnreadReport{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tlsbridge.UnreadReport{}, false
	}
	var rep tlsbridge.UnreadReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return tlsbridge.UnreadReport{}, false
	}
	return rep, true
}

// checkUnread lists the programs the running proxy is passing through unread, from its
// /tls-bridge/unread report, and on macOS says how to have the Go ones read. It replaces
// a check that searched the login keychain for the CA: the proxy's own answer counts
// trust granted in any domain, and names the programs it affects.
//
// Advice only, never a failure: a program passed through is working. Silent when the
// bridge is off, or when nothing at the stats address gives a report — a stopped proxy is
// another check's to report, and an older one has no list.
func checkUnread(env *setupEnv, ui *checklist.UI) {
	cfg, err := config.Load(env.configPath())
	if err != nil || !bridgeEnabled(cfg) {
		return
	}
	base := dialURL(cfg.Stats.StatsAddress)
	if base == "" {
		return
	}
	rep, ok := fetchUnreadReport(base)
	if !ok {
		return
	}
	if len(rep.Programs) == 0 {
		ui.Done("unread", "Cortex reads every program that reaches it", 0)
		return
	}
	goPrograms := false
	for _, p := range rep.Programs {
		ui.Note("unread", unreadLine(env, p))
		goPrograms = goPrograms || p.Reason == tlsbridge.UnreadOSTrustOnly
	}
	if goPrograms && env.goos == "darwin" {
		// What trusting the CA buys is said under the fix rather than promised by it: Go
		// programs are then read on the hosts Cortex intercepts, and go's and gh's own
		// hosts are not among them, so neither tool is read for its usual traffic.
		keychain := filepath.Join(env.home, "Library", "Keychains", "login.keychain-db")
		ui.Advise("Go tools", "work through Cortex but are not read (a Go agent's model calls are not priced either): macOS does not trust its CA",
			"security add-trusted-cert -k "+env.tilde(keychain)+" -p ssl "+env.tilde(filepath.Join(cfg.TLSBridge.CADir, "ca.crt")))
		ui.Faint("    once macOS trusts the CA, Cortex reads Go programs' traffic to the hosts it intercepts, within a minute") // at the fix's indent
	}
}

// unreadLine is one program's line: its name, the agent it ran under, why it is not
// read, the process when the entry is one process's, and the last host. The agent is
// shown as a path because an agent's executable is often named for its version (Claude
// Code's is), which says nothing on its own.
func unreadLine(env *setupEnv, p tlsbridge.UnreadProgram) string {
	s := filepath.Base(p.Program)
	if p.Agent != "" {
		s += " under " + env.tilde(p.Agent)
	}
	s += ": " + p.Reason
	if p.Stopped {
		s += ", not retried until Cortex restarts"
	}
	var aside []string
	if p.PID != 0 {
		// Only a process older than the CA is remembered by its PID: it cannot have
		// loaded the CA, so the program itself is not at fault.
		aside = append(aside, fmt.Sprintf("pid %d, started before Cortex's CA — restarting it is enough", p.PID))
	}
	if p.LastHost != "" {
		aside = append(aside, "last "+p.LastHost)
	}
	if len(aside) > 0 {
		s += " (" + strings.Join(aside, "; ") + ")"
	}
	return s
}
