package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- Preset Tests ---

func TestApplyPreset_EnvoySidecar(t *testing.T) {
	cfg := &Config{Mode: ModeEnvoySidecar}
	ApplyPreset(cfg)
	if cfg.Identity.Type != "spiffe" {
		t.Errorf("identity.type = %q, want spiffe", cfg.Identity.Type)
	}
	if cfg.Outbound.DefaultPolicy != "passthrough" {
		t.Errorf("default_policy = %q, want passthrough", cfg.Outbound.DefaultPolicy)
	}
	if cfg.Listener.ExtProcAddr != ":9090" {
		t.Errorf("ext_proc_addr = %q, want :9090", cfg.Listener.ExtProcAddr)
	}
	if len(cfg.Bypass.InboundPaths) == 0 {
		t.Error("expected default bypass paths")
	}
}

func TestApplyPreset_Waypoint(t *testing.T) {
	cfg := &Config{Mode: ModeWaypoint}
	ApplyPreset(cfg)
	if cfg.Identity.Type != "client-secret" {
		t.Errorf("identity.type = %q, want client-secret", cfg.Identity.Type)
	}
	if cfg.Outbound.DefaultPolicy != "exchange" {
		t.Errorf("default_policy = %q, want exchange", cfg.Outbound.DefaultPolicy)
	}
}

func TestApplyPreset_ProxySidecar(t *testing.T) {
	cfg := &Config{Mode: ModeProxySidecar}
	ApplyPreset(cfg)
	if cfg.Identity.Type != "spiffe" {
		t.Errorf("identity.type = %q, want spiffe", cfg.Identity.Type)
	}
	if cfg.Listener.ReverseProxyAddr != ":8080" {
		t.Errorf("reverse_proxy_addr = %q, want :8080", cfg.Listener.ReverseProxyAddr)
	}
}

func TestApplyPreset_UserOverride(t *testing.T) {
	cfg := &Config{
		Mode:     ModeEnvoySidecar,
		Identity: IdentityConfig{Type: "client-secret"}, // user override
	}
	ApplyPreset(cfg)
	if cfg.Identity.Type != "client-secret" {
		t.Errorf("user override lost: identity.type = %q", cfg.Identity.Type)
	}
}

