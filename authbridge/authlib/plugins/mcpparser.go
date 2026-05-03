package plugins

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// MCPParser parses MCP JSON-RPC 2.0 request bodies and populates
// pctx.Extensions.MCP with the parsed method, tool name, resource URI,
// or prompt name for downstream policy plugins.
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

	ext := &pipeline.MCPExtension{
		Method: rpc.Method,
		RPCID:  rpc.ID,
	}

	switch rpc.Method {
	case "tools/call":
		ext.Tool = &pipeline.MCPToolMetadata{
			Name: rpc.stringParam("name"),
			Args: rpc.mapParam("arguments"),
		}
		slog.Info("mcp-parser: parsed tools/call", "tool", ext.Tool.Name)
	case "resources/read":
		ext.Resource = &pipeline.MCPResourceMetadata{
			URI: rpc.stringParam("uri"),
		}
		slog.Info("mcp-parser: parsed resources/read", "uri", ext.Resource.URI)
	case "prompts/get":
		ext.Prompt = &pipeline.MCPPromptMetadata{
			Name: rpc.stringParam("name"),
			Args: rpc.stringMapParam("arguments"),
		}
		slog.Info("mcp-parser: parsed prompts/get", "prompt", ext.Prompt.Name)
	default:
		slog.Debug("mcp-parser: untracked method", "method", rpc.Method)
	}

	pctx.Extensions.MCP = ext
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

func (r *jsonRPCRequest) stringMapParam(key string) map[string]string {
	raw, ok := r.Params[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
