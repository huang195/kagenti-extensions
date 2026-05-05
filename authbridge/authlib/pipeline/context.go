package pipeline

import (
	"net/http"

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

// Context is the shared state passed through the plugin pipeline.
// Plugins read and mutate fields directly — there is no separate mutation API.
type Context struct {
	Direction Direction
	Method    string
	Host      string
	Path      string
	Headers   http.Header
	Body      []byte // nil unless at least one plugin declares BodyAccess: true

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
