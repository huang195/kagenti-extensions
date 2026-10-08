package tui

import (
	"encoding/json"
	"maps"

	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// activeRouter is the config of the inference-router the proxy runs, read off /v1/pipeline, and
// whether it routes. on is false wherever routerStatus has a reason it does not: the SERVER
// columns hide, and S refuses with that reason.
func (m *model) activeRouter() (c routerconfig.Config, on bool) {
	c, why := m.routerStatus()
	return c, why == ""
}

// routerStatus is the inference-router entry of the outbound pipeline the proxy runs, and why it
// routes nothing — "" when it routes. The reason is S's refusal, so it names this machine's
// config file, m.localConfigPath, where the fix goes; S asks only when attached to it.
//
// THE RUNNING CONFIGURATION, NOT THE FILE: what the SERVER columns and the S picker show is
// what the proxy routes by now, and an edit it has not reloaded — or refused — is not that. The
// keys arrive as "[REDACTED]" (core/redact blanks every field named key), and nothing here
// reads one.
//
// ITS POLICY AS WELL AS ITS CONFIG: under on_error: observe the router stays in the pipeline
// and keeps its config, but moves nothing, so a column read off the config alone would name a
// server no request goes to, and S would write a route that does not happen. servers.Inactive
// decides it, as it does for `agentop server`, and words it the same way.
//
// Decoded leniently, as `agentop server`'s listing reads the file: the proxy validated this
// config before it ran it. But a config that does not decode at all is not on — a column read
// off half of one would name servers that were never read — and that is the one reason here no
// edit to the file fixes, since the proxy runs it: an agentop older than the proxy reading a
// field it does not know the shape of.
func (m *model) routerStatus() (c routerconfig.Config, why string) {
	if m.pipeline == nil {
		return routerconfig.Config{}, "agentop has not read this Cortex's pipeline yet; try again in a moment"
	}
	for _, p := range m.pipeline.Outbound {
		if p.Name != servers.PluginName {
			continue
		}
		if why := servers.Inactive(p.OnError, m.localConfigPath); why != "" {
			return routerconfig.Config{}, why
		}
		if err := json.Unmarshal(p.Config, &c); err != nil {
			return routerconfig.Config{}, "agentop cannot read the inference-router's running config, so it can neither show nor change its routing; " +
				"an agentop as new as the proxy can"
		}
		return c, ""
	}
	// NOT CONFIGURED, OR OFF, AND THIS SIDE CANNOT TELL WHICH: the proxy does not build an
	// on_error: off plugin, so /v1/pipeline lists neither. Both fixes, then.
	return routerconfig.Config{}, "this Cortex runs no inference-router: add a server with agentop server add <name> <url>, " +
		"or, if " + m.localConfigPath + " has one under on_error: off, remove on_error to route"
}

// sameRouter reports whether two router configs route alike, so a pipeline refetch repaints the
// SERVER columns only when what they show has changed.
func sameRouter(a, b routerconfig.Config) bool {
	return maps.Equal(a.Servers, b.Servers) && maps.Equal(a.Agents, b.Agents)
}

// localCortexTarget is the config file and stats URL of the Cortex on this machine, when the
// endpoint on screen IS that Cortex. pipelineStore's local half and S share it, so `e` and S
// cannot disagree about which proxy they would change.
//
// Compared against localEndpoint rather than localEndpointOr(): the fallback is the in-cluster
// 9094, and matching that would claim a hand-run port-forward to a POD is this machine's config.
func (m *model) localCortexTarget() (path, statsURL string, ok bool) {
	if m.client != nil && m.localEndpoint != "" && sameEndpoint(m.client.Endpoint(), m.localEndpoint) &&
		m.localConfigPath != "" && m.localStatsURL != "" {
		return m.localConfigPath, m.localStatsURL, true
	}
	return "", "", false
}

// agentOwnChoice is the AGENTS pane's SERVER cell for an agent the router does not route.
//
// THE WORDING IS THE DIMMING. The spec draws it dimmed, and a table cell can carry no ANSI —
// the table truncates escapes as text (TestSessionsRows_CarryNoANSIUnderAForcedColourProfile).
// No server can be named this, since a server name has no space, so it never reads as one.
const agentOwnChoice = "own choice"

// agentsServerWidth fits agentOwnChoice and every server name worth typing.
const agentsServerWidth = 12

// agentServerCell is the AGENTS pane's SERVER cell for the row labelled label: the server its
// new sessions go to, or agentOwnChoice. A row no server can be chosen for shows nothing: Other,
// which pools every agent Cortex does not recognise, so there is no one agent for the router to
// name, and any agent the router refuses to route — the no-User-Agent row, whose requests belong
// to no one agent. "own choice" there would offer a choice S then refuses.
//
// SANITISED AT RENDER TIME, as every served label is: the proxy only runs server names its rule
// allows, but the name arrives over an unauthenticated API, and nothing here assumes the producer
// checked anything.
func agentServerCell(router routerconfig.Config, label string) string {
	agent := pipeline.AgentName(label)
	if label == otherAgents || routerconfig.CheckAgent(agent) != nil {
		return ""
	}
	if s := router.Agents[agent]; s != "" {
		return sanitizeLabel(s)
	}
	return agentOwnChoice
}
