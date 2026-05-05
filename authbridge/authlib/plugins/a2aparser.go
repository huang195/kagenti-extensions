package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// A2AParser parses A2A JSON-RPC 2.0 request bodies and populates
// pctx.Extensions.A2A with the parsed method, session ID, message parts,
// and role for downstream policy plugins (e.g., guardrails).
type A2AParser struct{}

func NewA2AParser() *A2AParser { return &A2AParser{} }

func (p *A2AParser) Name() string { return "a2a-parser" }

func (p *A2AParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Writes:     []string{"a2a"},
		BodyAccess: true,
	}
}

func (p *A2AParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if len(pctx.Body) == 0 {
		slog.Debug("a2a-parser: no body, skipping")
		return pipeline.Action{Type: pipeline.Continue}
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(pctx.Body, &rpc); err != nil {
		slog.Debug("a2a-parser: invalid JSON-RPC", "error", err, "bodyLen", len(pctx.Body))
		return pipeline.Action{Type: pipeline.Continue}
	}

	ext := &pipeline.A2AExtension{
		Method: rpc.Method,
		RPCID:  rpc.ID,
	}

	// Extract message fields generically — any method with params.message
	// gets full extraction (forward-compatible with future A2A methods).
	// A2A spec uses "contextId" (current) or "sessionId" (older drafts).
	ext.SessionID = rpc.stringParam("contextId")
	if ext.SessionID == "" {
		ext.SessionID = rpc.stringParam("sessionId")
	}
	if msg := rpc.mapParam("message"); msg != nil {
		if role, ok := msg["role"].(string); ok {
			ext.Role = role
		}
		if messageID, ok := msg["messageId"].(string); ok {
			ext.MessageID = messageID
		}
		if rawParts, ok := msg["parts"].([]any); ok {
			ext.Parts = parseA2AParts(rawParts)
		}
	}

	pctx.Extensions.A2A = ext

	slog.Info("a2a-parser", "method", rpc.Method)
	slog.Debug("a2a-parser: extracted",
		"method", rpc.Method,
		"sessionId", ext.SessionID,
		"role", ext.Role,
		"messageId", ext.MessageID,
		"parts", len(ext.Parts),
	)
	for i, part := range ext.Parts {
		slog.Debug("a2a-parser: part", "index", i, "kind", part.Kind, "content", truncate(part.Content, debugBodyMax))
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse extracts the server-assigned session/context ID from the response body
// and stores it on the A2A extension. This lets downstream plugins correlate the
// freshly-assigned session for the next turn of the conversation, since clients
// typically don't include a contextId on the first message.
//
// Handles both JSON-RPC responses (message/send) and SSE event streams (message/stream).
func (p *A2AParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if len(pctx.ResponseBody) == 0 || pctx.Extensions.A2A == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}

	sid := extractSessionID(pctx.ResponseBody)
	if sid == "" {
		slog.Debug("a2a-parser: no sessionId or contextId found in response")
		return pipeline.Action{Type: pipeline.Continue}
	}

	pctx.Extensions.A2A.SessionID = sid
	slog.Debug("a2a-parser: response sessionId", "sessionId", sid)
	slog.Debug("a2a-parser: response body", "body", truncate(string(pctx.ResponseBody), debugBodyMax))
	return pipeline.Action{Type: pipeline.Continue}
}

// extractSessionID finds a contextId (preferred) or sessionId in the response.
// Supports plain JSON-RPC responses and SSE event streams (message/stream).
func extractSessionID(body []byte) string {
	if sid := sessionIDFromJSON(body); sid != "" {
		return sid
	}
	// SSE format: scan "data:" lines for the first event that carries a session ID.
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if sid := sessionIDFromJSON(data); sid != "" {
			return sid
		}
	}
	return ""
}

func sessionIDFromJSON(data []byte) string {
	var resp struct {
		Result struct {
			ContextID string `json:"contextId"`
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &resp) != nil {
		return ""
	}
	if resp.Result.ContextID != "" {
		return resp.Result.ContextID
	}
	return resp.Result.SessionID
}

func parseA2AParts(rawParts []any) []pipeline.A2APart {
	parts := make([]pipeline.A2APart, 0, len(rawParts))
	for _, raw := range rawParts {
		partMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, ok := partMap["kind"].(string)
		if !ok || kind == "" {
			continue
		}
		var content string
		switch kind {
		case "text":
			content, _ = partMap["text"].(string)
		case "file":
			// TODO: update when A2A spec stabilizes — canonical Part uses mediaType + content field presence, not "kind".
			content, _ = partMap["data"].(string)
			if content == "" {
				content, _ = partMap["uri"].(string)
			}
		case "data":
			if dataVal, ok := partMap["data"]; ok && dataVal != nil {
				if b, err := json.Marshal(dataVal); err == nil {
					content = string(b)
				}
			}
		}
		parts = append(parts, pipeline.A2APart{Kind: kind, Content: content})
	}
	return parts
}
