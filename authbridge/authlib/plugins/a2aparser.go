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

	slog.Debug("a2a-parser: processing request body", "bodyLen", len(pctx.Body))

	var rpc jsonRPCRequest
	if err := json.Unmarshal(pctx.Body, &rpc); err != nil {
		slog.Debug("a2a-parser: body is not valid JSON-RPC", "error", err, "bodyLen", len(pctx.Body))
		return pipeline.Action{Type: pipeline.Continue}
	}

	slog.Debug("a2a-parser: decoded JSON-RPC", "method", rpc.Method, "id", rpc.ID)

	ext := &pipeline.A2AExtension{
		Method: rpc.Method,
		RPCID:  rpc.ID,
	}

	if rpc.Method == "message/send" || rpc.Method == "message/stream" {
		ext.SessionID = rpc.stringParam("sessionId")
		slog.Debug("a2a-parser: session", "sessionId", ext.SessionID)

		msg := rpc.mapParam("message")
		if msg != nil {
			if role, ok := msg["role"].(string); ok {
				ext.Role = role
			}
			if messageID, ok := msg["messageId"].(string); ok {
				ext.MessageID = messageID
			}
			if rawParts, ok := msg["parts"].([]any); ok {
				ext.Parts = parseA2AParts(rawParts)
			}
			slog.Debug("a2a-parser: message fields",
				"role", ext.Role,
				"messageId", ext.MessageID,
				"parts", len(ext.Parts),
			)
		} else {
			slog.Debug("a2a-parser: no message object in params")
		}

		slog.Info("a2a-parser: parsed "+rpc.Method,
			"sessionId", ext.SessionID,
			"role", ext.Role,
			"parts", len(ext.Parts),
		)
	} else {
		slog.Debug("a2a-parser: untracked method", "method", rpc.Method)
	}

	pctx.Extensions.A2A = ext
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *A2AParser) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func parseA2AParts(rawParts []any) []pipeline.A2APart {
	slog.Debug("a2a-parser: parsing parts", "count", len(rawParts))
	parts := make([]pipeline.A2APart, 0, len(rawParts))
	for i, raw := range rawParts {
		partMap, ok := raw.(map[string]any)
		if !ok {
			slog.Debug("a2a-parser: skipping non-map part", "index", i)
			continue
		}
		kind, _ := partMap["kind"].(string)
		var content string
		switch kind {
		case "text":
			content, _ = partMap["text"].(string)
		case "file":
			content, _ = partMap["data"].(string)
		case "data":
			if dataVal, ok := partMap["data"]; ok {
				if b, err := json.Marshal(dataVal); err == nil {
					content = string(b)
				}
			}
		}
		slog.Debug("a2a-parser: part extracted",
			"index", i,
			"kind", kind,
			"contentLen", len(content),
		)
		parts = append(parts, pipeline.A2APart{Kind: kind, Content: content})
	}
	return parts
}
