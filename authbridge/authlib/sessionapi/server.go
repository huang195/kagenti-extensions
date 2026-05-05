// Package sessionapi exposes AuthBridge's in-memory session store over HTTP:
// JSON snapshots plus an SSE stream of live events. Intended for local
// operators debugging the plugin pipeline via kubectl port-forward and for
// the abctl TUI.
//
// Trust model: no authentication. Bind only on in-cluster addresses, never
// behind an ingress. The payload may contain user messages, LLM completions,
// and tool results verbatim.
package sessionapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

// defaultHeartbeatInterval is how often the SSE stream sends a keep-alive
// comment so clients can detect a dead connection. Tuneable for tests via
// WithHeartbeatInterval.
const defaultHeartbeatInterval = 30 * time.Second

// Server wraps an http.Server bound to a session store.
type Server struct {
	server    *http.Server
	store     *session.Store
	heartbeat time.Duration
}

// Option configures a Server at construction time.
type Option func(*Server)

// WithHeartbeatInterval overrides the SSE heartbeat cadence. Primarily for
// tests — production deployments should use the default.
func WithHeartbeatInterval(d time.Duration) Option {
	return func(s *Server) { s.heartbeat = d }
}

// New constructs an HTTP server serving the session API at addr. store must
// be non-nil; callers should only instantiate when session tracking is on.
func New(addr string, store *session.Store, opts ...Option) *Server {
	s := &Server{
		store:     store,
		heartbeat: defaultHeartbeatInterval,
	}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions", s.handleList)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGet)
	mux.HandleFunc("GET /v1/events", s.handleStream)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	s.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Server returns the underlying *http.Server so callers can register it for
// graceful shutdown alongside the binary's other HTTP listeners.
func (s *Server) Server() *http.Server { return s.server }

// ListenAndServe blocks until the server returns. Returns http.ErrServerClosed
// on graceful shutdown.
func (s *Server) ListenAndServe() error { return s.server.ListenAndServe() }

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.server.Shutdown(ctx) }

// --- handlers -------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}{Sessions: s.store.ListSessions()}); err != nil {
		slog.Debug("sessionapi: list encode failed", "error", err)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	view := s.store.View(id)
	if view == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(view); err != nil {
		slog.Debug("sessionapi: get encode failed", "error", err, "sessionID", id)
	}
}

// handleStream delivers new session events as an SSE stream. Supports
// ?session=<id> to filter to one session. A heartbeat comment is emitted
// at the configured interval so clients can detect dead connections.
//
// Lifecycle: subscribes to the store, flushes each event to the client, and
// exits when the client disconnects (via r.Context().Done()). The subscriber
// is always cancelled on exit to free the buffered channel.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	filter := strings.TrimSpace(r.URL.Query().Get("session"))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering if any

	// Initial comment lets the client know the stream is live before any events.
	fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()

	sub, cancel := s.store.Subscribe()
	defer cancel()

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()

	var id uint64
	var lastDrops uint64

	for {
		select {
		case <-r.Context().Done():
			return

		case event, ok := <-sub.Events():
			if !ok {
				// Store closed or subscription cancelled externally.
				return
			}
			if filter != "" && event.A2A != nil && event.A2A.SessionID != filter {
				continue
			}
			// For outbound events there's no direct sessionID; best-effort
			// filtering happens only when the event carries an A2A extension.
			// A policy plugin wanting fine-grained per-session outbound filtering
			// should consume the full stream and filter client-side.

			payload, err := json.Marshal(event)
			if err != nil {
				slog.Debug("sessionapi: marshal event failed", "error", err)
				continue
			}
			atomic.AddUint64(&id, 1)
			fmt.Fprintf(w, "event: session-event\nid: %d\ndata: %s\n\n", id, payload)
			flusher.Flush()

		case <-heartbeat.C:
			// Surface accumulated drops so the operator can notice a slow client.
			if drops := sub.Drops(); drops > lastDrops {
				slog.Warn("sessionapi: sse consumer lagged",
					"drops", drops, "newDrops", drops-lastDrops)
				lastDrops = drops
			}
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
