package pipeline

import "time"

// SessionPhase distinguishes request from response events.
type SessionPhase int

const (
	SessionRequest SessionPhase = iota
	SessionResponse
)

func (p SessionPhase) String() string {
	switch p {
	case SessionRequest:
		return "request"
	case SessionResponse:
		return "response"
	default:
		return "unknown"
	}
}

// SessionEvent represents a single pipeline event captured by the session store.
// Exactly one of A2A, MCP, or Inference is non-nil.
type SessionEvent struct {
	At        time.Time
	Direction Direction
	Phase     SessionPhase
	A2A       *A2AExtension
	MCP       *MCPExtension
	Inference *InferenceExtension

	// Identity snapshot at record time. Lets downstream plugins attribute an
	// event to the caller (Subject) and the handling sidecar (AgentID)
	// without re-parsing the original request. Nil for events recorded
	// before jwt-validation ran or when session tracking is disabled.
	Identity *EventIdentity

	// StatusCode is the HTTP status of the response. Zero on request events
	// and on response events that were recorded before the status was known
	// (e.g., connection reset). A non-zero value >= 400 also populates Error.
	StatusCode int

	// Error captures a terminal error condition for this event. Nil on
	// successful requests and 2xx responses. Populated for non-2xx responses,
	// guardrail blocks, and parse failures.
	Error *EventError
}

// EventIdentity carries the "who" for a session event.
type EventIdentity struct {
	Subject  string   // end-user subject from JWT (Claims.Subject)
	Scopes   []string // validated scopes
	ClientID string   // JWT azp claim (the client that minted the token)
	AgentID  string   // the sidecar's own workload identity (Agent.WorkloadID)
}

// EventError describes why a response event represents a failure.
type EventError struct {
	Kind    string // "backend_error" | "blocked" | "parser_error" | "timeout"
	Code    string // protocol-specific error code (HTTP status, JSON-RPC code, etc.)
	Message string // human-readable reason; safe to surface in logs/metrics
}

// SessionView is a read-only snapshot of a session, safe to pass to plugins.
// It contains a copy of events — plugins cannot mutate the store.
type SessionView struct {
	ID     string
	Events []SessionEvent
}

// Intents returns only inbound A2A request events (user messages).
func (v *SessionView) Intents() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Direction == Inbound && e.Phase == SessionRequest && e.A2A != nil {
			out = append(out, e)
		}
	}
	return out
}

// ToolCalls returns only outbound MCP request events.
func (v *SessionView) ToolCalls() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Direction == Outbound && e.Phase == SessionRequest && e.MCP != nil {
			out = append(out, e)
		}
	}
	return out
}

// ToolResponses returns only outbound MCP response events.
func (v *SessionView) ToolResponses() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Direction == Outbound && e.Phase == SessionResponse && e.MCP != nil {
			out = append(out, e)
		}
	}
	return out
}

// InferenceRequests returns only outbound inference request events.
func (v *SessionView) InferenceRequests() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Direction == Outbound && e.Phase == SessionRequest && e.Inference != nil {
			out = append(out, e)
		}
	}
	return out
}

// LastIntent returns the most recent inbound A2A user message, or nil.
func (v *SessionView) LastIntent() *SessionEvent {
	for i := len(v.Events) - 1; i >= 0; i-- {
		e := v.Events[i]
		if e.Direction == Inbound && e.Phase == SessionRequest && e.A2A != nil {
			return &v.Events[i]
		}
	}
	return nil
}

// Len returns total event count.
func (v *SessionView) Len() int { return len(v.Events) }

// FailedEvents returns response-phase events that carry an Error.
func (v *SessionView) FailedEvents() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Phase == SessionResponse && e.Error != nil {
			out = append(out, e)
		}
	}
	return out
}

// LastError returns the most recent response event with an Error, or nil.
func (v *SessionView) LastError() *SessionEvent {
	for i := len(v.Events) - 1; i >= 0; i-- {
		if v.Events[i].Phase == SessionResponse && v.Events[i].Error != nil {
			return &v.Events[i]
		}
	}
	return nil
}
