package tui

import (
	"encoding/json"
	"maps"

	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// activeRouter is the inference-router entry of the outbound pipeline the proxy runs, read off
// /v1/pipeline. on is false when that pipeline has no such entry, or has not been fetched yet.
//
// THE RUNNING CONFIGURATION, NOT THE FILE: what the SERVER columns and the S picker show is
// what the proxy routes by now, and an edit it has not reloaded — or refused — is not that. The
// keys arrive as "[REDACTED]" (core/redact blanks every field named key), and nothing here
// reads one.
//
// Decoded leniently, as `agentop server`'s listing reads the file: the proxy validated this
// config before it ran it, and a decode this side cannot fail it again.
func (m *model) activeRouter() (c routerconfig.Config, on bool) {
	if m.pipeline == nil {
		return routerconfig.Config{}, false
	}
	for _, p := range m.pipeline.Outbound {
		if p.Name == servers.PluginName {
			_ = json.Unmarshal(p.Config, &c)
			return c, true
		}
	}
	return routerconfig.Config{}, false
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
// new sessions go to, or agentOwnChoice. Other shows nothing — it pools every agent Cortex does
// not recognise, so there is no one agent for the router to name.
func agentServerCell(router routerconfig.Config, label string) string {
	if label == otherAgents {
		return ""
	}
	if s := router.Agents[pipeline.AgentName(label)]; s != "" {
		return s
	}
	return agentOwnChoice
}
