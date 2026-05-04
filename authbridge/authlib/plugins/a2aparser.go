package plugins

import (
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
	ext.SessionID = rpc.stringParam("sessionId")
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
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *A2AParser) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
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
			content, _ = partMap["data"].(string)
			if content == "" {
				content, _ = partMap["uri"].(string)
			}
		case "data":
			if dataVal, ok := partMap["data"]; ok {
				if b, err := json.Marshal(dataVal); err == nil {
					content = string(b)
				}
			}
		}
		parts = append(parts, pipeline.A2APart{Kind: kind, Content: content})
	}
	return parts
}
