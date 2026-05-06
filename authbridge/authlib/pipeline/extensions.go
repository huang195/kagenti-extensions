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
// Result and Err are mutually exclusive: a response sets exactly one.
type MCPExtension struct {
	Method string         `json:"method,omitempty"`
	RPCID  any            `json:"rpcId,omitempty"`
	Params map[string]any `json:"params,omitempty"`
	Result map[string]any `json:"result,omitempty"`
	Err    *MCPError      `json:"error,omitempty"`
}

// MCPError mirrors a JSON-RPC 2.0 error object.
type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// A2AExtension carries parsed A2A protocol metadata from inbound requests
// and response summaries for debugging.
type A2AExtension struct {
	// Request fields
	Method    string    `json:"method,omitempty"`
	RPCID     any       `json:"rpcId,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	MessageID string    `json:"messageId,omitempty"`
	TaskID    string    `json:"taskId,omitempty"`
	Role      string    `json:"role,omitempty"`
	Parts     []A2APart `json:"parts,omitempty"`

	// Response fields (populated by a2a-parser OnResponse)
	FinalStatus  string `json:"finalStatus,omitempty"`  // "completed", "failed", "canceled"
	Artifact     string `json:"artifact,omitempty"`     // final artifact text
	ErrorMessage string `json:"errorMessage,omitempty"` // failure reason if status is "failed"
}

// A2APart represents a message part in an A2A request.
type A2APart struct {
	Kind    string `json:"kind"`
	Content string `json:"content,omitempty"`
}

// InferenceExtension carries parsed LLM inference request and response metadata.
// Request fields are populated by OnRequest; response fields by OnResponse.
type InferenceExtension struct {
	Model       string             `json:"model,omitempty"`
	Messages    []InferenceMessage `json:"messages,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	MaxTokens   *int               `json:"maxTokens,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Tools       []string           `json:"tools,omitempty"`

	// Response fields (populated after OnResponse runs).
	Completion       string `json:"completion,omitempty"`
	FinishReason     string `json:"finishReason,omitempty"`
	PromptTokens     int    `json:"promptTokens,omitempty"`
	CompletionTokens int    `json:"completionTokens,omitempty"`
	TotalTokens      int    `json:"totalTokens,omitempty"`
}

// InferenceMessage represents a single message in the conversation.
type InferenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
}

// SecurityExtension carries guardrail output.
// Caller identity is already in ctx.Agent and ctx.Claims — this slot is only
// for downstream signals from content-inspection plugins.
type SecurityExtension struct {
	Labels      []string `json:"labels,omitempty"`
	Blocked     bool     `json:"blocked,omitempty"`
	BlockReason string   `json:"blockReason,omitempty"`
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
