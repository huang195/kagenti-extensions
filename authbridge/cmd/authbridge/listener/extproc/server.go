// Package extproc implements an Envoy ext_proc gRPC streaming listener.
// It translates ext_proc ProcessingRequests into pipeline runs and maps
// the results back to ProcessingResponses.
package extproc

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocfilterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

// Server implements the Envoy ext_proc ExternalProcessor gRPC service.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer
	InboundPipeline  *pipeline.Pipeline
	OutboundPipeline *pipeline.Pipeline
}

// Process handles the bidirectional ext_proc stream.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()

	// pendingHeaders/pendingDirection hold state between RequestHeaders and
	// RequestBody phases. Envoy guarantees sequential message ordering per
	// stream: RequestBody always follows its RequestHeaders, and each stream
	// is a single request — no interleaving or stale state is possible.
	var pendingHeaders *corev3.HeaderMap
	var pendingDirection string

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := stream.Recv()
		if err != nil {
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		var resp *extprocv3.ProcessingResponse

		switch r := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			headers := r.RequestHeaders.Headers
			direction := getHeader(headers, "x-authbridge-direction")

			p := s.OutboundPipeline
			if direction == "inbound" {
				p = s.InboundPipeline
			}

			if p.NeedsBody() && requestHasBody(headers) {
				slog.Debug("ext_proc: requesting body from Envoy", "direction", direction)
				pendingHeaders = headers
				pendingDirection = direction
				resp = requestBodyResponse()
			} else if direction == "inbound" {
				resp = s.handleInbound(stream, headers, nil)
			} else {
				resp = s.handleOutbound(stream, headers, nil)
			}

		case *extprocv3.ProcessingRequest_RequestBody:
			body := r.RequestBody.Body
			slog.Debug("ext_proc: received request body", "direction", pendingDirection, "bodyLen", len(body))
			if len(body) > maxBodySize {
				slog.Warn("ext_proc: request body too large", "direction", pendingDirection, "bodyLen", len(body))
				resp = immediateResponse(http.StatusRequestEntityTooLarge, "request body too large")
			} else if pendingDirection == "inbound" {
				resp = s.handleInboundBody(stream, pendingHeaders, body)
			} else {
				resp = s.handleOutboundBody(stream, pendingHeaders, body)
			}
			pendingHeaders = nil
			pendingDirection = ""

		case *extprocv3.ProcessingRequest_ResponseHeaders:
			resp = &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extprocv3.HeadersResponse{},
				},
			}

		default:
			resp = &extprocv3.ProcessingResponse{}
		}

		if err := stream.Send(resp); err != nil {
			return status.Errorf(codes.Unknown, "cannot send stream response: %v", err)
		}
	}
}

func (s *Server) handleInbound(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) *extprocv3.ProcessingResponse {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
	}

	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		return denyResponse(typev3.StatusCode(action.Status),
			jsonError("unauthorized", action.Reason))
	}

	return allowResponse()
}

func (s *Server) handleInboundBody(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) *extprocv3.ProcessingResponse {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
	}

	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		return denyResponse(typev3.StatusCode(action.Status),
			jsonError("unauthorized", action.Reason))
	}

	return allowBodyResponse()
}

func (s *Server) handleOutbound(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) *extprocv3.ProcessingResponse {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      getHeader(headers, ":authority"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
	}
	if pctx.Host == "" {
		pctx.Host = getHeader(headers, "host")
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		return denyResponse(typev3.StatusCode_ServiceUnavailable,
			jsonError("token_acquisition_failed", action.Reason))
	}

	newAuth := pctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		return replaceTokenResponse(extractBearer(newAuth))
	}
	return passResponse()
}

func (s *Server) handleOutboundBody(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) *extprocv3.ProcessingResponse {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      getHeader(headers, ":authority"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
	}
	if pctx.Host == "" {
		pctx.Host = getHeader(headers, "host")
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		return denyResponse(typev3.StatusCode_ServiceUnavailable,
			jsonError("token_acquisition_failed", action.Reason))
	}

	newAuth := pctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		return replaceTokenBodyResponse(extractBearer(newAuth))
	}
	return passBodyResponse()
}

func headerMapToHTTP(headers *corev3.HeaderMap) http.Header {
	h := make(http.Header)
	if headers != nil {
		for _, hdr := range headers.Headers {
			h.Set(hdr.Key, string(hdr.RawValue))
		}
	}
	return h
}

func extractBearer(authHeader string) string {
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return authHeader[7:]
	}
	return ""
}

func requestBodyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{},
		},
		ModeOverride: &extprocfilterv3.ProcessingMode{
			RequestBodyMode: extprocfilterv3.ProcessingMode_BUFFERED,
		},
	}
}

func allowResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						RemoveHeaders: []string{"x-authbridge-direction"},
					},
				},
			},
		},
	}
}

func passResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{},
		},
	}
}

func passBodyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{},
		},
	}
}

func allowBodyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						RemoveHeaders: []string{"x-authbridge-direction"},
					},
				},
			},
		},
	}
}

func replaceTokenBodyResponse(token string) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						SetHeaders: []*corev3.HeaderValueOption{
							{
								Header: &corev3.HeaderValue{
									Key:      "authorization",
									RawValue: []byte("Bearer " + token),
								},
							},
						},
					},
				},
			},
		},
	}
}

func replaceTokenResponse(token string) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						SetHeaders: []*corev3.HeaderValueOption{
							{
								Header: &corev3.HeaderValue{
									Key:      "authorization",
									RawValue: []byte("Bearer " + token),
								},
							},
						},
					},
				},
			},
		},
	}
}

func denyResponse(code typev3.StatusCode, body string) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status: &typev3.HttpStatus{Code: code},
				Body:   []byte(body),
			},
		},
	}
}

func immediateResponse(httpStatus int, reason string) *extprocv3.ProcessingResponse {
	body, _ := json.Marshal(map[string]string{"error": reason})
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status: &typev3.HttpStatus{Code: typev3.StatusCode(httpStatus)},
				Body:   body,
			},
		},
	}
}

func jsonError(errorCode, message string) string {
	b, _ := json.Marshal(map[string]string{"error": errorCode, "message": message})
	return string(b)
}

func requestHasBody(headers *corev3.HeaderMap) bool {
	method := getHeader(headers, ":method")
	if method == "GET" || method == "HEAD" || method == "OPTIONS" || method == "DELETE" {
		return false
	}
	cl := getHeader(headers, "content-length")
	if cl != "" && cl != "0" {
		return true
	}
	te := getHeader(headers, "transfer-encoding")
	return te != ""
}

func getHeader(headers *corev3.HeaderMap, key string) string {
	if headers == nil {
		return ""
	}
	for _, h := range headers.Headers {
		if strings.EqualFold(h.Key, key) {
			return string(h.RawValue)
		}
	}
	return ""
}
