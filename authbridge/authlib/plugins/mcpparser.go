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

func (p *MCPParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if len(pctx.ResponseBody) == 0 || pctx.Extensions.MCP == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}

	var rpc jsonRPCResponse
	if err := json.Unmarshal(pctx.ResponseBody, &rpc); err != nil {
		slog.Debug("mcp-parser: response is not valid JSON-RPC", "error", err, "bodyLen", len(pctx.ResponseBody))
		return pipeline.Action{Type: pipeline.Continue}
	}

	if rpc.Error != nil {
		slog.Info("mcp-parser: response error", "method", pctx.Extensions.MCP.Method, "error", rpc.Error)
		pctx.Extensions.MCP.Result = rpc.Error
		return pipeline.Action{Type: pipeline.Continue}
	}

	if rpc.Result != nil {
		pctx.Extensions.MCP.Result = rpc.Result
		slog.Info("mcp-parser: response", "method", pctx.Extensions.MCP.Method, "resultKeys", resultKeys(rpc.Result))
		slog.Debug("mcp-parser: response detail", "method", pctx.Extensions.MCP.Method, "body", truncate(string(pctx.ResponseBody), 256))
	}

	return pipeline.Action{Type: pipeline.Continue}
}

type jsonRPCResponse struct {
	ID     any            `json:"id"`
	Result map[string]any `json:"result"`
	Error  map[string]any `json:"error"`
}

func resultKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

type jsonRPCRequest struct {
	Method string         `json:"method"`
	ID     any            `json:"id"`
	Params map[string]any `json:"params"`
}

// stringParam and mapParam are shared helpers used by both mcp-parser and a2a-parser.
func (r *jsonRPCRequest) stringParam(key string) string {
	v, _ := r.Params[key].(string)
	return v
}

func (r *jsonRPCRequest) mapParam(key string) map[string]any {
	v, _ := r.Params[key].(map[string]any)
	return v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
