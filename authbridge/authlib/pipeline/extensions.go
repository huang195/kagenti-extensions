package pipeline

import "time"

// Extensions holds typed extension slots for plugin-to-plugin communication.
// Each slot is populated by a specific plugin and consumed by downstream plugins.
type Extensions struct {
	MCP        *MCPExtension
	A2A        *A2AExtension
	Security   *SecurityExtension
	Delegation *DelegationExtension
	Custom     map[string]any
}

// MCPExtension carries parsed MCP JSON-RPC metadata.
// Exactly one of Tool, Resource, or Prompt is populated per request.
type MCPExtension struct {
	Method string // JSON-RPC method: "tools/call", "resources/read", "prompts/get"
	RPCID  any    // JSON-RPC id for request-response correlation

	Tool     *MCPToolMetadata
	Resource *MCPResourceMetadata
	Prompt   *MCPPromptMetadata
}

// MCPToolMetadata is populated for tools/call requests.
type MCPToolMetadata struct {
	Name string
	Args map[string]any
}

// MCPResourceMetadata is populated for resources/read requests.
type MCPResourceMetadata struct {
	URI string
}

// MCPPromptMetadata is populated for prompts/get requests.
type MCPPromptMetadata struct {
	Name string
	Args map[string]string
}

// A2AExtension carries parsed A2A protocol metadata from inbound requests.
type A2AExtension struct {
	Method    string // JSON-RPC method: "message/send", "message/stream"
	RPCID     any    // JSON-RPC id for request-response correlation
	SessionID string // conversation session (from params.sessionId)
	MessageID string // unique message ID (from params.message.messageId)
	Role      string // "user" or "assistant"
	Parts     []A2APart
}

// A2APart represents a message part in an A2A request.
type A2APart struct {
	Kind    string // "text", "file", "data"
	Content string // text content, file URI, or serialized data
}

// SecurityExtension carries guardrail output.
// Caller identity is already in ctx.Agent and ctx.Claims — this slot is only
// for downstream signals from content-inspection plugins.
type SecurityExtension struct {
	Labels      []string
	Blocked     bool
	BlockReason string
}

// DelegationExtension tracks the token delegation chain across hops.
// The chain is append-only and unexported to prevent forgery or truncation.
type DelegationExtension struct {
	chain  []DelegationHop
	Origin string // original caller's subject ID
	Actor  string // current actor's subject ID
}

// Chain returns a copy of the delegation chain. The copy prevents callers from
// mutating the backing slice (truncation, reordering, forgery).
func (d *DelegationExtension) Chain() []DelegationHop {
	out := make([]DelegationHop, len(d.chain))
	copy(out, d.chain)
	return out
}

// Depth returns the number of hops in the delegation chain.
func (d *DelegationExtension) Depth() int {
	return len(d.chain)
}

// DelegationHop represents one hop in the delegation chain.
type DelegationHop struct {
	SubjectID string
	Scopes    []string
	Timestamp time.Time
}

// AppendHop adds a hop to the delegation chain. This is the only way to extend
// the chain — direct mutation is prevented by the unexported slice.
//
// AppendHop is not safe for concurrent use. The pipeline guarantees sequential
// invocation.
func (d *DelegationExtension) AppendHop(hop DelegationHop) {
	d.chain = append(d.chain, hop)
	if d.Origin == "" {
		d.Origin = hop.SubjectID
	}
	d.Actor = hop.SubjectID
}
