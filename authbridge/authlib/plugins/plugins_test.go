package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// TestAuthbridgeCombinedYAML_Loads asserts that the in-repo default
// config consumed by the combined sidecar image
// (authbridge/authproxy/authbridge-combined.yaml) parses, env-expands,
// and produces working pipelines. Since that YAML leans on per-plugin
// defaults for file paths and bypass patterns, a future rename of any
// default constant would silently break the shipped image unless this
// test fails. It's cheaper to fail in CI than in a running pod.
func TestAuthbridgeCombinedYAML_Loads(t *testing.T) {
	// The canonical file path is relative to this test file —
	// plugins_test.go lives in authlib/plugins/, the YAML in
	// authproxy/. Go up two directories, across into authproxy/.
	yamlPath := filepath.Join("..", "..", "authproxy", "authbridge-combined.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Skipf("authbridge-combined.yaml not found (repo layout changed?): %v", err)
	}

	envs := map[string]string{
		"ISSUER":                  "http://keycloak.localtest.me:8080/realms/kagenti",
		"KEYCLOAK_URL":            "http://keycloak-service.keycloak.svc:8080",
		"KEYCLOAK_REALM":          "kagenti",
		"DEFAULT_OUTBOUND_POLICY": "passthrough",
		"TOKEN_URL":               "", // intentionally empty: the plugin should derive from keycloak_url + realm
	}
	for k, v := range envs {
		t.Setenv(k, v)
	}

	cfg, err := config.Load(yamlPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", yamlPath, err)
	}
	if cfg.Mode != config.ModeEnvoySidecar {
		t.Errorf("mode = %q, want %q", cfg.Mode, config.ModeEnvoySidecar)
	}
	if err := config.Validate(cfg); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// Build both pipelines. Any plugin whose Configure rejects the
	// env-expanded config subtree (e.g. because a default path moved
	// but the YAML still relies on it) fails the build here.
	if _, err := Build(cfg.Pipeline.Inbound.Plugins); err != nil {
		t.Errorf("Build inbound: %v", err)
	}
	if _, err := Build(cfg.Pipeline.Outbound.Plugins); err != nil {
		t.Errorf("Build outbound: %v", err)
	}
}

// --- JWTValidation: Configure ---

func TestJWTValidation_Configure_MissingIssuer(t *testing.T) {
	p := NewJWTValidation()
	err := p.Configure([]byte(`{}`))
	if err == nil {
		t.Fatal("expected error for missing issuer")
	}
}

func TestJWTValidation_Configure_UnknownField(t *testing.T) {
	p := NewJWTValidation()
	err := p.Configure([]byte(`{"issuer":"http://ex","audience":"a","not_a_field":"x"}`))
	if err == nil {
		t.Fatal("expected error for unknown field; DisallowUnknownFields should reject")
	}
}

// Legacy test obsolete: applyDefaults now sets audience_file to
// /shared/client-id.txt when neither audience nor audience_file is
// supplied, so this scenario no longer reaches validate(). The
// replacement test is TestJWTValidation_Configure_DefaultAudienceFile.

func TestJWTValidation_Configure_PerHost(t *testing.T) {
	p := NewJWTValidation()
	// per-host mode does not require an audience field.
	err := p.Configure([]byte(`{"issuer":"http://ex","audience_mode":"per-host"}`))
	if err != nil {
		t.Fatalf("per-host mode should not require audience: %v", err)
	}
	if p.audienceDeriver == nil {
		t.Error("per-host mode should set audienceDeriver")
	}
}

// When neither audience nor audience_file is supplied, the plugin
// defaults audience_file to /shared/client-id.txt (the Kagenti
// client-registration convention). Omitting both in the YAML must not
// fail validation — the file read is best-effort with a background
// fallback poll.
func TestJWTValidation_Configure_DefaultAudienceFile(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex"}`)); err != nil {
		t.Fatalf("Configure with defaults: %v", err)
	}
	if p.cfg.AudienceFile != "/shared/client-id.txt" {
		t.Errorf("AudienceFile = %q, want /shared/client-id.txt", p.cfg.AudienceFile)
	}
}

// bypass_paths defaults to bypass.DefaultPatterns so health / .well-known
// endpoints don't reject every JWT-less probe from kubelet.
func TestJWTValidation_Configure_DefaultBypassPaths(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience":"a"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(p.cfg.BypassPaths) == 0 {
		t.Fatal("expected default bypass paths")
	}
}

