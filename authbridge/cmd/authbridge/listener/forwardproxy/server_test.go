package forwardproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

type mockVerifier struct {
	claims *validation.Claims
	err    error
}

func (m *mockVerifier) Verify(_ context.Context, _ string, _ string) (*validation.Claims, error) {
	return m.claims, m.err
}

func outboundPipelineFromAuth(t *testing.T, a *auth.Auth) *pipeline.Pipeline {
	t.Helper()
	p, err := plugins.DefaultOutboundPipeline(a)
	if err != nil {
		t.Fatalf("building outbound pipeline: %v", err)
	}
	return p
}

func TestForwardProxy_Exchange(t *testing.T) {
	// Token exchange server
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-token",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer exchangeSrv.Close()

	// Backend server that the proxy forwards to
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if got != "Bearer exchanged-token" {
			t.Errorf("backend got Authorization = %q, want Bearer exchanged-token", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	router, _ := routing.NewRouter("exchange", []routing.Route{})
	exchanger := exchange.NewClient(exchangeSrv.URL, &exchange.ClientSecretAuth{
		ClientID: "agent", ClientSecret: "secret",
	})
	a := auth.New(auth.Config{
		Router:    router,
		Exchanger: exchanger,
		Cache:     cache.New(),
	})

	srv := &Server{OutboundPipeline: outboundPipelineFromAuth(t, a), Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Forward proxy: request URL is the full backend URL (as a proxy would receive)
	req, _ := http.NewRequest("GET", backend.URL+"/test", nil)
	req.Header.Set("Authorization", "Bearer user-token")

	// Route through the proxy by sending the request to proxy address
	// but with the backend URL as the target (simulates HTTP_PROXY behavior)
	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestForwardProxy_CONNECT_Rejected(t *testing.T) {
	a := auth.New(auth.Config{})
	srv := NewServer(outboundPipelineFromAuth(t, a))
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("CONNECT", proxy.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestForwardProxy_Deny(t *testing.T) {
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	a := auth.New(auth.Config{
		Router:        router,
		NoTokenPolicy: auth.NoTokenPolicyDeny,
	})

	srv := NewServer(outboundPipelineFromAuth(t, a))
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/test", nil)
	// No Authorization header — should be denied
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}

// --- Body Buffering Tests ---

// bodyRecorderPlugin records whether it received a body during OnRequest.
type bodyRecorderPlugin struct {
	receivedBody []byte
}

func (p *bodyRecorderPlugin) Name() string { return "body-recorder" }
func (p *bodyRecorderPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{BodyAccess: true}
}
func (p *bodyRecorderPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.receivedBody = pctx.Body
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *bodyRecorderPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func TestForwardProxy_BodyBuffering(t *testing.T) {
	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv := &Server{OutboundPipeline: p, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"method":"tools/call","id":1,"params":{"name":"get_weather"}}`
	req, _ := http.NewRequest("POST", backend.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if string(recorder.receivedBody) != body {
		t.Errorf("plugin got body = %q, want %q", recorder.receivedBody, body)
	}
}

func TestForwardProxy_BodyTooLarge(t *testing.T) {
	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("backend should not be reached for oversized body")
	}))
	defer backend.Close()

	srv := &Server{OutboundPipeline: p, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// Send body larger than maxBodySize (1MB)
	bigBody := strings.Repeat("x", maxBodySize+1)
	req, _ := http.NewRequest("POST", backend.URL+"/mcp", strings.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestForwardProxy_NoBodyBuffering_WhenNotNeeded(t *testing.T) {
	a := auth.New(auth.Config{})
	p := outboundPipelineFromAuth(t, a) // default pipeline has no body-access plugins

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv := &Server{OutboundPipeline: p, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"data":"should not be buffered"}`
	req, _ := http.NewRequest("POST", backend.URL+"/api", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
