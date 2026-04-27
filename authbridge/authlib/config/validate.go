package config

import (
	"fmt"
	"log/slog"
)

// Validate checks the configuration for errors and warnings.
// Returns an error for invalid configurations that would fail at runtime.
// Logs warnings for unusual-but-valid combinations.
func Validate(cfg *Config) error {
	// Mode is required
	switch cfg.Mode {
	case ModeEnvoySidecar, ModeWaypoint, ModeProxySidecar:
		// valid
	case "":
		return fmt.Errorf("mode is required (envoy-sidecar, waypoint, or proxy-sidecar)")
	default:
		return fmt.Errorf("unknown mode %q (valid: envoy-sidecar, waypoint, proxy-sidecar)", cfg.Mode)
	}

	// Required fields
	if cfg.Inbound.Issuer == "" {
		return fmt.Errorf("inbound.issuer is required")
	}
	if cfg.Inbound.JWKSURL == "" {
		return fmt.Errorf("inbound.jwks_url is required")
	}
	if cfg.Outbound.TokenURL == "" {
		// token_url may have been derived from keycloak_url + keycloak_realm in Resolve()
		return fmt.Errorf("outbound.token_url is required (or set keycloak_url + keycloak_realm)")
	}

	// Identity validation
	if err := validateIdentity(cfg); err != nil {
		return err
	}

	// Mode-specific listener validation
	if err := validateListeners(cfg); err != nil {
		return err
	}

	// Warnings for unusual combinations
	warnUnusual(cfg)

	return nil
}

func validateIdentity(cfg *Config) error {
	switch cfg.Identity.Type {
	case "spiffe":
		if cfg.Identity.SocketPath == "" && cfg.Identity.JWTSVIDPath == "" {
			return fmt.Errorf("identity.type=spiffe requires socket_path or jwt_svid_path")
		}
		if cfg.Identity.ClientID == "" && cfg.Identity.ClientIDFile == "" {
			return fmt.Errorf("identity.type=spiffe requires client_id or client_id_file")
		}
	case "client-secret":
		if cfg.Identity.ClientID == "" && cfg.Identity.ClientIDFile == "" {
			return fmt.Errorf("identity.type=client-secret requires client_id or client_id_file")
		}
		if cfg.Identity.ClientSecret == "" && cfg.Identity.ClientSecretFile == "" {
			return fmt.Errorf("identity.type=client-secret requires client_secret or client_secret_file")
		}
	case "k8s-sa":
		// Future: validate service account token path
	case "":
		return fmt.Errorf("identity.type is required")
	default:
		return fmt.Errorf("unknown identity.type %q (valid: spiffe, client-secret, k8s-sa)", cfg.Identity.Type)
	}
	return nil
}

func validateListeners(cfg *Config) error {
	switch cfg.Mode {
	case ModeEnvoySidecar:
		if cfg.Listener.ReverseProxyAddr != "" {
			return fmt.Errorf("envoy-sidecar mode does not support reverse_proxy_addr (use proxy-sidecar mode)")
		}
		if cfg.Listener.ExtAuthzAddr != "" {
			return fmt.Errorf("envoy-sidecar mode does not support ext_authz_addr (use waypoint mode)")
		}
	case ModeWaypoint:
		if cfg.Listener.ExtProcAddr != "" {
			return fmt.Errorf("waypoint mode does not support ext_proc_addr (use envoy-sidecar mode)")
		}
		if cfg.Listener.ReverseProxyAddr != "" {
			return fmt.Errorf("waypoint mode does not support reverse_proxy_addr")
		}
	case ModeProxySidecar:
		if cfg.Listener.ExtProcAddr != "" {
			return fmt.Errorf("proxy-sidecar mode does not support ext_proc_addr (use envoy-sidecar mode)")
		}
		if cfg.Listener.ExtAuthzAddr != "" {
			return fmt.Errorf("proxy-sidecar mode does not support ext_authz_addr (use waypoint mode)")
		}
		if cfg.Listener.ReverseProxyBackend == "" {
			return fmt.Errorf("proxy-sidecar mode requires listener.reverse_proxy_backend")
		}
	}
	return nil
}

func warnUnusual(cfg *Config) {
	warnings := []string{}

	if cfg.Mode == ModeEnvoySidecar && cfg.Identity.Type == "client-secret" {
		warnings = append(warnings, "envoy-sidecar with client-secret identity is unusual (typically uses spiffe)")
	}
	if cfg.Mode == ModeWaypoint && cfg.Identity.Type == "spiffe" {
		warnings = append(warnings, "waypoint with spiffe identity is unusual (typically uses client-secret)")
	}

	for _, w := range warnings {
		slog.Warn(w)
	}
}

// ValidateOutboundPolicy checks that default_policy is a valid value.
func ValidateOutboundPolicy(policy string) error {
	switch policy {
	case "exchange", "passthrough", "":
		return nil
	default:
		return fmt.Errorf("unknown outbound.default_policy %q (valid: exchange, passthrough)", policy)
	}
}
