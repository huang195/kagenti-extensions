package extproc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/metadata"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

// mockStream implements ExternalProcessor_ProcessServer for testing.
type mockStream struct {
	extprocv3.ExternalProcessor_ProcessServer
	ctx       context.Context
	requests  []*extprocv3.ProcessingRequest
	responses []*extprocv3.ProcessingResponse
	recvIdx   int
}

func (m *mockStream) Context() context.Context { return m.ctx }
func (m *mockStream) Send(resp *extprocv3.ProcessingResponse) error {
	m.responses = append(m.responses, resp)
	return nil
}
func (m *mockStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if m.recvIdx >= len(m.requests) {
		return nil, fmt.Errorf("EOF")
	}
	req := m.requests[m.recvIdx]
	m.recvIdx++
	return req, nil
}
func (m *mockStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockStream) SendHeader(metadata.MD) error { return nil }
func (m *mockStream) SetTrailer(metadata.MD)       {}
func (m *mockStream) SendMsg(any) error            { return nil }
func (m *mockStream) RecvMsg(any) error            { return nil }

type mockVerifier struct {
	claims *validation.Claims
	err    error
}

func (v *mockVerifier) Verify(_ context.Context, _ string, _ string) (*validation.Claims, error) {
	return v.claims, v.err
}

func serverFromAuth(t *testing.T, a *auth.Auth) *Server {
	t.Helper()
	inbound, err := plugins.DefaultInboundPipeline(a)
	if err != nil {
		t.Fatalf("building inbound pipeline: %v", err)
	}
	outbound, err := plugins.DefaultOutboundPipeline(a)
	if err != nil {
		t.Fatalf("building outbound pipeline: %v", err)
	}
	return &Server{InboundPipeline: inbound, OutboundPipeline: outbound}
}

func makeHeaders(kvs ...string) *corev3.HeaderMap {
	hm := &corev3.HeaderMap{}
	for i := 0; i < len(kvs); i += 2 {
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{
			Key:      kvs[i],
			RawValue: []byte(kvs[i+1]),
		})
	}
	return hm
}

func inboundRequest(headers *corev3.HeaderMap) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{Headers: headers},
		},
	}
}

func outboundRequest(headers *corev3.HeaderMap) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{Headers: headers},
		},
	}
}

// --- Inbound Tests ---

func TestExtProc_Inbound_ValidJWT(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{claims: &validation.Claims{Subject: "user-1"}},
		Identity: auth.IdentityConfig{Audience: "my-agent"},
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				"authorization", "Bearer valid-token",
				":path", "/api/test",
			)),
		},
	}

	_ = srv.Process(stream) // returns error on EOF from Recv, expected

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	resp := stream.responses[0]
	// Should be allow (HeadersResponse, not ImmediateResponse)
	rh := resp.GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected RequestHeaders response (allow), got ImmediateResponse")
	}
	// Should remove x-authbridge-direction header
	if rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatal("expected header mutation to remove direction header")
	}
	found := false
	for _, h := range rh.Response.HeaderMutation.RemoveHeaders {
		if h == "x-authbridge-direction" {
			found = true
		}
	}
	if !found {
		t.Error("expected x-authbridge-direction in RemoveHeaders")
	}
}

func TestExtProc_Inbound_InvalidJWT(t *testing.T) {
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("token expired")},
		Identity: auth.IdentityConfig{Audience: "my-agent"},
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				"authorization", "Bearer bad-token",
				":path", "/api/test",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	ir := stream.responses[0].GetImmediateResponse()
	if ir == nil {
		t.Fatal("expected ImmediateResponse (deny)")
	}
	if ir.Status.Code != 401 {
		t.Errorf("status = %d, want 401", ir.Status.Code)
	}
}

func TestExtProc_Inbound_BypassPath(t *testing.T) {
	matcher, _ := bypass.NewMatcher(bypass.DefaultPatterns)
	a := auth.New(auth.Config{
		Verifier: &mockVerifier{err: fmt.Errorf("should not be called")},
		Bypass:   matcher,
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			inboundRequest(makeHeaders(
				"x-authbridge-direction", "inbound",
				":path", "/healthz",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected allow for bypass path")
	}
}

// --- Outbound Tests ---

func TestExtProc_Outbound_Exchange(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "exchanged-token",
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
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "target-svc",
				"authorization", "Bearer user-token",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil || rh.Response == nil || rh.Response.HeaderMutation == nil {
		t.Fatal("expected HeadersResponse with token replacement")
	}
	if len(rh.Response.HeaderMutation.SetHeaders) == 0 {
		t.Fatal("expected SetHeaders with new token")
	}
	got := string(rh.Response.HeaderMutation.SetHeaders[0].Header.RawValue)
	if got != "Bearer exchanged-token" {
		t.Errorf("token = %q, want Bearer exchanged-token", got)
	}
}

func TestExtProc_Outbound_Passthrough(t *testing.T) {
	router, _ := routing.NewRouter("passthrough", []routing.Route{})
	a := auth.New(auth.Config{Router: router})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "unknown-svc",
				"authorization", "Bearer token",
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetRequestHeaders()
	if rh == nil {
		t.Fatal("expected passthrough (HeadersResponse)")
	}
	// Passthrough should have no header mutations
	if rh.Response != nil && rh.Response.HeaderMutation != nil && len(rh.Response.HeaderMutation.SetHeaders) > 0 {
		t.Error("passthrough should not set headers")
	}
}

func TestExtProc_Outbound_Deny(t *testing.T) {
	router, _ := routing.NewRouter("exchange", []routing.Route{})
	a := auth.New(auth.Config{
		Router:        router,
		NoTokenPolicy: auth.NoTokenPolicyDeny,
	})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			outboundRequest(makeHeaders(
				":authority", "target-svc",
				// No authorization header
			)),
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	ir := stream.responses[0].GetImmediateResponse()
	if ir == nil {
		t.Fatal("expected ImmediateResponse (deny)")
	}
	if ir.Status.Code != 503 {
		t.Errorf("status = %d, want 503", ir.Status.Code)
	}
}

// --- Response Headers ---

func TestExtProc_ResponseHeaders(t *testing.T) {
	a := auth.New(auth.Config{})
	srv := serverFromAuth(t, a)

	stream := &mockStream{
		ctx: context.Background(),
		requests: []*extprocv3.ProcessingRequest{
			{
				Request: &extprocv3.ProcessingRequest_ResponseHeaders{
					ResponseHeaders: &extprocv3.HttpHeaders{
						Headers: makeHeaders("content-type", "application/json"),
					},
				},
			},
		},
	}

	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	rh := stream.responses[0].GetResponseHeaders()
	if rh == nil {
		t.Fatal("expected ResponseHeaders passthrough")
	}
}
