package pipeline

import (
	"encoding/json"
	"log/slog"
	"time"
)

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

// MarshalJSON emits the string form ("request"/"response") so the wire
// format stays human-readable.
func (p SessionPhase) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

// UnmarshalJSON decodes a SessionPhase from the string form emitted by
// MarshalJSON. Unknown strings decode to SessionRequest (zero value)
// without error — tolerant forward-compat behavior. A Debug-level log
// fires on unknown input so wire-format drift (e.g., a server emitting a
// typo) is at least observable in a verbose test run rather than silent.
func (p *SessionPhase) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "request":
		*p = SessionRequest
	case "response":
		*p = SessionResponse
	default:
		slog.Debug("pipeline: unknown SessionPhase, defaulting to request", "value", s)
		*p = SessionRequest
	}
	return nil
}

// SessionEvent represents a single pipeline event captured by the session store.
// Exactly one of A2A, MCP, or Inference is non-nil.
type SessionEvent struct {
	// SessionID is the session bucket the event was appended to. Populated by
	// Store.Append so downstream consumers (particularly the SSE stream
	// filter) can attribute any event — including outbound MCP/Inference
	// events that have no protocol-native session concept — to a session
	// without needing a side-channel lookup.
	SessionID string

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

	// Host is the HTTP :authority (or Host header) of the event. For inbound
	// events it's the agent's own address; for outbound events it's the
	// target service, which is the useful case — a session with many
	// outbound calls can be attributed to the tool / LLM / target each
	// landed on. Empty when the listener didn't populate pctx.Host.
	Host string

	// TargetAudience is the OAuth audience the outbound request was routed
	// to, or empty when no route matched (passthrough) or the event is
	// inbound. Useful for policy plugins that care which target scope a
	// call was made against, independent of host (which can be a glob).
	TargetAudience string

	// Duration is the wall-clock time from request entry into the listener
	// to response recording. Zero on request-phase events. On response
	// events it's computed as now - matching-request.At.
	Duration time.Duration
}

// MarshalJSON emits SessionEvent in a form consumable by off-process clients:
// Direction and Phase become strings instead of opaque int enums, and Duration
// is emitted as milliseconds (the field name reflects the unit). The default
// json marshaler would stringify enums as numbers and duration as nanoseconds
// — both awkward for CLI / dashboard consumption.
// sessionEventWire is the on-the-wire shape for SessionEvent. MarshalJSON
// writes to it directly; UnmarshalJSON reads into it and converts back.
// Keeping the layout in one place guarantees round-trip symmetry.
type sessionEventWire struct {
	SessionID      string              `json:"sessionId,omitempty"`
	At             time.Time           `json:"at"`
	Direction      Direction           `json:"direction"`
	Phase          SessionPhase        `json:"phase"`
	A2A            *A2AExtension       `json:"a2a,omitempty"`
	MCP            *MCPExtension       `json:"mcp,omitempty"`
	Inference      *InferenceExtension `json:"inference,omitempty"`
	Identity       *EventIdentity      `json:"identity,omitempty"`
	StatusCode     int                 `json:"statusCode,omitempty"`
	Error          *EventError         `json:"error,omitempty"`
	Host           string              `json:"host,omitempty"`
	TargetAudience string              `json:"targetAudience,omitempty"`
	DurationMs     int64               `json:"durationMs,omitempty"`
}

func (e SessionEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(sessionEventWire{
		SessionID:      e.SessionID,
		At:             e.At,
		Direction:      e.Direction,
		Phase:          e.Phase,
		A2A:            e.A2A,
		MCP:            e.MCP,
		Inference:      e.Inference,
		Identity:       e.Identity,
		StatusCode:     e.StatusCode,
		Error:          e.Error,
		Host:           e.Host,
		TargetAudience: e.TargetAudience,
		DurationMs:     e.Duration.Milliseconds(),
	})
}

// UnmarshalJSON accepts the on-the-wire form written by MarshalJSON. This
// makes SessionEvent round-trippable through JSON so off-process clients
// (e.g. abctl) can decode straight into the canonical type.
func (e *SessionEvent) UnmarshalJSON(data []byte) error {
	var w sessionEventWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*e = SessionEvent{
		SessionID:      w.SessionID,
		At:             w.At,
		Direction:      w.Direction,
		Phase:          w.Phase,
		A2A:            w.A2A,
		MCP:            w.MCP,
		Inference:      w.Inference,
		Identity:       w.Identity,
		StatusCode:     w.StatusCode,
		Error:          w.Error,
		Host:           w.Host,
		TargetAudience: w.TargetAudience,
		Duration:       time.Duration(w.DurationMs) * time.Millisecond,
	}
	return nil
}

// EventIdentity carries the "who" for a session event.
type EventIdentity struct {
	Subject  string   `json:"subject,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
	ClientID string   `json:"clientId,omitempty"`
	AgentID  string   `json:"agentId,omitempty"`
}

// EventError describes why a response event represents a failure.
type EventError struct {
	Kind    string `json:"kind"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"` // human-readable reason; safe to surface in logs/metrics
}

// SessionView is a read-only snapshot of a session, safe to pass to plugins.
// It contains a copy of events — plugins cannot mutate the store.
type SessionView struct {
	ID     string         `json:"id"`
	Events []SessionEvent `json:"events"`
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
