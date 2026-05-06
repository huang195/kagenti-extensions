// Package reverseproxy implements an HTTP reverse proxy listener.
// Inbound requests are validated via the inbound pipeline before being
// forwarded to a fixed backend.
package reverseproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

type pctxKey struct{}

// responseRejectedError carries a pipeline Reject from the roundTripper
// back to the error handler, where it's rendered into the
// http.ResponseWriter. The embedded action keeps Violation.Render() and
// helper constructors available at the render site.
type responseRejectedError struct {
	action pipeline.Action
}

func (e *responseRejectedError) Error() string {
	if e.action.Violation != nil {
		return e.action.Violation.Reason
	}
	return "response rejected"
}

// Server is an HTTP reverse proxy with inbound JWT validation.
type Server struct {
	InboundPipeline *pipeline.Pipeline
	Sessions        *session.Store // nil when session tracking is disabled
	proxy           *httputil.ReverseProxy
	backend         string
}

// NewServer creates a reverse proxy that forwards to the given backend URL.
func NewServer(inbound *pipeline.Pipeline, sessions *session.Store, backendURL string) (*Server, error) {
	target, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	s := &Server{
		InboundPipeline: inbound,
		Sessions:        sessions,
		proxy:           proxy,
		backend:         backendURL,
	}
	proxy.ModifyResponse = s.modifyResponse
	proxy.ErrorHandler = s.errorHandler
	return s, nil
}

// Handler returns the HTTP handler for the reverse proxy.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleRequest)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Host:      r.Host,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
	}

	if s.InboundPipeline.NeedsBody() && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Warn("reverse-proxy: request body too large or unreadable", "host", r.Host, "error", err)
			http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		pctx.Body = body
		slog.Debug("reverse-proxy: buffered request body", "host", r.Host, "bodyLen", len(body))
	}

	action := s.InboundPipeline.Run(r.Context(), pctx)
	if action.Type == pipeline.Reject {
		writeRejection(w, action)
		return
	}

	if s.Sessions != nil && pctx.Extensions.A2A != nil {
		sid := pctx.Extensions.A2A.SessionID
		if sid == "" {
			sid = s.Sessions.ActiveSession()
		}
		if sid == "" {
			sid = session.DefaultSessionID
		}
		s.Sessions.Append(sid, pipeline.SessionEvent{
			At:        time.Now(),
			Direction: pipeline.Inbound,
			Phase:     pipeline.SessionRequest,
			A2A:       pctx.Extensions.A2A,
		})
	}

	r = r.WithContext(context.WithValue(r.Context(), pctxKey{}, pctx))
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) modifyResponse(resp *http.Response) error {
	pctx, _ := resp.Request.Context().Value(pctxKey{}).(*pipeline.Context)
	if pctx == nil {
		return nil
	}

	pctx.StatusCode = resp.StatusCode
	pctx.ResponseHeaders = resp.Header.Clone()

	if s.InboundPipeline.NeedsBody() && resp.Body != nil {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
		if err != nil {
			return err
		}
		resp.Body.Close()
		if len(body) > maxBodySize {
			return fmt.Errorf("response body too large (%d bytes)", len(body))
		}
		pctx.ResponseBody = body
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}

	action := s.InboundPipeline.RunResponse(resp.Request.Context(), pctx)
	if action.Type == pipeline.Reject {
		return &responseRejectedError{action: action}
	}

	// If a plugin mutated ResponseBody, use the mutated version.
	if s.InboundPipeline.NeedsBody() && pctx.ResponseBody != nil {
		resp.Body = io.NopCloser(bytes.NewReader(pctx.ResponseBody))
		resp.ContentLength = int64(len(pctx.ResponseBody))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(pctx.ResponseBody)))
	}
	return nil
}

func (s *Server) errorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	if rErr, ok := err.(*responseRejectedError); ok {
		writeRejection(w, rErr.action)
		return
	}
	http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
}

// writeRejection renders a pipeline Reject to the http.ResponseWriter,
// preserving the plugin's status, headers, and body.
func writeRejection(w http.ResponseWriter, action pipeline.Action) {
	status, headers, body := action.Violation.Render()
	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
