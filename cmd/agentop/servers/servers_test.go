package servers

import (
	"net/url"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

var twoServers = routerconfig.Config{Servers: map[string]routerconfig.Server{
	"ete": {URL: "https://ete.example.com", Key: "k"},
	"glm": {URL: "https://GLM.example.com:8443", Key: "k"},
}}

func TestMapping(t *testing.T) {
	for _, tc := range []struct {
		s    routerconfig.Server
		want string
	}{
		{routerconfig.Server{}, "uses Claude Code's names"},
		{routerconfig.Server{Opus: "glm-5.3", Sonnet: "glm-5.3", Haiku: "glm-5.3"}, "all → glm-5.3"},
		{routerconfig.Server{Opus: "big", Sonnet: "mid", Haiku: "small"}, "opus → big · sonnet → mid · haiku → small"},
	} {
		if got := Mapping(tc.s); got != tc.want {
			t.Errorf("Mapping(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestHost(t *testing.T) {
	for url, want := range map[string]string{
		"https://ete.example.com:443":  "ete.example.com",
		"https://glm.example.com:8443": "glm.example.com:8443",
		"http://localhost:4000":        "http://localhost:4000",
	} {
		if got := Host(routerconfig.Server{URL: url}); got != want {
			t.Errorf("Host(%q) = %q, want %q", url, got, want)
		}
	}
}

// A URL the router refuses shows nothing of itself, not even its host: the likeliest
// reason it is refused is a pasted key, and a key given as a username containing
// '/', '?' or '#' ends the authority early, so url.Parse takes the key for the host.
func TestHost_ShowsNothingOfARefusedURL(t *testing.T) {
	for _, raw := range []string{
		"https://u:sk-secret@x.example",
		"https://sk-SECRET/x@h.example.com",
		"https://sk-SECRET?x@h.example.com",
		"https://sk-SECRET#x@h.example.com",
		"https://h.example.com/?key=sk-SECRET",
		"https://h.example.com:sk-SECRET",
	} {
		got := Host(routerconfig.Server{URL: raw})
		if got != "not a valid URL" {
			t.Errorf("Host(%q) = %q, want %q", raw, got, "not a valid URL")
		}
		parts := []string{"secret", "x.example", "h.example.com", "u:"}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			parts = append(parts, strings.ToLower(u.Host))
		}
		for _, part := range parts {
			if strings.Contains(strings.ToLower(got), part) {
				t.Errorf("Host(%q) = %q shows %q", raw, got, part)
			}
		}
	}
}

// Matched as the plugin matches a request: the port and the case do not matter, so an event
// recorded as glm.example.com:8443 and a server configured on GLM.example.com are one host.
func TestForHost(t *testing.T) {
	for host, want := range map[string]string{
		"ete.example.com":      "ete",
		"ete.example.com:443":  "ete",
		"glm.example.com:8443": "glm",
		"GLM.EXAMPLE.COM":      "glm",
		"api.anthropic.com":    "",
		"":                     "",
	} {
		got, ok := ForHost(twoServers, host)
		if got != want || ok != (want != "") {
			t.Errorf("ForHost(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
}

func TestAgentChange(t *testing.T) {
	set := AgentChange("claude-code", "glm")
	if set.Chain != "outbound" || set.Plugin != PluginName || strings.Join(set.Path, ".") != "agents.claude-code" ||
		set.Value == nil || set.Value.Value != "glm" || set.CreatePlugin {
		t.Errorf("AgentChange(claude-code, glm) = %+v", set)
	}
	if reset := AgentChange("claude-code", ""); reset.Value != nil {
		t.Errorf("AgentChange(claude-code, \"\") sets %v; want a removal", reset.Value)
	}
}

// Verify applies the plugin's own rules to the router entry and ignores every other entry.
func TestVerify(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pipeline.Outbound.Plugins = []config.PluginEntry{
		{Name: "tool-prune", Config: []byte(`{"remove": []}`)},
		{Name: PluginName, Config: []byte(`{"servers": {"ete": {"url": "https://ete.example.com", "key": "k"}}, "agents": {"claude-code": "glm"}}`)},
	}
	if err := Verify(cfg); err == nil || !strings.Contains(err.Error(), `"glm" is not a server listed under servers`) {
		t.Errorf("Verify = %v, want the plugin's refusal of an agent routed to no server", err)
	}
	cfg.Pipeline.Outbound.Plugins[1].Config = []byte(`{"servers": {"ete": {"url": "https://ete.example.com", "key": "k"}}, "agents": {"claude-code": "ete"}}`)
	if err := Verify(cfg); err != nil {
		t.Errorf("Verify = %v on a valid router entry", err)
	}
}
