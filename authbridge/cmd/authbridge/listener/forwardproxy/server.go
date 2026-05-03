// Package forwardproxy implements an HTTP forward proxy listener.
// Agents set HTTP_PROXY to route outbound traffic through this proxy
// for transparent token exchange.
package forwardproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

// Server is an HTTP forward proxy that performs token exchange on outbound requests.
type Server struct {
	OutboundPipeline *pipeline.Pipeline
	Client           *http.Client
}

// NewServer creates a forward proxy server with a default HTTP client.
func NewServer(outbound *pipeline.Pipeline) *Server {
	return &Server{
		OutboundPipeline: outbound,
		Client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Handler returns the HTTP handler for the forward proxy.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleRequest)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		http.Error(w, `{"error":"HTTPS CONNECT not supported — only HTTP proxy"}`, http.StatusMethodNotAllowed)
		return
	}

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      r.Host,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
	}

	if s.OutboundPipeline.NeedsBody() && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Warn("forward-proxy: request body too large or unreadable", "host", r.Host, "error", err)
			http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		pctx.Body = body
		slog.Debug("forward-proxy: buffered request body", "host", r.Host, "bodyLen", len(body))
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.OutboundPipeline.Run(r.Context(), pctx)

	if action.Type == pipeline.Reject {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(action.Status)
		body, _ := json.Marshal(map[string]string{"error": action.Reason})
		w.Write(body)
		return
	}

	newAuth := pctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		r.Header.Set("Authorization", "Bearer "+extractBearer(newAuth))
	}

	// Remove hop-by-hop headers
	r.Header.Del("Connection")
	r.Header.Del("Keep-Alive")
	r.Header.Del("Proxy-Authenticate")
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Proxy-Connection")
	r.Header.Del("TE")
	r.Header.Del("Trailer")
	r.Header.Del("Transfer-Encoding")
	r.Header.Del("Upgrade")

	// Clear RequestURI — set by the server but must be empty for client requests
	r.RequestURI = ""

	resp, err := s.Client.Do(r)
	if err != nil {
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Debug("response copy error", "host", r.Host, "error", err)
	}
}

func extractBearer(authHeader string) string {
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return authHeader[7:]
	}
	return ""
}
