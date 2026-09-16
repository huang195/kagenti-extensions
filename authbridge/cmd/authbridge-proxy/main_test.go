package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/tokenexchange"
)

func identityConfig(idType string) json.RawMessage {
	return json.RawMessage(`{"identity":{"type":"` + idType + `"}}`)
}

// TestSpiffeIdentityTypeMatchesTokenExchange guards against drift between
// main.go's local spiffeIdentityType constant and the token-exchange plugin's
// canonical SpiffeIdentity value. main.go intentionally does NOT import the
// plugin package (so token-exchange stays build-tag excludable via
// plugins_tokenexchange.go); this test re-couples the two at test time — a
// test file may import the package unconditionally without pulling it into the
// tag-gated production binary.
func TestSpiffeIdentityTypeMatchesTokenExchange(t *testing.T) {
	if spiffeIdentityType != tokenexchange.SpiffeIdentity {
		t.Errorf("spiffeIdentityType = %q but tokenexchange.SpiffeIdentity = %q; "+
			"the identity.type=spiffe convention has drifted — update spiffeIdentityType in main.go",
			spiffeIdentityType, tokenexchange.SpiffeIdentity)
	}
}

// TestSpiffeProviderNeeded pins the need-driven gate: the SPIFFE provider is
// built only when mTLS is on or a plugin selects the spiffe identity scheme.
// A bare `spiffe: {}` block with neither must NOT trigger it — that is what
// lets the TLS bridge (cert-manager CA, no SVID) boot without SPIRE.
func TestSpiffeProviderNeeded(t *testing.T) {
	outbound := func(entries ...config.PluginEntry) *config.Config {
		return &config.Config{Pipeline: config.PipelineConfig{
			Outbound: config.PipelineStageConfig{Plugins: entries},
		}}
	}
	inbound := func(entries ...config.PluginEntry) *config.Config {
		return &config.Config{Pipeline: config.PipelineConfig{
			Inbound: config.PipelineStageConfig{Plugins: entries},
		}}
	}

	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"empty config", &config.Config{}, false},
		{"mtls present", &config.Config{MTLS: &config.MTLSConfig{}}, true},
		{"token-exchange client-secret", outbound(config.PluginEntry{
			Name: "token-exchange", Config: identityConfig("client-secret"),
		}), false},
		{"token-exchange spiffe (outbound)", outbound(config.PluginEntry{
			Name: "token-exchange", Config: identityConfig("spiffe"),
		}), true},
		{"spiffe identity (inbound)", inbound(config.PluginEntry{
			Name: "some-plugin", Config: identityConfig("spiffe"),
		}), true},
		{"plugin with no config", outbound(config.PluginEntry{Name: "jwt-validation"}), false},
		{"malformed plugin config", outbound(config.PluginEntry{
			Name: "bad-plugin", Config: json.RawMessage(`{not valid json`),
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spiffeProviderNeeded(tt.cfg); got != tt.want {
				t.Errorf("spiffeProviderNeeded() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestProviderConsumersCoveredByPredicate enforces the invariant that ties
// spiffeProviderNeeded to plugins.BuildWithSPIFFE: the predicate detects a
// plugin's need via identity.type=spiffe, which covers token-exchange — the
// only spiffe.ProviderConsumer today. If a new ProviderConsumer is registered,
// this fails so the author confirms spiffeProviderNeeded detects its need;
// otherwise that plugin would silently receive a nil Provider on a SPIRE-less
// cluster. The main package blank-imports every plugin, so the registry here is
// the full production set.
func TestProviderConsumersCoveredByPredicate(t *testing.T) {
	// Plugins whose SPIFFE need spiffeProviderNeeded is known to detect.
	covered := map[string]bool{"token-exchange": true}
	for _, name := range plugins.SPIFFEConsumerPlugins() {
		// Tripwire: a new consumer must be consciously reviewed and listed.
		if !covered[name] {
			t.Errorf("plugin %q implements spiffe.ProviderConsumer but is not covered by "+
				"spiffeProviderNeeded; make it signal its need via identity.type=spiffe (or "+
				"extend the predicate), then add %q to this set", name, name)
		}
		// Functional: the predicate must actually fire for that consumer's
		// spiffe config, so it never receives a nil Provider.
		cfg := &config.Config{Pipeline: config.PipelineConfig{
			Outbound: config.PipelineStageConfig{Plugins: []config.PluginEntry{
				{Name: name, Config: identityConfig("spiffe")},
			}},
		}}
		if !spiffeProviderNeeded(cfg) {
			t.Errorf("spiffeProviderNeeded must return true for consumer %q with identity.type=spiffe", name)
		}
	}
}

// captureWarns runs fn with a logger recording WARN records as JSON lines.
func captureWarns(t *testing.T, fn func(*slog.Logger)) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	fn(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// A cost ledger that is on by DEFAULT and unreachable because sessions are off must
// say so. This is the residue of the same defect config.Validate now refuses in its
// explicit form: the whole ledger block in main is nested inside
// `if cfg.Session.SessionEnabled()`, so with sessions off the operator got no error,
// no warning, and no log line naming the ledger at all — an empty `abctl usage`
// history with nothing anywhere to explain it.
//
// Local mode is the case that matters: --local turns the ledger on without anyone
// writing cost_ledger into the config, so this is the one combination that is
// legitimate, silent, and wrong.
func TestWarnCostLedgerNeedsSessions_LocalDefaultOn(t *testing.T) {
	off := false
	recs := captureWarns(t, func(l *slog.Logger) {
		warnCostLedgerNeedsSessions(&config.Config{
			Mode:    config.ModeProxySidecar,
			Session: config.SessionConfig{Enabled: &off},
		}, true, l)
	})
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 warn, got %d: %#v", len(recs), recs)
	}
	msg, _ := recs[0]["msg"].(string)
	if !strings.Contains(msg, "cost ledger") {
		t.Errorf("warn must name the ledger, got %q", msg)
	}
	// Both settings, because the fix is a decision between them and a message naming
	// one would send the reader to the wrong file.
	for _, key := range []string{"cost_ledger.enabled", "session.enabled"} {
		if _, ok := recs[0][key]; !ok {
			t.Errorf("warn must name %q; got keys %v", key, recs[0])
		}
	}
}

// It must stay quiet when the ledger was not going to run anyway — otherwise every
// in-cluster deployment with sessions off logs a warning about a feature it never
// asked for, and the line stops meaning anything.
func TestWarnCostLedgerNeedsSessions_SilentWhenLedgerIsOff(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name      string
		cfg       *config.Config
		defaultOn bool
	}{
		{"no durable location, so the ledger was never going to run", &config.Config{Mode: config.ModeProxySidecar, Session: config.SessionConfig{Enabled: &off}}, false},
		{"explicitly off, so its absence is a choice", &config.Config{Mode: config.ModeProxySidecar, Session: config.SessionConfig{Enabled: &off}, CostLedger: &config.CostLedgerConfig{Enabled: &off}}, true},
		// Sessions on is the normal --local shape: the ledger runs, so there is
		// nothing to warn about. This is what makes the call site in main safe to
		// make unconditional instead of hiding it down one arm of an if.
		{"sessions on, which is the ordinary shape", &config.Config{Mode: config.ModeProxySidecar}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if recs := captureWarns(t, func(l *slog.Logger) {
				warnCostLedgerNeedsSessions(tc.cfg, tc.defaultOn, l)
			}); len(recs) != 0 {
				t.Errorf("expected silence, got %#v", recs)
			}
		})
	}
}

// THE DEFAULT IS KEYED ON DURABILITY, NOT ON WHICH FLAG STARTED THE BINARY.
//
// It used to be localMode: true under --local, false otherwise. That is a property of the
// command line, not of whether the ledger can do its job, and the consequence was measured —
// `abctl service install` runs `--config`, never `--local`, so the documented "on by default"
// was false for every installed laptop and cost history was silently never written.
//
// The rule now asks whether anything survives a restart. The third row is the one that has to
// stay OFF: a container with no volume can only write to a layer that is discarded on restart,
// so the ledger would pay its whole cost and keep nothing — and at a measured 36 MB to 1.2 GB
// per 30 days it would do that against an ephemeral-storage limit, where exceeding it evicts
// the pod rather than losing a figure.
func TestLedgerDefaultOn_KeyedOnDurabilityNotOnAFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dir     string
		home    string
		want    bool
		wantWhy string
	}{
		{
			// The case that regressed: a service install, no dir named, real home.
			name: "installed service with a home directory", home: t.TempDir(),
			want: true, wantWhy: "home directory",
		},
		{
			// Kubernetes done properly: the operator mounted a volume and named it.
			name: "explicit dir and no home", dir: "/var/lib/cortex/cost",
			want: true, wantWhy: "cost_ledger.dir",
		},
		{
			// A container with neither. The only row that must be off.
			name: "no dir and no home", want: false, wantWhy: "discarded on restart",
		},
		{
			name: "both, dir decides", dir: "/var/lib/cortex/cost", home: t.TempDir(),
			want: true, wantWhy: "cost_ledger.dir",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// HOME drives os.UserHomeDir on the platforms this runs on; empty makes it
			// fail, which is the container-with-no-home case.
			t.Setenv("HOME", tc.home)
			cfg := &config.Config{Mode: config.ModeProxySidecar}
			if tc.dir != "" {
				cfg.CostLedger = &config.CostLedgerConfig{Dir: tc.dir}
			}

			got, why := ledgerDefaultOn(cfg)
			if got != tc.want {
				t.Errorf("ledgerDefaultOn() = %v, want %v (reason given: %q)", got, tc.want, why)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("reason = %q, want it to mention %q — the log line quotes this, so a "+
					"vague reason sends an operator to main.go", why, tc.wantWhy)
			}
		})
	}
}

// An explicit cost_ledger.enabled still wins in BOTH directions, so the derived default is a
// default and not a policy. The false case is the escape hatch for a full or read-only disk;
// the true case is a filesystem the operator knows persists even though this process cannot
// tell (a container whose HOME is on a volume mounted over it, say).
func TestLedgerEnabled_ExplicitSettingOverridesTheDerivedDefault(t *testing.T) {
	on, off := true, false
	t.Setenv("HOME", "") // derived default would be OFF here
	cfg := &config.Config{Mode: config.ModeProxySidecar, CostLedger: &config.CostLedgerConfig{Enabled: &on}}
	if !cfg.CostLedger.LedgerEnabled(ledgerDefaultOnValue(cfg)) {
		t.Error("enabled: true did not override a derived default of off")
	}
	home := t.TempDir()
	t.Setenv("HOME", home) // derived default would be ON here
	cfg = &config.Config{Mode: config.ModeProxySidecar, CostLedger: &config.CostLedgerConfig{Enabled: &off}}
	if cfg.CostLedger.LedgerEnabled(ledgerDefaultOnValue(cfg)) {
		t.Error("enabled: false did not override a derived default of on")
	}
}
