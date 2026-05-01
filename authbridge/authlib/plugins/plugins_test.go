package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
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

// --- JWTValidation Tests ---

func TestJWTValidation_ValidToken(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user-1", ClientID: "agent"}},
		Identity: auth.IdentityConfig{Audience: "my-agent"},
	})
	plugin := NewJWTValidation(a)

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Path:      "/api/test",
		Headers:   http.Header{"Authorization": []string{"Bearer valid-token"}},
	}
	action := plugin.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue", action.Type)
	}
	if pctx.Claims == nil {
		t.Fatal("expected pctx.Claims to be populated")
	}
	if pctx.Claims.Subject != "user-1" {
		t.Errorf("subject = %q, want user-1", pctx.Claims.Subject)
	}
}

func TestJWTValidation_InvalidToken(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: errorf("token expired")},
		Identity: auth.IdentityConfig{Audience: "my-agent"},
	})
	plugin := NewJWTValidation(a)

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Path:      "/api/test",
		Headers:   http.Header{"Authorization": []string{"Bearer bad-token"}},
	}
	action := plugin.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	if action.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", action.Status)
	}
}

func TestJWTValidation_MissingHeader(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user"}},
		Identity: auth.IdentityConfig{Audience: "my-agent"},
	})
	plugin := NewJWTValidation(a)

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Path:      "/api/test",
		Headers:   http.Header{},
	}
	action := plugin.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	if action.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", action.Status)
	}
}

func TestJWTValidation_WithAudienceDeriver(t *testing.T) {
	var receivedAudience string
	verifier := &mockVerifier{claims: &validation.Claims{Subject: "user"}}
	a := auth.New(auth.Config{
		Verifier: &captureAudienceVerifier{inner: verifier, captured: &receivedAudience},
		Identity: auth.IdentityConfig{Audience: "default-aud"},
	})
	plugin := NewJWTValidation(a, WithAudienceDeriver(func(host string) string {
		return "derived-from-" + host
	}))

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Host:      "target-svc",
		Path:      "/api",
		Headers:   http.Header{"Authorization": []string{"Bearer token"}},
	}
	action := plugin.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("got %v, want Continue", action.Type)
	}
	if receivedAudience != "derived-from-target-svc" {
		t.Errorf("audience = %q, want derived-from-target-svc", receivedAudience)
	}
}

// --- TokenExchange Tests ---

func TestTokenExchange_Passthrough(t *testing.T) {
	router, _ := routing.NewRouter("passthrough", []routing.Route{})
	a := auth.New(auth.Config{Router: router})
	plugin := NewTokenExchange(a)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "some-host",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := plugin.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue", action.Type)
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

	router, _ := routing.NewRouter("exchange", []routing.Route{})
	exchanger := exchange.NewClient(exchangeSrv.URL, &exchange.ClientSecretAuth{
		ClientID: "agent", ClientSecret: "secret",
	})
	a := auth.New(auth.Config{
		Router:    router,
		Exchanger: exchanger,
		Cache:     cache.New(),
	})
	plugin := NewTokenExchange(a)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := plugin.OnRequest(context.Background(), pctx)
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

	router, _ := routing.NewRouter("exchange", []routing.Route{})
	exchanger := exchange.NewClient(exchangeSrv.URL, &exchange.ClientSecretAuth{
		ClientID: "agent", ClientSecret: "secret",
	})
	a := auth.New(auth.Config{
		Router:    router,
		Exchanger: exchanger,
	})
	plugin := NewTokenExchange(a)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{"Authorization": []string{"Bearer user-token"}},
	}
	action := plugin.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	if action.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", action.Status)
	}
}

func TestTokenExchange_NoToken_Deny(t *testing.T) {
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	a := auth.New(auth.Config{
		Router:        router,
		NoTokenPolicy: auth.NoTokenPolicyDeny,
	})
	plugin := NewTokenExchange(a)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "target-svc",
		Headers:   http.Header{},
	}
	action := plugin.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject", action.Type)
	}
	if action.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", action.Status)
	}
}

// --- Registry/Build Tests ---

func TestBuild_ValidNames(t *testing.T) {
	a := auth.New(auth.Config{})
	p, err := Build([]string{"jwt-validation", "token-exchange"}, a)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil pipeline")
	}
}

func TestBuild_UnknownName(t *testing.T) {
	a := auth.New(auth.Config{})
	_, err := Build([]string{"nonexistent-plugin"}, a)
	if err == nil {
		t.Fatal("expected error for unknown plugin name")
	}
}

func TestBuild_EmptyList(t *testing.T) {
	a := auth.New(auth.Config{})
	p, err := Build([]string{}, a)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	action := p.Run(context.Background(), &pipeline.Context{Headers: http.Header{}})
	if action.Type != pipeline.Continue {
		t.Errorf("empty pipeline got %v, want Continue", action.Type)
	}
}

func TestDefaultInboundPipeline(t *testing.T) {
	a := auth.New(auth.Config{})
	p, err := DefaultInboundPipeline(a)
	if err != nil {
		t.Fatalf("DefaultInboundPipeline returned error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil pipeline")
	}
}

func TestDefaultOutboundPipeline(t *testing.T) {
	a := auth.New(auth.Config{})
	p, err := DefaultOutboundPipeline(a)
	if err != nil {
		t.Fatalf("DefaultOutboundPipeline returned error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil pipeline")
	}
}

// --- Helpers ---

type errString string

func errorf(s string) error { return errString(s) }

func (e errString) Error() string { return string(e) }

// captureAudienceVerifier wraps a verifier and captures the audience parameter.
type captureAudienceVerifier struct {
	inner    *mockVerifier
	captured *string
}

func (v *captureAudienceVerifier) Verify(ctx context.Context, token string, audience string) (*validation.Claims, error) {
	*v.captured = audience
	return v.inner.Verify(ctx, token, audience)
}
