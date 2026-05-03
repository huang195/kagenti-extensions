// Package reverseproxy implements an HTTP reverse proxy listener.
// Inbound requests are validated via the inbound pipeline before being
// forwarded to a fixed backend.
package reverseproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

// Server is an HTTP reverse proxy with inbound JWT validation.
type Server struct {
	InboundPipeline *pipeline.Pipeline
	proxy           *httputil.ReverseProxy
	backend         string
}

// NewServer creates a reverse proxy that forwards to the given backend URL.
func NewServer(inbound *pipeline.Pipeline, backendURL string) (*Server, error) {
	target, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}
	return &Server{
		InboundPipeline: inbound,
		proxy:           httputil.NewSingleHostReverseProxy(target),
		backend:         backendURL,
	}, nil
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(action.Status)
		body, _ := json.Marshal(map[string]string{"error": action.Reason})
		w.Write(body)
		return
	}

	s.proxy.ServeHTTP(w, r)
}
