// Package forwardproxy implements an HTTP forward proxy listener.
// Agents set HTTP_PROXY to route outbound traffic through this proxy
// for transparent token exchange.
package forwardproxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

// Server is an HTTP forward proxy that performs token exchange on outbound requests.
type Server struct {
	OutboundPipeline *pipeline.Pipeline
	Sessions         *session.Store // nil when session tracking is disabled
	Client           *http.Client
}

// NewServer creates a forward proxy server with a default HTTP client.
func NewServer(outbound *pipeline.Pipeline, sessions *session.Store) *Server {
	return &Server{
		OutboundPipeline: outbound,
		Sessions:         sessions,
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

	if s.Sessions != nil {
		if aid := s.Sessions.ActiveSession(); aid != "" {
			pctx.Session = s.Sessions.View(aid)
		}
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.OutboundPipeline.Run(r.Context(), pctx)

	if action.Type == pipeline.Reject {
		writeRejection(w, action)
		return
	}

	if s.Sessions != nil {
		sid := s.Sessions.ActiveSession()
		if sid == "" {
			sid = session.DefaultSessionID
		}
		ev := pipeline.SessionEvent{
			At:        time.Now(),
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			MCP:       pctx.Extensions.MCP,
			Inference: pctx.Extensions.Inference,
		}
		if ev.MCP != nil || ev.Inference != nil {
			s.Sessions.Append(sid, ev)
		}
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

	// Response phase: populate pctx and run plugins in reverse order.
	pctx.StatusCode = resp.StatusCode
	pctx.ResponseHeaders = resp.Header.Clone()

	if s.OutboundPipeline.NeedsBody() && resp.Body != nil {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
		if err != nil {
			slog.Warn("forward-proxy: response body read error", "host", r.Host, "error", err)
			http.Error(w, `{"error":"response body read error"}`, http.StatusBadGateway)
			return
		}
		if len(respBody) > maxBodySize {
			slog.Warn("forward-proxy: response body too large", "host", r.Host, "len", len(respBody))
			http.Error(w, `{"error":"response body too large"}`, http.StatusBadGateway)
			return
		}
		pctx.ResponseBody = respBody
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}

	respAction := s.OutboundPipeline.RunResponse(r.Context(), pctx)
	if respAction.Type == pipeline.Reject {
		writeRejection(w, respAction)
		return
	}

	// If a plugin mutated ResponseBody, use the mutated version.
	if s.OutboundPipeline.NeedsBody() && pctx.ResponseBody != nil {
		resp.Body = io.NopCloser(bytes.NewReader(pctx.ResponseBody))
		resp.ContentLength = int64(len(pctx.ResponseBody))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(pctx.ResponseBody)))
	}

	if s.Sessions != nil {
		sid := s.Sessions.ActiveSession()
		if sid == "" {
			sid = session.DefaultSessionID
		}
		ev := pipeline.SessionEvent{
			At:        time.Now(),
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionResponse,
			MCP:       pctx.Extensions.MCP,
			Inference: pctx.Extensions.Inference,
		}
		if ev.MCP != nil || ev.Inference != nil {
			s.Sessions.Append(sid, ev)
		}
	}

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

// writeRejection renders a pipeline Reject to the http.ResponseWriter.
// Preserves the plugin's status, headers, and body — the old flat
// {"error":reason} is gone; plugins now control the wire shape via
// action.Violation.
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