func TestNoTokenPolicyForMode(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{ModeEnvoySidecar, "client-credentials"},
		{ModeWaypoint, "allow"},
		{ModeProxySidecar, "deny"},
		{"unknown", "deny"},
	}
	for _, tt := range tests {
		if got := NoTokenPolicyForMode(tt.mode); got != tt.want {
			t.Errorf("NoTokenPolicyForMode(%q) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

// --- Validation Tests ---

func TestValidate_MissingMode(t *testing.T) {
	cfg := &Config{}
	if err := Validate(cfg); err == nil {
		t.Error("expected error for missing mode")
	}
}

func TestValidate_InvalidMode(t *testing.T) {
	cfg := &Config{Mode: "invalid"}
	if err := Validate(cfg); err == nil {
		t.Error("expected error for invalid mode")
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	base := Config{
		Mode:     ModeWaypoint,
		Identity: IdentityConfig{Type: "client-secret", ClientID: "c", ClientSecret: "s"},
	}

	// Missing issuer
	cfg := base
	cfg.Inbound = InboundConfig{JWKSURL: "http://jwks"}
	cfg.Outbound = OutboundConfig{TokenURL: "http://token"}
	if err := Validate(&cfg); err == nil {
		t.Error("expected error for missing issuer")
	}

	// Missing jwks_url
	cfg = base
	cfg.Inbound = InboundConfig{Issuer: "http://issuer"}
	cfg.Outbound = OutboundConfig{TokenURL: "http://token"}
	if err := Validate(&cfg); err == nil {
		t.Error("expected error for missing jwks_url")
	}

	// Missing token_url
	cfg = base
	cfg.Inbound = InboundConfig{JWKSURL: "http://jwks", Issuer: "http://issuer"}
	if err := Validate(&cfg); err == nil {
		t.Error("expected error for missing token_url")
	}
}

func TestValidate_SpiffeIdentityRequiresPath(t *testing.T) {
	cfg := validEnvoySidecarConfig()
	cfg.Identity = IdentityConfig{Type: "spiffe"}
	if err := Validate(cfg); err == nil {
		t.Error("expected error for spiffe without path")
	}
}

func TestValidate_CrossModeFieldsAreWarnings(t *testing.T) {
	// Cross-mode listener fields are now warnings, not errors.
	// This allows a shared ConfigMap with ${...} placeholders for all modes.
	tests := []struct {
		name string
		cfg  *Config
	}{
		{"envoy-sidecar + reverse_proxy_addr", func() *Config {
			c := validEnvoySidecarConfig()
			c.Listener.ReverseProxyAddr = "${REVERSE_PROXY_ADDR}"
			return c
		}()},
		{"envoy-sidecar + ext_authz_addr", func() *Config {
			c := validEnvoySidecarConfig()
			c.Listener.ExtAuthzAddr = ":9091"
			return c
		}()},
		{"waypoint + ext_proc_addr", func() *Config {
			c := validWaypointConfig()
			c.Listener.ExtProcAddr = ":9090"
			return c
		}()},
		{"waypoint + reverse_proxy_addr", func() *Config {
			c := validWaypointConfig()
			c.Listener.ReverseProxyAddr = ":8080"
			return c
		}()},
		{"proxy-sidecar + ext_proc_addr", func() *Config {
			c := validProxySidecarConfig()
			c.Listener.ExtProcAddr = ":9090"
			return c
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.cfg); err != nil {
				t.Errorf("cross-mode field should warn, not error: %v", err)
			}
		})
	}
}

func TestValidate_ProxySidecarRequiresBackend(t *testing.T) {
	cfg := validProxySidecarConfig()
	cfg.Listener.ReverseProxyBackend = ""
	if err := Validate(cfg); err == nil {
		t.Error("expected error for proxy-sidecar without backend")
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	for _, cfg := range []*Config{
		validEnvoySidecarConfig(),
		validWaypointConfig(),
		validProxySidecarConfig(),
	} {
		if err := Validate(cfg); err != nil {
			t.Errorf("unexpected error for mode %s: %v", cfg.Mode, err)
		}
	}
}

// --- Load Tests ---

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `mode: waypoint
inbound:
  jwks_url: "${TEST_JWKS_URL}"
  issuer: "http://issuer"
outbound:
  token_url: "http://token"
identity:
  type: client-secret
  client_id: "svc"
  client_secret: "secret"
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("TEST_JWKS_URL", "http://expanded-jwks")
	defer os.Unsetenv("TEST_JWKS_URL")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != ModeWaypoint {
		t.Errorf("mode = %q, want waypoint", cfg.Mode)
	}
	if cfg.Inbound.JWKSURL != "http://expanded-jwks" {
		t.Errorf("jwks_url = %q, want expanded value", cfg.Inbound.JWKSURL)
	}
}

// --- RouteConfig backwards compat ---

func TestRouteConfig_LegacyPassthrough(t *testing.T) {
	rc := RouteConfig{Host: "internal", Passthrough: true}
	// Simulate what resolve.go does
	action := rc.Action
	if action == "" && rc.Passthrough {
		action = "passthrough"
	}
	if action != "passthrough" {
		t.Errorf("expected passthrough, got %q", action)
	}
}

// --- Keycloak URL derivation ---

func TestDeriveKeycloakURLs(t *testing.T) {
	cfg := &Config{
		Mode:     ModeWaypoint,
		Outbound: OutboundConfig{KeycloakURL: "http://keycloak:8080", KeycloakRealm: "kagenti"},
		Identity: IdentityConfig{Type: "client-secret", ClientID: "svc", ClientSecret: "secret"},
	}
	deriveKeycloakURLs(cfg)
	if cfg.Outbound.TokenURL != "http://keycloak:8080/realms/kagenti/protocol/openid-connect/token" {
		t.Errorf("token_url = %q", cfg.Outbound.TokenURL)
	}
	if cfg.Inbound.Issuer != "http://keycloak:8080/realms/kagenti" {
		t.Errorf("issuer = %q", cfg.Inbound.Issuer)
	}
}

func TestDeriveKeycloakURLs_ExplicitTakesPrecedence(t *testing.T) {
	cfg := &Config{
		Inbound:  InboundConfig{Issuer: "http://explicit-issuer"},
		Outbound: OutboundConfig{TokenURL: "http://explicit-token", KeycloakURL: "http://keycloak:8080", KeycloakRealm: "kagenti"},
	}
	deriveKeycloakURLs(cfg)
	if cfg.Outbound.TokenURL != "http://explicit-token" {
		t.Errorf("explicit token_url should not be overridden, got %q", cfg.Outbound.TokenURL)
	}
	if cfg.Inbound.Issuer != "http://explicit-issuer" {
		t.Errorf("explicit issuer should not be overridden, got %q", cfg.Inbound.Issuer)
	}
}

// --- JWKS URL derivation ---

func TestDeriveJWKSURL(t *testing.T) {
	cfg := &Config{
		Outbound: OutboundConfig{TokenURL: "http://keycloak:8080/realms/kagenti/protocol/openid-connect/token"},
	}
	deriveJWKSURL(cfg)
	if cfg.Inbound.JWKSURL != "http://keycloak:8080/realms/kagenti/protocol/openid-connect/certs" {
		t.Errorf("jwks_url = %q", cfg.Inbound.JWKSURL)
	}
}

func TestDeriveJWKSURL_ExplicitTakesPrecedence(t *testing.T) {
	cfg := &Config{
		Inbound:  InboundConfig{JWKSURL: "http://explicit-jwks"},
		Outbound: OutboundConfig{TokenURL: "http://keycloak:8080/realms/kagenti/protocol/openid-connect/token"},
	}
	deriveJWKSURL(cfg)
	if cfg.Inbound.JWKSURL != "http://explicit-jwks" {
		t.Errorf("explicit jwks_url should not be overridden, got %q", cfg.Inbound.JWKSURL)
	}
}

// --- Credential file validation ---

func TestValidate_ClientIDFile(t *testing.T) {
	cfg := &Config{
		Mode:     ModeWaypoint,
		Inbound:  InboundConfig{JWKSURL: "http://jwks", Issuer: "http://issuer"},
		Outbound: OutboundConfig{TokenURL: "http://token"},
		Identity: IdentityConfig{Type: "client-secret", ClientIDFile: "/shared/client-id.txt", ClientSecretFile: "/shared/client-secret.txt"},
	}
	// Should pass validation — file paths accepted as alternatives
	if err := Validate(cfg); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- Credential file reading ---

func TestWaitAndReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client-id.txt")
	if err := os.WriteFile(path, []byte("  my-agent  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	var dest string
	if err := waitAndReadFile(path, &dest, 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest != "my-agent" {
		t.Errorf("got %q, want trimmed value", dest)
	}
}

func TestWaitForFile_Timeout(t *testing.T) {
	err := waitForFile("/nonexistent/file", 1*time.Second)
	if err == nil {
		t.Error("expected timeout error")
	}
}

// --- Helpers ---

func validEnvoySidecarConfig() *Config {
	return &Config{
		Mode:     ModeEnvoySidecar,
		Inbound:  InboundConfig{JWKSURL: "http://jwks", Issuer: "http://issuer"},
		Outbound: OutboundConfig{TokenURL: "http://token"},
		Identity: IdentityConfig{Type: "spiffe", JWTSVIDPath: "/opt/svid.token", ClientID: "agent"},
	}
}

func validWaypointConfig() *Config {
	return &Config{
		Mode:     ModeWaypoint,
		Inbound:  InboundConfig{JWKSURL: "http://jwks", Issuer: "http://issuer"},
		Outbound: OutboundConfig{TokenURL: "http://token"},
		Identity: IdentityConfig{Type: "client-secret", ClientID: "svc", ClientSecret: "secret"},
	}
}

func validProxySidecarConfig() *Config {
	return &Config{
		Mode:     ModeProxySidecar,
		Inbound:  InboundConfig{JWKSURL: "http://jwks", Issuer: "http://issuer"},
		Outbound: OutboundConfig{TokenURL: "http://token"},
		Identity: IdentityConfig{Type: "spiffe", JWTSVIDPath: "/opt/svid.token", ClientID: "agent"},
		Listener: ListenerConfig{ReverseProxyBackend: "http://localhost:8081"},
	}
}
