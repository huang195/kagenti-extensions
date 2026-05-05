// Package session provides an in-memory session store for correlating
// inbound user intents with outbound tool calls across request boundaries.
// The store is per-pod (AuthBridge sidecar) and does not persist across restarts.
package session

import (
	"log/slog"
	"sync"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// DefaultSessionID is used when no explicit A2A SessionID is present and no
// active session exists. This collapses all such requests into one shared session,
// which is correct for single-agent pods but may cause cross-user correlation in
// multi-tenant deployments. Future work: derive session ID from JWT claims.
const DefaultSessionID = "default"

// entry holds the events for one conversation.
type entry struct {
	ID        string
	Events    []pipeline.SessionEvent
	CreatedAt time.Time
	UpdatedAt time.Time
}

const maxSessionIDLen = 256

// Store is an in-memory, per-pod session store. It is safe for concurrent use.
type Store struct {
	mu          sync.RWMutex
	sessions    map[string]*entry
	ttl         time.Duration
	maxEvents   int
	maxSessions int
	activeID    string
	stop        chan struct{}
	closeOnce   sync.Once
}

// New creates a Store with the given TTL, per-session event limit, and max
// concurrent sessions. A background goroutine runs cleanup every TTL/2.
// Call Close() during graceful shutdown to stop the background goroutine.
func New(ttl time.Duration, maxEvents int, maxSessions int) *Store {
	s := &Store{
		sessions:    make(map[string]*entry),
		ttl:         ttl,
		maxEvents:   maxEvents,
		maxSessions: maxSessions,
		stop:        make(chan struct{}),
	}
	go s.backgroundCleanup()
	return s
}

// Close stops the background cleanup goroutine. Safe to call multiple times.
func (s *Store) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
}

func (s *Store) backgroundCleanup() {
	interval := s.ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.Cleanup()
		}
	}
}

// Append adds an event to the named session. Creates the session if it
// doesn't exist. Updates activeID to this session. Evicts the oldest event
// if the session exceeds maxEvents.
func (s *Store) Append(sessionID string, event pipeline.SessionEvent) {
	if len(sessionID) > maxSessionIDLen {
		sessionID = sessionID[:maxSessionIDLen]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	sess, ok := s.sessions[sessionID]
	if !ok {
		sess = &entry{
			ID:        sessionID,
			CreatedAt: now,
		}
		s.sessions[sessionID] = sess
	}

	sess.Events = append(sess.Events, event)
	sess.UpdatedAt = now
	s.activeID = sessionID

	logAppended(sessionID, &event)

	if s.maxEvents > 0 && len(sess.Events) > s.maxEvents {
		excess := len(sess.Events) - s.maxEvents
		trimmed := make([]pipeline.SessionEvent, s.maxEvents)
		copy(trimmed, sess.Events[excess:])
		sess.Events = trimmed
	}

	// Evict oldest session if cap is exceeded.
	if s.maxSessions > 0 && len(s.sessions) > s.maxSessions {
		s.evictOldestLocked()
	}
}

// View returns a read-only snapshot of the session's events.
// Returns nil if the session doesn't exist or is expired.
func (s *Store) View(sessionID string) *pipeline.SessionView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	if s.isExpired(sess, time.Now()) {
		return nil
	}

	events := make([]pipeline.SessionEvent, len(sess.Events))
	copy(events, sess.Events)
	return &pipeline.SessionView{ID: sessionID, Events: events}
}

// ActiveSession returns the most recently updated session ID.
// Used for outbound correlation when no explicit session ID is available.
func (s *Store) ActiveSession() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.activeID == "" {
		return ""
	}
	sess, ok := s.sessions[s.activeID]
	if !ok || s.isExpired(sess, time.Now()) {
		return ""
	}
	return s.activeID
}

