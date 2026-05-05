package pipeline

import "time"

// Extensions holds typed extension slots for plugin-to-plugin communication.
// Each slot is populated by a specific plugin and consumed by downstream plugins.
type Extensions struct {
	MCP        *MCPExtension
	A2A        *A2AExtension
	Security   *SecurityExtension
	Delegation *DelegationExtension
	Inference  *InferenceExtension
	Custom     map[string]any
}

// MCPExtension carries parsed MCP JSON-RPC metadata.
type MCPExtension struct {
	Method string         // JSON-RPC method (e.g. "tools/call", "resources/read", "initialize")
	RPCID  any            // JSON-RPC id for request-response correlation
	Params map[string]any // raw params from the JSON-RPC request
	Result map[string]any // parsed result from the JSON-RPC response (nil until OnResponse runs)
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

// InferenceExtension carries parsed LLM inference request and response metadata.
// Request fields are populated by OnRequest; response fields by OnResponse.
type InferenceExtension struct {
	Model       string             // model name (e.g., "llama3.1", "gpt-4")
	Messages    []InferenceMessage // conversation messages
	Temperature *float64           // sampling temperature (nil if not set)
	MaxTokens   *int               // max tokens to generate (nil if not set)
	Stream      bool               // whether streaming is requested
	Tools       []string           // tool/function names declared

	// Response fields (populated after OnResponse runs).
	Completion       string // assistant's response text (concatenated across SSE deltas)
	FinishReason     string // "stop", "length", "tool_calls", "content_filter", etc.
	PromptTokens     int    // tokens consumed by the prompt
	CompletionTokens int    // tokens generated in the response
	TotalTokens      int    // PromptTokens + CompletionTokens (as reported by the server)
}

// InferenceMessage represents a single message in the conversation.
type InferenceMessage struct {
	Role    string // "system", "user", "assistant", "tool"
	Content string
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
