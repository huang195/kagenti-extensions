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
