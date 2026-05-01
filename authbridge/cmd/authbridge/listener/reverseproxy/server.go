// Package reverseproxy implements an HTTP reverse proxy listener.
// Inbound requests are validated via the inbound pipeline before being
// forwarded to a fixed backend.
package reverseproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

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
