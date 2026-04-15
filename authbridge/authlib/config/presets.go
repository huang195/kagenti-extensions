package config

import (
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
)

// ApplyPreset fills in mode-specific defaults for settings the user didn't specify.
// Locked settings (which listeners to start) are enforced by Phase 3 listeners,
// not here — this only sets defaults for user-overridable settings.
func ApplyPreset(cfg *Config) {
	switch cfg.Mode {
	case ModeEnvoySidecar:
		setDefault(&cfg.Identity.Type, "spiffe")
		setDefault(&cfg.Outbound.DefaultPolicy, "passthrough")
		setDefault(&cfg.Listener.ExtProcAddr, ":9090")

	case ModeWaypoint:
		setDefault(&cfg.Identity.Type, "client-secret")
		setDefault(&cfg.Outbound.DefaultPolicy, "exchange")
		setDefault(&cfg.Listener.ExtAuthzAddr, ":9090")
		setDefault(&cfg.Listener.ForwardProxyAddr, ":8080")

	case ModeProxySidecar:
		setDefault(&cfg.Identity.Type, "spiffe")
		setDefault(&cfg.Outbound.DefaultPolicy, "passthrough")
		setDefault(&cfg.Listener.ReverseProxyAddr, ":8080")
		setDefault(&cfg.Listener.ForwardProxyAddr, ":8081")
	}

	// All modes share the same default bypass paths
	if len(cfg.Bypass.InboundPaths) == 0 {
		cfg.Bypass.InboundPaths = bypass.DefaultPatterns
	}
}

// NoTokenPolicyForMode returns the no-token outbound policy for a mode.
// This is locked per mode — users cannot override it.
func NoTokenPolicyForMode(mode string) string {
	switch mode {
	case ModeEnvoySidecar:
		return auth.NoTokenPolicyClientCredentials
	case ModeWaypoint:
		return auth.NoTokenPolicyAllow
	case ModeProxySidecar:
		return auth.NoTokenPolicyDeny
	default:
		return auth.NoTokenPolicyDeny
	}
}

func setDefault(field *string, value string) {
	if *field == "" {
		*field = value
	}
}
