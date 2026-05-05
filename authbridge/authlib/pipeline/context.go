package pipeline

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)


// Direction indicates whether a request is inbound (caller → this agent) or
// outbound (this agent → target service).
type Direction int

const (
	Inbound Direction = iota
	Outbound
)

// String returns "inbound" / "outbound". Used for structured logs and the
// wire format of SessionEvent.
func (d Direction) String() string {
	switch d {
	case Inbound:
		return "inbound"
	case Outbound:
		return "outbound"
	default:
		return "unknown"
	}
}

// MarshalJSON emits the string form ("inbound"/"outbound") so the wire
// format is human-readable without an enum→int lookup.
func (d Direction) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes a Direction from the string form emitted by
// MarshalJSON. Unknown strings decode to Inbound (zero value) without
// error so downstream consumers stay tolerant of forward-compatible
// additions.
func (d *Direction) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "outbound":
		*d = Outbound
	default:
		*d = Inbound
	}
	return nil
}

// Context is the shared state passed through the plugin pipeline.
// Plugins read and mutate fields directly — there is no separate mutation API.
type Context struct {
	Direction Direction
	Method    string
	Host      string
	Path      string
	Headers   http.Header
	Body      []byte // nil unless at least one plugin declares BodyAccess: true

	// StartedAt is the wall-clock time this context was constructed by the
	// listener at the start of a request. Used on the response path to
	// compute SessionEvent.Duration without walking the event history.
	StartedAt time.Time

	Agent   *AgentIdentity
	Claims  *validation.Claims    // nil before jwt-validation runs
	Route   *routing.ResolvedRoute
	Session *SessionView // nil unless session tracking is enabled

	// Response-phase fields (populated by listener before RunResponse).
	// ResponseBody may be nil even during response phase if no plugin declared BodyAccess.
	StatusCode      int
	ResponseHeaders http.Header
	ResponseBody    []byte

	Extensions Extensions
}

// AgentIdentity carries the agent's own workload identity.
type AgentIdentity struct {
	ClientID    string
	WorkloadID  string
	TrustDomain string
}
