package extauthz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"

	authpkg "github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

type mockVerifier struct {
	claims       *validation.Claims
	err          error
	lastAudience string
}

func (m *mockVerifier) Verify(_ context.Context, _ string, audience string) (*validation.Claims, error) {
	m.lastAudience = audience
	return m.claims, m.err
}

func serverFromAuth(t *testing.T, a *authpkg.Auth) *Server {
	t.Helper()
	// ext_authz is waypoint mode — audience derived from host
	inbound, err := plugins.WaypointInboundPipeline(a)
	if err != nil {
		t.Fatalf("building inbound pipeline: %v", err)
	}
	outbound, err := plugins.DefaultOutboundPipeline(a)
	if err != nil {
		t.Fatalf("building outbound pipeline: %v", err)
	}
	return &Server{InboundPipeline: inbound, OutboundPipeline: outbound}
}

func checkRequest(host, path, authHeader string) *authv3.CheckRequest {
	headers := map[string]string{
		":authority": host,
		":path":      path,
	}
	if authHeader != "" {
		headers["authorization"] = authHeader
	}
	return &authv3.CheckRequest{
		Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{
				Http: &authv3.AttributeContext_HttpRequest{
					Headers: headers,
					Path:    path,
				},
			},
		},
	}
}

// ServiceNameFromHost is tested in routing/hostutil_test.go (shared implementation)

func TestCheck_ValidToken_Exchange(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer exchangeSrv.Close()

	mv := &mockVerifier{claims: &validation.Claims{
		Subject: "user", Audience: []string{"caller-agent"},
	}}
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	exchanger := exchange.NewClient(exchangeSrv.URL, &exchange.ClientSecretAuth{
		ClientID: "svc", ClientSecret: "secret",
	})
	a := authpkg.New(authpkg.Config{
		Verifier:  mv,
		Router:    router,
		Exchanger: exchanger,
		Cache:     cache.New(),
		Identity:  authpkg.IdentityConfig{Audience: "default-aud"},
	})
	srv := serverFromAuth(t, a)

	resp, err := srv.Check(context.Background(),
		checkRequest("auth-target-service.authbridge.svc:8081", "/api/test", "Bearer user-token"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Audience should be derived from host
	if mv.lastAudience != "auth-target-service" {
		t.Errorf("audience = %q, want auth-target-service", mv.lastAudience)
	}

	// Should be OK with token replacement
	ok := resp.GetOkResponse()
	if ok == nil {
		t.Fatal("expected OkResponse")
	}
	if len(ok.Headers) == 0 {
		t.Fatal("expected Authorization header override")
	}
	if ok.Headers[0].Header.Value != "Bearer exchanged" {
		t.Errorf("token = %q, want Bearer exchanged", ok.Headers[0].Header.Value)
	}
}

func TestCheck_InvalidToken(t *testing.T) {
	a := authpkg.New(authpkg.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("bad token")},
		Identity: authpkg.IdentityConfig{Audience: "aud"},
	})
	srv := serverFromAuth(t, a)

	resp, _ := srv.Check(context.Background(),
		checkRequest("svc", "/api", "Bearer bad"))

	denied := resp.GetDeniedResponse()
	if denied == nil {
		t.Fatal("expected DeniedResponse")
	}
	if resp.Status.Code != int32(codes.Unauthenticated) {
		t.Errorf("code = %d, want %d", resp.Status.Code, codes.Unauthenticated)
	}
}

func TestCheck_MissingHTTPAttributes(t *testing.T) {
	a := authpkg.New(authpkg.Config{})
	srv := serverFromAuth(t, a)

	resp, _ := srv.Check(context.Background(), &authv3.CheckRequest{})

	denied := resp.GetDeniedResponse()
	if denied == nil {
		t.Fatal("expected DeniedResponse for missing attributes")
	}
}
