package reverseproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

type mockVerifier struct {
	claims *validation.Claims
	err    error
}

func (m *mockVerifier) Verify(_ context.Context, _ string, _ string) (*validation.Claims, error) {
	return m.claims, m.err
}

func inboundPipelineFromAuth(t *testing.T, a *auth.Auth) *pipeline.Pipeline {
	t.Helper()
	p, err := plugins.DefaultInboundPipeline(a)
	if err != nil {
		t.Fatalf("building inbound pipeline: %v", err)
	}
	return p
}

func TestReverseProxy_AllowedRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audience: "my-app"},
	})
	srv, err := NewServer(inboundPipelineFromAuth(t, a), backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestReverseProxy_DeniedRequest(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("invalid token")},
		Identity: auth.IdentityConfig{Audience: "my-app"},
	})
	srv, _ := NewServer(inboundPipelineFromAuth(t, a), "http://localhost:9999")

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestReverseProxy_BypassPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("agent-card"))
	}))
	defer backend.Close()

	matcher, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("should not be called")},
		Bypass:   matcher,
	})
	srv, _ := NewServer(inboundPipelineFromAuth(t, a), backend.URL)

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// No auth header, but bypass path should be allowed
	req, _ := http.NewRequest("GET", proxy.URL+"/.well-known/agent.json", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for bypass path", resp.StatusCode)
	}
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

func TestReverseProxy_BodyBuffering(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(p, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"method":"tools/call","id":1,"params":{"name":"get_weather"}}`
	req, _ := http.NewRequest("POST", proxy.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if string(recorder.receivedBody) != body {
		t.Errorf("plugin got body = %q, want %q", recorder.receivedBody, body)
	}
}

func TestReverseProxy_BodyTooLarge(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("backend should not be reached for oversized body")
	}))
	defer backend.Close()

	recorder := &bodyRecorderPlugin{}
	p, err := pipeline.New([]pipeline.Plugin{recorder})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(p, backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	bigBody := strings.Repeat("x", maxBodySize+1)
	req, _ := http.NewRequest("POST", proxy.URL+"/mcp", strings.NewReader(bigBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestReverseProxy_BodyNotBuffered_WhenNotNeeded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audience: "test"},
	})
	srv, err := NewServer(inboundPipelineFromAuth(t, a), backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"data":"should not be buffered"}`
	req, _ := http.NewRequest("POST", proxy.URL+"/api", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
