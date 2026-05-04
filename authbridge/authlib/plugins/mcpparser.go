package plugins

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// MCPParser parses MCP JSON-RPC 2.0 request bodies and populates
// pctx.Extensions.MCP with the method, RPC ID, and raw params for
// downstream policy plugins.
type MCPParser struct{}

func NewMCPParser() *MCPParser { return &MCPParser{} }

func (p *MCPParser) Name() string { return "mcp-parser" }

func (p *MCPParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Writes:     []string{"mcp"},
		BodyAccess: true,
	}
}

func (p *MCPParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if len(pctx.Body) == 0 {
		slog.Debug("mcp-parser: no body, skipping")
		return pipeline.Action{Type: pipeline.Continue}
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(pctx.Body, &rpc); err != nil {
		slog.Debug("mcp-parser: body is not valid JSON-RPC", "error", err, "bodyLen", len(pctx.Body))
		return pipeline.Action{Type: pipeline.Continue}
	}

	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: rpc.Method,
		RPCID:  rpc.ID,
		Params: rpc.Params,
	}

	slog.Info("mcp-parser: request", "method", rpc.Method)
	slog.Debug("mcp-parser: payload", "method", rpc.Method, "body", truncate(string(pctx.Body), 128))

	return pipeline.Action{Type: pipeline.Continue}
}

func (p *MCPParser) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

type jsonRPCRequest struct {
	Method string         `json:"method"`
	ID     any            `json:"id"`
	Params map[string]any `json:"params"`
}

func (r *jsonRPCRequest) stringParam(key string) string {
	if v, ok := r.Params[key].(string); ok {
		return v
	}
	return ""
}

func (r *jsonRPCRequest) mapParam(key string) map[string]any {
	if v, ok := r.Params[key].(map[string]any); ok {
		return v
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