// Inline audience suppresses the audience_file default: operators who
// supply a literal audience must not also get a surprise file read.
func TestJWTValidation_Configure_InlineAudienceSuppressesFileDefault(t *testing.T) {
	p := NewJWTValidation()
	if err := p.Configure([]byte(`{"issuer":"http://ex","audience":"literal"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.AudienceFile != "" {
		t.Errorf("AudienceFile = %q, want empty (inline audience should suppress default)", p.cfg.AudienceFile)
	}
}

func TestJWTValidation_Configure_DefaultsJWKSFromIssuer(t *testing.T) {
	p := NewJWTValidation()
	err := p.Configure([]byte(`{"issuer":"http://keycloak/realms/kagenti","audience":"a"}`))
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// The derived JWKS URL is applied during Configure — we can't
	// inspect it directly because it's buried inside the verifier, but
	// if the inner auth handler is nil we know Configure bailed.
	if p.inner == nil {
		t.Fatal("Configure produced no inner auth handler")
	}
}

func TestJWTValidation_Configure_AudienceFromFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "aud")
	if err := os.WriteFile(f, []byte("my-agent"), 0600); err != nil {
		t.Fatal(err)
	}
	p := NewJWTValidation()
	raw := []byte(`{"issuer":"http://ex","audience_file":"` + f + `"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !p.inner.Ready() {
		t.Error("expected inner.Ready() == true after synchronous audience load")
	}
}

// --- JWTValidation: OnRequest ---

func TestJWTValidation_OnRequest_NotConfigured(t *testing.T) {
	p := NewJWTValidation()
	action := p.OnRequest(context.Background(), &pipeline.Context{Headers: http.Header{}})
	if action.Type != pipeline.Reject {
		t.Errorf("got %v, want Reject for unconfigured plugin", action.Type)
	}
}

// --- TokenExchange: Configure ---

func TestTokenExchange_Configure_MissingTokenURL(t *testing.T) {
	p := NewTokenExchange()
	err := p.Configure([]byte(`{"identity":{"type":"client-secret","client_id":"c","client_secret":"s"}}`))
	if err == nil {
		t.Fatal("expected error for missing token_url")
	}
}

func TestTokenExchange_Configure_DerivesTokenURL(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "keycloak_url":"http://keycloak:8080",
	  "keycloak_realm":"kagenti",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	want := "http://keycloak:8080/realms/kagenti/protocol/openid-connect/token"
	if p.cfg.TokenURL != want {
		t.Errorf("token_url = %q, want %q", p.cfg.TokenURL, want)
	}
}

// Identity file paths default to Kagenti conventions when the operator
// doesn't supply them. Inline values suppress the default.
func TestTokenExchange_Configure_DefaultIdentityPaths_SPIFFE(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"spiffe"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Identity.ClientIDFile != "/shared/client-id.txt" {
		t.Errorf("ClientIDFile = %q, want /shared/client-id.txt", p.cfg.Identity.ClientIDFile)
	}
	if p.cfg.Identity.JWTSVIDPath != "/opt/jwt_svid.token" {
		t.Errorf("JWTSVIDPath = %q, want /opt/jwt_svid.token", p.cfg.Identity.JWTSVIDPath)
	}
}

func TestTokenExchange_Configure_DefaultIdentityPaths_ClientSecret(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"client-secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Identity.ClientIDFile != "/shared/client-id.txt" {
		t.Errorf("ClientIDFile = %q, want /shared/client-id.txt", p.cfg.Identity.ClientIDFile)
	}
	if p.cfg.Identity.ClientSecretFile != "/shared/client-secret.txt" {
		t.Errorf("ClientSecretFile = %q, want /shared/client-secret.txt", p.cfg.Identity.ClientSecretFile)
	}
}

// Inline identity values must suppress the file defaults, otherwise an
// operator who writes inline credentials could be silently overridden
// by a pre-existing file on the mount point.
func TestTokenExchange_Configure_InlineIdentitySuppressesFileDefaults(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Identity.ClientIDFile != "" {
		t.Errorf("ClientIDFile = %q, want empty", p.cfg.Identity.ClientIDFile)
	}
	if p.cfg.Identity.ClientSecretFile != "" {
		t.Errorf("ClientSecretFile = %q, want empty", p.cfg.Identity.ClientSecretFile)
	}
}

func TestTokenExchange_Configure_DefaultRoutesFile(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://t",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Routes.File != "/etc/authproxy/routes.yaml" {
		t.Errorf("Routes.File = %q, want /etc/authproxy/routes.yaml", p.cfg.Routes.File)
	}
}

func TestTokenExchange_Configure_DefaultsPassthrough(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://token",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.DefaultPolicy != "passthrough" {
		t.Errorf("default_policy = %q, want passthrough", p.cfg.DefaultPolicy)
	}
}

