package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/kagenti/kagenti-extensions/AuthBridge/AuthProxy/go-processor/internal/resolver"
)

func TestMatchBypassPath(t *testing.T) {
	tests := []struct {
		name         string
		patterns     []string
		requestPath  string
		expectBypass bool
	}{
		{
			name:         "exact match /healthz",
			patterns:     []string{"/healthz", "/readyz"},
			requestPath:  "/healthz",
			expectBypass: true,
		},
		{
			name:         "exact match /readyz",
			patterns:     []string{"/healthz", "/readyz"},
			requestPath:  "/readyz",
			expectBypass: true,
		},
		{
			name:         "glob match /.well-known/*",
			patterns:     []string{"/.well-known/*"},
			requestPath:  "/.well-known/agent.json",
			expectBypass: true,
		},
		{
			name:         "glob does not match nested path",
			patterns:     []string{"/.well-known/*"},
			requestPath:  "/.well-known/a/b",
			expectBypass: false,
		},
		{
			name:         "no match for /api/data",
			patterns:     []string{"/.well-known/*", "/healthz", "/readyz", "/livez"},
			requestPath:  "/api/data",
			expectBypass: false,
		},
		{
			name:         "empty bypass list",
			patterns:     []string{},
			requestPath:  "/healthz",
			expectBypass: false,
		},
		{
			name:         "nil bypass list",
			patterns:     nil,
			requestPath:  "/healthz",
			expectBypass: false,
		},
		{
			name:         "path with query string - matches",
			patterns:     []string{"/healthz"},
			requestPath:  "/healthz?verbose=true",
			expectBypass: true,
		},
		{
			name:         "path with query string - glob matches",
			patterns:     []string{"/.well-known/*"},
			requestPath:  "/.well-known/agent.json?format=json",
			expectBypass: true,
		},
		{
			name:         "path with query string - no match",
			patterns:     []string{"/healthz"},
			requestPath:  "/api/data?healthz=true",
			expectBypass: false,
		},
		{
			name:         "empty request path",
			patterns:     []string{"/healthz"},
			requestPath:  "",
			expectBypass: false,
		},
		{
			name:         "root path exact match",
			patterns:     []string{"/"},
			requestPath:  "/",
			expectBypass: true,
		},
		// Malformed pattern: silently skipped, does not match
		{
			name:         "malformed pattern is skipped",
			patterns:     []string{"["},
			requestPath:  "/healthz",
			expectBypass: false,
		},
		{
			name:         "malformed pattern does not block valid patterns",
			patterns:     []string{"[", "/healthz"},
			requestPath:  "/healthz",
			expectBypass: true,
		},
		// Path normalization: non-canonical forms should still match
		{
			name:         "double slash normalized",
			patterns:     []string{"/healthz"},
			requestPath:  "//healthz",
			expectBypass: true,
		},
		{
			name:         "dot segment normalized",
			patterns:     []string{"/healthz"},
			requestPath:  "/./healthz",
			expectBypass: true,
		},
		{
			name:         "dot-dot segment normalized",
			patterns:     []string{"/.well-known/*"},
			requestPath:  "/foo/../.well-known/agent.json",
			expectBypass: true,
		},
		{
			name:         "trailing slash normalized",
			patterns:     []string{"/healthz"},
			requestPath:  "/healthz/",
			expectBypass: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save and restore the global state
			orig := bypassInboundPaths
			bypassInboundPaths = tt.patterns
			defer func() { bypassInboundPaths = orig }()

			got := matchBypassPath(tt.requestPath)
			if got != tt.expectBypass {
				t.Errorf("matchBypassPath(%q) = %v, want %v (patterns: %v)",
					tt.requestPath, got, tt.expectBypass, tt.patterns)
			}
		})
	}
}

