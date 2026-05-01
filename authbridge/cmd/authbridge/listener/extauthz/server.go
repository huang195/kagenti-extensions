// Package extauthz implements an Envoy ext_authz gRPC unary listener.
// Used by waypoint mode where both inbound validation and outbound exchange
// happen in a single Check() call.
package extauthz

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"

	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
)

// Server implements the Envoy ext_authz Authorization gRPC service.
type Server struct {
	authv3.UnimplementedAuthorizationServer
	InboundPipeline  *pipeline.Pipeline
	OutboundPipeline *pipeline.Pipeline
}

// Check handles a single ext_authz authorization request.
func (s *Server) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	httpReq := req.GetAttributes().GetRequest().GetHttp()
	if httpReq == nil {
		return denied(codes.InvalidArgument, 400, "missing HTTP request attributes"), nil
	}

	headers := httpReq.GetHeaders()
	host := headers[":authority"]
	if host == "" {
		host = headers["host"]
	}
	path := httpReq.GetPath()

	// Derive audience from destination host (waypoint pattern)
	audience := routing.ServiceNameFromHost(host)
	_ = audience // audience derivation will be used in future waypoint-specific plugin

	// Inbound validation via pipeline
	inPctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Host:      host,
		Path:      path,
		Headers:   mapToHTTPHeader(headers),
	}
	inAction := s.InboundPipeline.Run(ctx, inPctx)
	if inAction.Type == pipeline.Reject {
		return denied(codes.Unauthenticated, inAction.Status, inAction.Reason), nil
	}

	// Outbound exchange via pipeline
	outPctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      host,
		Path:      path,
		Headers:   mapToHTTPHeader(headers),
	}
	originalAuth := outPctx.Headers.Get("Authorization")
	outAction := s.OutboundPipeline.Run(ctx, outPctx)
	if outAction.Type == pipeline.Reject {
		return denied(codes.PermissionDenied, outAction.Status, outAction.Reason), nil
	}

	newAuth := outPctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		return allowedWithToken(extractBearer(newAuth)), nil
	}
	return allowed(), nil
}

func mapToHTTPHeader(m map[string]string) http.Header {
	h := make(http.Header)
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func extractBearer(authHeader string) string {
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return authHeader[7:]
	}
	return ""
}

func denied(code codes.Code, httpStatus int, msg string) *authv3.CheckResponse {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{
			Code:    int32(code),
			Message: msg,
		},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status: &typev3.HttpStatus{
					Code: typev3.StatusCode(httpStatus),
				},
				Body: string(body),
				Headers: []*corev3.HeaderValueOption{
					{
						Header: &corev3.HeaderValue{
							Key:   "Content-Type",
							Value: "application/json",
						},
					},
				},
			},
		},
	}
}

func allowed() *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status:       &rpcstatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{}},
	}
}

func allowedWithToken(token string) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{
				Headers: []*corev3.HeaderValueOption{
					{
						Header: &corev3.HeaderValue{
							Key:   "authorization",
							Value: "Bearer " + token,
						},
					},
				},
			},
		},
	}
}