func TestTokenExchange_Configure_InvalidDefaultPolicy(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://token",
	  "default_policy":"nope",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err == nil {
		t.Fatal("expected error for invalid default_policy")
	}
}

// Identity type is still required — defaulting covers the *paths* to
// credential files, not the choice between SPIFFE and client-secret.
// Unknown types fall through to the default error branch.
func TestTokenExchange_Configure_IdentityValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"type missing", `{"token_url":"http://t"}`},
		{"type unknown", `{"token_url":"http://t","identity":{"type":"whatever"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewTokenExchange()
			if err := p.Configure([]byte(c.raw)); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// --- TokenExchange: OnRequest (end-to-end through Configure) ---

func TestTokenExchange_Passthrough(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://unused",
	  "default_policy":"passthrough",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "some-host",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("got %v, want Continue", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "Bearer user-token" {
		t.Error("headers should not be modified for passthrough")
	}
}

func TestTokenExchange_ExchangeSuccess(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-token",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer exchangeSrv.Close()

	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"` + exchangeSrv.URL + `",
	  "default_policy":"exchange",
	  "identity":{"type":"client-secret","client_id":"agent","client_secret":"secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("got %v, want Continue", action.Type)
	}
	if pctx.Headers.Get("Authorization") != "Bearer new-token" {
		t.Errorf("token = %q, want Bearer new-token", pctx.Headers.Get("Authorization"))
	}
}

func TestTokenExchange_ExchangeFailure(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer exchangeSrv.Close()

	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"` + exchangeSrv.URL + `",
	  "default_policy":"exchange",
	  "identity":{"type":"client-secret","client_id":"agent","client_secret":"secret"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	status, _, _ := action.Violation.Render()
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
}

func TestTokenExchange_NoToken_Deny(t *testing.T) {
	p := NewTokenExchange()
	raw := []byte(`{
	  "token_url":"http://unused",
	  "default_policy":"exchange",
	  "no_token_policy":"deny",
	  "identity":{"type":"client-secret","client_id":"c","client_secret":"s"}
	}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{},
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	status, _, _ := action.Violation.Render()
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

// --- Registry / Build ---

func TestBuild_ValidNames(t *testing.T) {
	p, err := Build([]config.PluginEntry{
		{Name: "a2a-parser"},
		{Name: "mcp-parser"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil pipeline")
	}
}

func TestBuild_UnknownName(t *testing.T) {
	_, err := Build([]config.PluginEntry{{Name: "nonexistent-plugin"}})
	if err == nil {
		t.Fatal("expected error for unknown plugin name")
	}
}

func TestBuild_EmptyList(t *testing.T) {
	p, err := Build([]config.PluginEntry{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	action := p.Run(context.Background(), &pipeline.Context{Headers: http.Header{}})
	if action.Type != pipeline.Continue {
		t.Errorf("empty pipeline got %v, want Continue", action.Type)
	}
}

// A config: block on a plugin that doesn't implement Configurable is a
// startup error. Silent acceptance would hide typos (wrong plugin name)
// and stale config across refactors.
func TestBuild_ConfigForNonConfigurablePlugin(t *testing.T) {
	_, err := Build([]config.PluginEntry{
		{Name: "a2a-parser", Config: []byte(`{"unused":true}`)},
	})
	if err == nil {
		t.Fatal("expected error for config on non-Configurable plugin")
	}
	// Error text is operator-facing contract — a future refactor that
	// changes it must update this assertion intentionally.
	if !strings.Contains(err.Error(), "does not accept configuration") {
		t.Errorf("error %q does not match the operator-facing contract "+
			`"%q does not accept configuration"`, err, "a2a-parser")
	}
}

// Configure errors surface through Build with the offending plugin's
// name so startup logs identify the broken entry without the operator
// having to read every plugin's error wording.
func TestBuild_ConfigureError(t *testing.T) {
	_, err := Build([]config.PluginEntry{
		{Name: "jwt-validation", Config: []byte(`{}`)}, // missing issuer
	})
	if err == nil {
		t.Fatal("expected error for invalid jwt-validation config")
	}
	if !strings.Contains(err.Error(), "jwt-validation") {
		t.Errorf("error %q does not name the offending plugin", err)
	}
}