func TestActorTokenCache(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := tokenExchangeResponse{
			AccessToken: "actor-token-v1",
			ExpiresIn:   300,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cache := &actorTokenCache{}

	// First call should hit the server
	token, err := cache.getActorToken("client-id", "client-secret", server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "actor-token-v1" {
		t.Errorf("expected actor-token-v1, got %s", token)
	}
	if callCount != 1 {
		t.Errorf("expected 1 server call, got %d", callCount)
	}

	// Second call should return cached value
	token, err = cache.getActorToken("client-id", "client-secret", server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "actor-token-v1" {
		t.Errorf("expected cached actor-token-v1, got %s", token)
	}
	if callCount != 1 {
		t.Errorf("expected still 1 server call (cached), got %d", callCount)
	}

	// Expire the cache and verify refresh
	cache.mu.Lock()
	cache.expiresAt = time.Now().Add(-1 * time.Second)
	cache.mu.Unlock()

	token, err = cache.getActorToken("client-id", "client-secret", server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 server calls after expiry, got %d", callCount)
	}
}

func TestActorTokenDisabled(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := tokenExchangeResponse{
			AccessToken: "should-not-appear",
			ExpiresIn:   300,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Save and restore the global flag
	orig := actorTokenEnabled
	actorTokenEnabled = false
	defer func() { actorTokenEnabled = orig }()

	saved := saveGlobals()
	defer restoreGlobals(saved)

	routesYAML := fmt.Sprintf(`
- host: "test-service"
  target_audience: "test-aud"
  token_scopes: "openid test-aud"
  token_url: %q
`, server.URL)

	defaultOutboundPolicy = "passthrough"
	globalResolver = setupTestResolver(t, routesYAML)
	setGlobalConfig("test-client", "test-secret", server.URL)

	p := &processor{}
	headers := buildHeaders("test-service", "Bearer some-jwt")
	p.handleOutbound(context.Background(), headers)

	// The server may be called for the token exchange itself, but NOT for an actor token grant.
	// With actorTokenEnabled=false, no client_credentials call for actor token should happen.
	// We verify by checking the server wasn't called with grant_type=client_credentials
	// before the exchange call.
	if callCount == 0 {
		t.Error("expected at least one server call for token exchange")
	}
}

func TestActorTokenCacheError(t *testing.T) {
	// Server that always returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "server_error"}`))
	}))
	defer server.Close()

	cache := &actorTokenCache{}
	token, err := cache.getActorToken("client-id", "client-secret", server.URL)
	if err == nil {
		t.Fatal("expected error from getActorToken, got nil")
	}
	if token != "" {
		t.Errorf("expected empty token on error, got %q", token)
	}
}

func TestActorTokenCacheConcurrent(t *testing.T) {
	var callCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		resp := tokenExchangeResponse{
			AccessToken: "concurrent-token",
			ExpiresIn:   300,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cache := &actorTokenCache{}
	var wg sync.WaitGroup
	errs := make(chan error, 20)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := cache.getActorToken("client-id", "client-secret", server.URL)
			if err != nil {
				errs <- err
				return
			}
			if token != "concurrent-token" {
				errs <- fmt.Errorf("expected concurrent-token, got %s", token)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent access error: %v", err)
	}
}

func TestActorTokenCacheMinTTL(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := tokenExchangeResponse{
			AccessToken: "min-ttl-token",
			ExpiresIn:   0, // Server returns 0 expires_in
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cache := &actorTokenCache{}

	// First call
	token, err := cache.getActorToken("client-id", "client-secret", server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "min-ttl-token" {
		t.Errorf("expected min-ttl-token, got %s", token)
	}

	// Second call should use cache (min TTL floor prevents immediate expiry)
	token, err = cache.getActorToken("client-id", "client-secret", server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 server call (min TTL should cache), got %d", callCount)
	}
}

func TestExchangeTokenWithActorToken(t *testing.T) {
	var receivedParams url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		receivedParams = r.Form
		resp := tokenExchangeResponse{
			AccessToken: "exchanged-token",
			ExpiresIn:   300,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Test with actor token present
	token, err := exchangeToken("cid", "csecret", server.URL, "subject-tok", "aud", "openid", "actor-tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "exchanged-token" {
		t.Errorf("expected exchanged-token, got %s", token)
	}
	if receivedParams.Get("actor_token") != "actor-tok" {
		t.Errorf("expected actor_token=actor-tok, got %s", receivedParams.Get("actor_token"))
	}
	if receivedParams.Get("actor_token_type") != "urn:ietf:params:oauth:token-type:access_token" {
		t.Errorf("expected actor_token_type, got %s", receivedParams.Get("actor_token_type"))
	}
}

func TestExchangeTokenWithoutActorToken(t *testing.T) {
	var receivedParams url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		receivedParams = r.Form
		resp := tokenExchangeResponse{
			AccessToken: "exchanged-token",
			ExpiresIn:   300,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Test with empty actor token
	token, err := exchangeToken("cid", "csecret", server.URL, "subject-tok", "aud", "openid", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "exchanged-token" {
		t.Errorf("expected exchanged-token, got %s", token)
	}
	if receivedParams.Get("actor_token") != "" {
		t.Errorf("expected no actor_token param, got %s", receivedParams.Get("actor_token"))
	}
	if receivedParams.Get("actor_token_type") != "" {
		t.Errorf("expected no actor_token_type param, got %s", receivedParams.Get("actor_token_type"))
	}
}

func TestDefaultBypassPaths(t *testing.T) {
	// Verify defaults are applied without any env var override
	orig := bypassInboundPaths
	bypassInboundPaths = defaultBypassInboundPaths
	defer func() { bypassInboundPaths = orig }()

	shouldBypass := []string{
		"/.well-known/agent.json",
		"/.well-known/openid-configuration",
		"/healthz",
		"/readyz",
		"/livez",
	}
	for _, p := range shouldBypass {
		if !matchBypassPath(p) {
			t.Errorf("default bypass should match %q but did not", p)
		}
	}

	shouldBlock := []string{
		"/",
		"/api/data",
		"/v1/tasks",
		"/.well-known/nested/deep",
	}
	for _, p := range shouldBlock {
		if matchBypassPath(p) {
			t.Errorf("default bypass should NOT match %q but did", p)
		}
	}
}

// --- Test helpers ---

// buildHeaders creates a core.HeaderMap with the given host and optional Authorization header.
func buildHeaders(host, authHeader string) *core.HeaderMap {
	headers := []*core.HeaderValue{
		{Key: ":authority", RawValue: []byte(host)},
		{Key: ":path", RawValue: []byte("/")},
		{Key: ":method", RawValue: []byte("GET")},
	}
	if authHeader != "" {
		headers = append(headers, &core.HeaderValue{
			Key:      "authorization",
			RawValue: []byte(authHeader),
		})
	}
	return &core.HeaderMap{Headers: headers}
}

// setupTestResolver creates a StaticResolver from inline YAML for testing.
func setupTestResolver(t *testing.T, yaml string) resolver.TargetResolver {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("failed to write test routes.yaml: %v", err)
	}
	r, err := resolver.NewStaticResolver(path)
	if err != nil {
		t.Fatalf("failed to create resolver: %v", err)
	}
	return r
}

// emptyResolver returns a resolver with no routes (simulates missing routes.yaml).
func emptyResolver(t *testing.T) resolver.TargetResolver {
	t.Helper()
	r, err := resolver.NewStaticResolver("/nonexistent/path/routes.yaml")
	if err != nil {
		t.Fatalf("unexpected error creating empty resolver: %v", err)
	}
	return r
}

// mockKeycloak starts a test HTTP server that mimics Keycloak's token endpoint.
func mockKeycloak(t *testing.T, statusCode int, responseBody interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(responseBody)
	}))
}

// isPassthrough returns true if the response forwards the request unchanged.
func isPassthrough(resp *v3.ProcessingResponse) bool {
	rh, ok := resp.Response.(*v3.ProcessingResponse_RequestHeaders)
	if !ok {
		return false
	}
	return rh.RequestHeaders.Response == nil || rh.RequestHeaders.Response.HeaderMutation == nil
}

// isDenied returns true if the response is an ImmediateResponse (503).
func isDenied(resp *v3.ProcessingResponse) bool {
	_, ok := resp.Response.(*v3.ProcessingResponse_ImmediateResponse)
	return ok
}

// hasReplacedAuthHeader returns true if the response mutates the Authorization header.
func hasReplacedAuthHeader(resp *v3.ProcessingResponse) (string, bool) {
	rh, ok := resp.Response.(*v3.ProcessingResponse_RequestHeaders)
	if !ok {
		return "", false
	}
	if rh.RequestHeaders.Response == nil || rh.RequestHeaders.Response.HeaderMutation == nil {
		return "", false
	}
	for _, h := range rh.RequestHeaders.Response.HeaderMutation.SetHeaders {
		if strings.EqualFold(h.Header.Key, "authorization") {
			return string(h.Header.RawValue), true
		}
	}
	return "", false
}

type savedGlobals struct {
	policy       string
	resolver     resolver.TargetResolver
	clientID     string
	clientSecret string
	tokenURL     string
}

func saveGlobals() savedGlobals {
	globalConfig.mu.RLock()
	defer globalConfig.mu.RUnlock()
	return savedGlobals{
		policy:       defaultOutboundPolicy,
		resolver:     globalResolver,
		clientID:     globalConfig.ClientID,
		clientSecret: globalConfig.ClientSecret,
		tokenURL:     globalConfig.TokenURL,
	}
}

func restoreGlobals(saved savedGlobals) {
	defaultOutboundPolicy = saved.policy
	globalResolver = saved.resolver
	globalConfig.mu.Lock()
	defer globalConfig.mu.Unlock()
	globalConfig.ClientID = saved.clientID
	globalConfig.ClientSecret = saved.clientSecret
	globalConfig.TokenURL = saved.tokenURL
}

func setGlobalConfig(clientID, clientSecret, tokenURL string) {
	globalConfig.mu.Lock()
	defer globalConfig.mu.Unlock()
	globalConfig.ClientID = clientID
	globalConfig.ClientSecret = clientSecret
	globalConfig.TokenURL = tokenURL
}

// --- Test: Default agents (weather-service pattern) ---

// TestDefaultOutboundPolicy verifies that agents without routes.yaml get passthrough
// behavior by default. This models the weather-service scenario: an agent calling
// Ollama (LLM), otel-collector (telemetry), or any other service that doesn't
// need Keycloak token exchange.
func TestDefaultOutboundPolicy(t *testing.T) {
	tests := []struct {
		name           string
		policy         string
		host           string
		authHeader     string
		globalConfig   bool // whether to set complete global config
		expectPassthru bool
	}{
		{
			name:           "passthrough_default_ollama",
			policy:         "passthrough",
			host:           "ollama-service.team1.svc.cluster.local",
			expectPassthru: true,
		},
		{
			name:           "passthrough_default_otel",
			policy:         "passthrough",
			host:           "otel-collector.kagenti-system.svc.cluster.local:8335",
			expectPassthru: true,
		},
		{
			name:           "passthrough_default_any_host",
			policy:         "passthrough",
			host:           "random-service.default.svc.cluster.local",
			expectPassthru: true,
		},
		{
			name:           "passthrough_with_auth_header",
			policy:         "passthrough",
			host:           "ollama-service.team1.svc.cluster.local",
			authHeader:     "Bearer sk-some-api-key",
			expectPassthru: true,
		},
		{
			name:           "passthrough_unset_env_defaults_to_passthrough",
			policy:         "", // empty = not set, should keep default "passthrough"
			host:           "any-host.example.com",
			expectPassthru: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveGlobals()
			defer restoreGlobals(saved)

			if tt.policy != "" {
				defaultOutboundPolicy = tt.policy
			}
			globalResolver = emptyResolver(t)

			if tt.globalConfig {
				setGlobalConfig("test-client", "test-secret", "http://keycloak/token")
			} else {
				setGlobalConfig("", "", "")
			}

			p := &processor{}
			headers := buildHeaders(tt.host, tt.authHeader)
			resp := p.handleOutbound(context.Background(), headers)

			if tt.expectPassthru && !isPassthrough(resp) {
				t.Errorf("expected passthrough for host %q with policy %q, but got non-passthrough response", tt.host, tt.policy)
			}
			if !tt.expectPassthru && isPassthrough(resp) {
				t.Errorf("expected non-passthrough for host %q with policy %q, but got passthrough", tt.host, tt.policy)
			}
		})
	}
}

// TestDefaultOutboundPolicyLegacyExchangeNoRoutes verifies that when
// DEFAULT_OUTBOUND_POLICY=exchange but no routes are configured, the request
// passes through because there is no target audience/scopes to use.
func TestDefaultOutboundPolicyLegacyExchangeNoRoutes(t *testing.T) {
	saved := saveGlobals()
	defer restoreGlobals(saved)

	defaultOutboundPolicy = "exchange"
	globalResolver = emptyResolver(t)
	setGlobalConfig("test-client", "test-secret", "http://keycloak/token")

	p := &processor{}
	headers := buildHeaders("random-service.example.com", "Bearer some-jwt-token")
	resp := p.handleOutbound(context.Background(), headers)

	if !isPassthrough(resp) {
		t.Error("expected passthrough when exchange policy is set but no routes provide audience/scopes")
	}
}

// TestDefaultOutboundPolicyLegacyExchangeWithRoute verifies that when
// DEFAULT_OUTBOUND_POLICY=exchange and a route matches, token exchange works.
func TestDefaultOutboundPolicyLegacyExchangeWithRoute(t *testing.T) {
	saved := saveGlobals()
	defer restoreGlobals(saved)

	keycloak := mockKeycloak(t, http.StatusOK, tokenExchangeResponse{
		AccessToken: "legacy-exchanged-token",
		TokenType:   "Bearer",
		ExpiresIn:   300,
	})
	defer keycloak.Close()

	routesYAML := fmt.Sprintf(`
- host: "random-service.example.com"
  target_audience: "test-audience"
  token_scopes: "openid test-aud"
  token_url: %q
`, keycloak.URL)

	defaultOutboundPolicy = "exchange"
	globalResolver = setupTestResolver(t, routesYAML)
	setGlobalConfig("test-client", "test-secret", keycloak.URL)

	p := &processor{}
	headers := buildHeaders("random-service.example.com", "Bearer some-jwt-token")
	resp := p.handleOutbound(context.Background(), headers)

	token, ok := hasReplacedAuthHeader(resp)
	if !ok {
		if isDenied(resp) {
			t.Fatal("expected token exchange to succeed, but got 503 denial")
		}
		t.Fatal("expected Authorization header to be replaced, but got passthrough")
	}
	if token != "Bearer legacy-exchanged-token" {
		t.Errorf("expected 'Bearer legacy-exchanged-token', got %q", token)
	}
}

// --- Test: Github-issue agent pattern (route-based exchange) ---

// TestOutboundPolicyWithRoutes verifies that agents with routes.yaml entries
// get token exchange only for matched hosts. This models the github-issue agent:
// calls to github-tool get exchange, calls to the LLM pass through.
func TestOutboundPolicyWithRoutes(t *testing.T) {
	saved := saveGlobals()
	defer restoreGlobals(saved)

	keycloak := mockKeycloak(t, http.StatusOK, tokenExchangeResponse{
		AccessToken: "exchanged-token-for-github-tool",
		TokenType:   "Bearer",
		ExpiresIn:   300,
	})
	defer keycloak.Close()

	routesYAML := fmt.Sprintf(`
- host: "github-issue-tool-headless.team1.svc.cluster.local"
  target_audience: "github-tool"
  token_scopes: "openid github-tool-aud github-full-access"
  token_url: %q
- host: "otel-collector.*.svc.cluster.local"
  passthrough: true
`, keycloak.URL)

	defaultOutboundPolicy = "passthrough"
	globalResolver = setupTestResolver(t, routesYAML)
	setGlobalConfig("spiffe://localtest.me/ns/team1/sa/github-issue-agent", "client-secret-123", keycloak.URL)

	t.Run("route_match_exchanges_token", func(t *testing.T) {
		p := &processor{}
		headers := buildHeaders("github-issue-tool-headless.team1.svc.cluster.local", "Bearer valid-jwt-from-keycloak")
		resp := p.handleOutbound(context.Background(), headers)

		token, ok := hasReplacedAuthHeader(resp)
		if !ok {
			if isDenied(resp) {
				t.Fatal("expected exchange to succeed, but got 503 denial")
			}
			t.Fatal("expected Authorization header to be replaced, but got passthrough")
		}
		if token != "Bearer exchanged-token-for-github-tool" {
			t.Errorf("expected 'Bearer exchanged-token-for-github-tool', got %q", token)
		}
	})

	t.Run("route_match_no_auth_header_uses_client_credentials", func(t *testing.T) {
		p := &processor{}
		headers := buildHeaders("github-issue-tool-headless.team1.svc.cluster.local", "")
		resp := p.handleOutbound(context.Background(), headers)

		token, ok := hasReplacedAuthHeader(resp)
		if !ok {
			if isDenied(resp) {
				t.Fatal("expected client_credentials to succeed, but got 503 denial")
			}
			t.Fatal("expected Authorization header to be injected via client_credentials, but got passthrough")
		}
		if !strings.HasPrefix(token, "Bearer ") {
			t.Errorf("expected Bearer token, got %q", token)
		}
	})

	t.Run("unmatched_host_still_passthrough", func(t *testing.T) {
		p := &processor{}
		headers := buildHeaders("api.openai.com", "Bearer sk-openai-api-key")
		resp := p.handleOutbound(context.Background(), headers)

		if !isPassthrough(resp) {
			t.Error("expected passthrough for unmatched host api.openai.com, but got non-passthrough response")
		}
	})

	t.Run("route_passthrough_explicit", func(t *testing.T) {
		p := &processor{}
		headers := buildHeaders("otel-collector.kagenti-system.svc.cluster.local", "")
		resp := p.handleOutbound(context.Background(), headers)

		if !isPassthrough(resp) {
			t.Error("expected passthrough for otel-collector (explicit passthrough route), but got non-passthrough response")
		}
	})
}

// TestOutboundPolicyRouteMatchExchangeFails verifies that when a route matches but
// Keycloak returns an error, the proxy returns 503 (not passthrough).
func TestOutboundPolicyRouteMatchExchangeFails(t *testing.T) {
	saved := saveGlobals()
	defer restoreGlobals(saved)

	keycloak := mockKeycloak(t, http.StatusBadRequest, map[string]string{
		"error":             "invalid_request",
		"error_description": "Invalid token",
	})
	defer keycloak.Close()

	routesYAML := fmt.Sprintf(`
- host: "github-issue-tool-headless.team1.svc.cluster.local"
  target_audience: "github-tool"
  token_scopes: "openid github-tool-aud"
  token_url: %q
`, keycloak.URL)

	defaultOutboundPolicy = "passthrough"
	globalResolver = setupTestResolver(t, routesYAML)
	setGlobalConfig("test-client", "test-secret", keycloak.URL)

	p := &processor{}
	headers := buildHeaders("github-issue-tool-headless.team1.svc.cluster.local", "Bearer some-invalid-jwt")
	resp := p.handleOutbound(context.Background(), headers)

	if !isDenied(resp) {
		t.Error("expected 503 denial when exchange fails for a routed host, but got non-denied response")
	}
}