// Rekey renames a session from oldID to newID, preserving all events.
// Used to merge the bootstrap "default" session into the server-assigned
// contextId after the backend response reveals it, so events recorded
// during the request phase (under "default") and subsequent turns (under
// the real contextId) land in the same bucket.
//
// Safe to call when oldID does not exist (no-op) or newID already exists
// (no-op — preserves the existing newID entry). If oldID was the active
// session, activeID is updated to newID.
//
// Assumes single-tenant, no concurrent conversations per pod. In a
// multi-tenant deployment two in-flight first-turn requests could both
// land under "default"; rekeying the first to arrive would strand the
// second's events. Call sites are expected to guard against that.
func (s *Store) Rekey(oldID, newID string) {
	if oldID == newID || oldID == "" || newID == "" {
		return
	}
	if len(newID) > maxSessionIDLen {
		newID = newID[:maxSessionIDLen]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[oldID]
	if !ok {
		return
	}
	if _, exists := s.sessions[newID]; exists {
		return
	}

	sess.ID = newID
	s.sessions[newID] = sess
	delete(s.sessions, oldID)
	if s.activeID == oldID {
		s.activeID = newID
	}
}

// Cleanup removes expired sessions. Safe for concurrent use.
func (s *Store) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(time.Now())
}

func (s *Store) cleanupLocked(now time.Time) {
	for id, sess := range s.sessions {
		if s.isExpired(sess, now) {
			delete(s.sessions, id)
			if s.activeID == id {
				s.activeID = ""
			}
		}
	}
}

func (s *Store) evictOldestLocked() {
	var oldestID string
	var oldestTime time.Time
	for id, sess := range s.sessions {
		if id == s.activeID {
			continue
		}
		if oldestID == "" || sess.UpdatedAt.Before(oldestTime) {
			oldestID = id
			oldestTime = sess.UpdatedAt
		}
	}
	if oldestID == "" {
		// All sessions are the active session — evict it as last resort.
		oldestID = s.activeID
		s.activeID = ""
	}
	if oldestID != "" {
		delete(s.sessions, oldestID)
	}
}

func (s *Store) isExpired(sess *entry, now time.Time) bool {
	return now.Sub(sess.UpdatedAt) > s.ttl
}

// logAppended emits a structured DEBUG line so operators can observe session
// state evolution. Fields are chosen to cover the data captured by all four
// record helpers — extension payloads themselves are intentionally omitted
// since the parsers already log them.
func logAppended(sessionID string, e *pipeline.SessionEvent) {
	attrs := []any{
		"sessionId", sessionID,
		"direction", directionName(e.Direction),
		"phase", e.Phase.String(),
	}
	if e.Host != "" {
		attrs = append(attrs, "host", e.Host)
	}
	if e.TargetAudience != "" {
		attrs = append(attrs, "aud", e.TargetAudience)
	}
	if e.StatusCode != 0 {
		attrs = append(attrs, "status", e.StatusCode)
	}
	if e.Duration != 0 {
		attrs = append(attrs, "durationMs", e.Duration.Milliseconds())
	}
	if e.Identity != nil {
		if e.Identity.Subject != "" {
			attrs = append(attrs, "subject", e.Identity.Subject)
		}
		if e.Identity.AgentID != "" {
			attrs = append(attrs, "agent", e.Identity.AgentID)
		}
	}
	switch {
	case e.A2A != nil:
		attrs = append(attrs, "proto", "a2a", "method", e.A2A.Method)
	case e.MCP != nil:
		attrs = append(attrs, "proto", "mcp", "method", e.MCP.Method)
	case e.Inference != nil:
		attrs = append(attrs, "proto", "inference", "model", e.Inference.Model)
	}
	if e.Error != nil {
		attrs = append(attrs, "errorKind", e.Error.Kind)
	}
	slog.Debug("session: event appended", attrs...)
}

func directionName(d pipeline.Direction) string {
	if d == pipeline.Inbound {
		return "inbound"
	}
	return "outbound"
}
