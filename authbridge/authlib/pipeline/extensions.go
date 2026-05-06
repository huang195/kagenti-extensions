package pipeline

import "time"

// Extensions holds typed extension slots for plugin-to-plugin communication.
// Each slot is populated by a specific plugin and consumed by downstream plugins.
//
// The named slots (MCP, A2A, Security, Delegation, Inference) are reserved
// for telemetry-worthy extensions — data that flows into SessionEvent, is
// serialized on the wire API, and has a published schema that unrelated
// plugins can rely on. Adding a new named slot is a core-library change.
//
// For plugin-private, per-request state that doesn't need a published
// schema, use the generic GetState / SetState helpers defined below; they
// store values in Custom keyed by plugin name, letting a new plugin land
// without any authlib modification.
type Extensions struct {
	MCP        *MCPExtension
	A2A        *A2AExtension
	Security   *SecurityExtension
	Delegation *DelegationExtension
	Inference  *InferenceExtension
	Custom     map[string]any
}

// SetState stashes a typed value on pctx under key. Intended for plugin-
// private per-request state — e.g., a rate-limiter remembering how many
// tokens were available when OnRequest saw the call, for OnResponse to
// consult. The generic type parameter is documentary: it forces callers
// to pass *T rather than an unrelated interface, which pairs with the
// symmetric type-assert in GetState.
//
// Convention: `key` should be the plugin's Name() so keys from unrelated
// plugins don't collide. SetState is not safe for concurrent use — pctx
// is single-threaded per request in the pipeline.
func SetState[T any](pctx *Context, key string, v *T) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[key] = v
}

// GetState retrieves a typed value previously stored via SetState. Returns
// nil when the key is absent or when the stored value is not a *T —
// safe-fails rather than panicking so a mid-pipeline type migration
// (plugin version skew) degrades instead of crashing the handler.
func GetState[T any](pctx *Context, key string) *T {
	if pctx.Extensions.Custom == nil {
		return nil
	}
	v, ok := pctx.Extensions.Custom[key].(*T)
	if !ok {
		return nil
	}
	return v
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
	TopP        *float64           `json:"topP,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Tools       []InferenceTool    `json:"tools,omitempty"`
	ToolChoice  any                `json:"toolChoice,omitempty"` // "auto" | "none" | {type,function:{name}}

	// Response fields (populated after OnResponse runs).
	Completion       string              `json:"completion,omitempty"`
	FinishReason     string              `json:"finishReason,omitempty"`
	PromptTokens     int                 `json:"promptTokens,omitempty"`
	CompletionTokens int                 `json:"completionTokens,omitempty"`
	TotalTokens      int                 `json:"totalTokens,omitempty"`
	ToolCalls        []InferenceToolCall `json:"toolCalls,omitempty"`
}

// InferenceMessage represents a single message in the conversation.
type InferenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
}

// InferenceTool is a function/tool the client declared the model may call.
// Parameters is the OpenAI-style JSON Schema object describing valid args.
type InferenceTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// InferenceToolCall is a tool invocation the model emitted in its response.
// Arguments is the raw JSON string as returned by the LLM (often needs
// json.Unmarshal by the caller) — kept as a string so malformed output
// from the model doesn't prevent capture.
type InferenceToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
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
