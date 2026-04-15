// Package auth composes authlib building blocks into HandleInbound and
// HandleOutbound — the two functions that all listeners call.
package auth

import (
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

// Inbound actions.
const (
	ActionAllow = "allow"
	ActionDeny  = "deny"
)

// Outbound actions.
const (
	ActionReplaceToken = "replace_token"
	// ActionAllow and ActionDeny are reused from inbound.
)

// No-token outbound policies (set by mode preset).
const (
	NoTokenPolicyClientCredentials = "client-credentials"
	NoTokenPolicyAllow             = "allow"
	NoTokenPolicyDeny              = "deny"
)

// InboundResult is the outcome of inbound JWT validation.
type InboundResult struct {
	Action     string            // ActionAllow or ActionDeny
	Claims     *validation.Claims // non-nil when a valid JWT was present
	DenyStatus int               // HTTP status code (e.g., 401)
	DenyReason string            // human-readable error
}

// OutboundResult is the outcome of outbound token exchange.
type OutboundResult struct {
	Action     string // ActionAllow, ActionReplaceToken, or ActionDeny
	Token      string // replacement token (when Action == ActionReplaceToken)
	DenyStatus int    // HTTP status code (e.g., 503)
	DenyReason string // human-readable error
}
