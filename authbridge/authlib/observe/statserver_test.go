package observe

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
)

func newTestConfig() *config.Config {
	return &config.Config{
		Mode: "proxy-sidecar",
		Inbound: config.InboundConfig{
			JWKSURL: "https://keycloak.example.com/certs",
			Issuer:  "https://keycloak.example.com/realms/test",
		},
		Outbound: config.OutboundConfig{
			TokenURL:      "https://keycloak.example.com/token",
			DefaultPolicy: "passthrough",
		},
		Identity: config.IdentityConfig{
			Type:             "client-secret",
			ClientID:         "my-agent",
			ClientSecret:     "super-secret-value",
			ClientIDFile:     "/shared/client-id.txt",
			ClientSecretFile: "/shared/client-secret.txt",
			SocketPath:       "/run/spire/sockets/agent.sock",
			JWTSVIDPath:      "/opt/jwt_svid.token",
			JWTAudience:      []string{"my-audience"},
		},
		Listener: config.ListenerConfig{
			ForwardProxyAddr:    ":15123",
			ReverseProxyAddr:    ":15124",
			ReverseProxyBackend: "http://localhost:8080",
		},
		Bypass: config.BypassConfig{
			InboundPaths: []string{"/health", "/ready"},
		},
		Routes: config.RoutesConfig{
			Rules: []config.RouteConfig{
				{Host: "target-svc", TargetAudience: "target", TokenScopes: "openid"},
			},
		},
		Stats: config.StatsConfig{
			StatsAddress: ":9093",
		},
	}
}

func serveMux(cfg *config.Config, stats *auth.Stats) http.Handler {
	s := NewStatServer(":0", cfg, stats)
	return s.server.Handler
}

func TestRootHandler(t *testing.T) {
	handler := serveMux(newTestConfig(), auth.NewStats())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	html := string(body)
	if !strings.Contains(html, "/config") {
		t.Error("root page missing /config link")
	}
	if !strings.Contains(html, "/stats") {
		t.Error("root page missing /stats link")
	}
}

func TestConfigEndpointRedactsSensitiveFields(t *testing.T) {
	cfg := newTestConfig()
	handler := serveMux(cfg, auth.NewStats())

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /config status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got config.Config
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode config response: %v", err)
	}

	if got.Identity.ClientSecret != "*redacted*" {
		t.Errorf("ClientSecret = %q, want *redacted*", got.Identity.ClientSecret)
	}
	if got.Identity.ClientIDFile != "*redacted*" {
		t.Errorf("ClientIDFile = %q, want *redacted*", got.Identity.ClientIDFile)
	}
	if got.Identity.ClientSecretFile != "*redacted*" {
		t.Errorf("ClientSecretFile = %q, want *redacted*", got.Identity.ClientSecretFile)
	}
	if got.Identity.SocketPath != "*redacted*" {
		t.Errorf("SocketPath = %q, want *redacted*", got.Identity.SocketPath)
	}
	if got.Identity.JWTSVIDPath != "*redacted*" {
		t.Errorf("JWTSVIDPath = %q, want *redacted*", got.Identity.JWTSVIDPath)
	}
}

func TestConfigEndpointPreservesNonSensitiveFields(t *testing.T) {
	cfg := newTestConfig()
	handler := serveMux(cfg, auth.NewStats())

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var got config.Config
	if err := json.NewDecoder(w.Result().Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Mode != cfg.Mode {
		t.Errorf("Mode = %q, want %q", got.Mode, cfg.Mode)
	}
	if got.Identity.Type != cfg.Identity.Type {
		t.Errorf("Identity.Type = %q, want %q", got.Identity.Type, cfg.Identity.Type)
	}
	if got.Identity.ClientID != cfg.Identity.ClientID {
		t.Errorf("Identity.ClientID = %q, want %q", got.Identity.ClientID, cfg.Identity.ClientID)
	}
	if got.Inbound.Issuer != cfg.Inbound.Issuer {
		t.Errorf("Inbound.Issuer = %q, want %q", got.Inbound.Issuer, cfg.Inbound.Issuer)
	}
	if got.Outbound.DefaultPolicy != cfg.Outbound.DefaultPolicy {
		t.Errorf("Outbound.DefaultPolicy = %q, want %q", got.Outbound.DefaultPolicy, cfg.Outbound.DefaultPolicy)
	}
	if got.Stats.StatsAddress != cfg.Stats.StatsAddress {
		t.Errorf("Stats.StatsAddress = %q, want %q", got.Stats.StatsAddress, cfg.Stats.StatsAddress)
	}
}

func TestStatsEndpointEmptyStats(t *testing.T) {
	stats := auth.NewStats()
	handler := serveMux(newTestConfig(), stats)

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /stats status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got map[string]map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, key := range []string{"inbound_approvals", "inbound_denials", "outbound_approvals", "outbound_denials"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q in stats response", key)
		}
	}
}

func TestStatsEndpointWithCounts(t *testing.T) {
	stats := auth.NewStats()
	handler := serveMux(newTestConfig(), stats)

	// Stats are exported from auth package — the Stats fields are unexported,
	// but we can exercise the JSON output by marshalling through the endpoint.
	// First, verify we get valid JSON back even from a fresh Stats object.
	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body, _ := io.ReadAll(w.Result().Body)
	if !json.Valid(body) {
		t.Fatalf("stats endpoint returned invalid JSON: %s", body)
	}
}

func TestNewStatServerSetsAddr(t *testing.T) {
	s := NewStatServer(":9093", newTestConfig(), auth.NewStats())
	if s.server.Addr != ":9093" {
		t.Errorf("server.Addr = %q, want :9093", s.server.Addr)
	}
}

func TestNewStatServerCustomAddr(t *testing.T) {
	s := NewStatServer("127.0.0.1:8888", newTestConfig(), auth.NewStats())
	if s.server.Addr != "127.0.0.1:8888" {
		t.Errorf("server.Addr = %q, want 127.0.0.1:8888", s.server.Addr)
	}
}

func TestConfigEndpointPreservesRoutes(t *testing.T) {
	cfg := newTestConfig()
	handler := serveMux(cfg, auth.NewStats())

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var got config.Config
	if err := json.NewDecoder(w.Result().Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Routes.Rules) != 1 {
		t.Fatalf("Routes.Rules length = %d, want 1", len(got.Routes.Rules))
	}
	if got.Routes.Rules[0].Host != "target-svc" {
		t.Errorf("Routes.Rules[0].Host = %q, want target-svc", got.Routes.Rules[0].Host)
	}
}

func TestConfigEndpointPreservesBypass(t *testing.T) {
	cfg := newTestConfig()
	handler := serveMux(cfg, auth.NewStats())

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var got config.Config
	if err := json.NewDecoder(w.Result().Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Bypass.InboundPaths) != 2 {
		t.Fatalf("Bypass.InboundPaths length = %d, want 2", len(got.Bypass.InboundPaths))
	}
}

func TestConfigEndpointPreservesJWTAudience(t *testing.T) {
	cfg := newTestConfig()
	handler := serveMux(cfg, auth.NewStats())

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var got config.Config
	if err := json.NewDecoder(w.Result().Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Identity.JWTAudience) != 1 || got.Identity.JWTAudience[0] != "my-audience" {
		t.Errorf("Identity.JWTAudience = %v, want [my-audience]", got.Identity.JWTAudience)
	}
}
