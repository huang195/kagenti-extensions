package plugins

import (
	"context"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

func TestMCPParser_Capabilities(t *testing.T) {
	p := NewMCPParser()

	if p.Name() != "mcp-parser" {
		t.Errorf("Name() = %q, want %q", p.Name(), "mcp-parser")
	}

	caps := p.Capabilities()
	if !caps.BodyAccess {
		t.Error("BodyAccess should be true")
	}
	if len(caps.Writes) != 1 || caps.Writes[0] != "mcp" {
		t.Errorf("Writes = %v, want [mcp]", caps.Writes)
	}
}

func TestMCPParser_ToolCall(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"get_weather","arguments":{"city":"NYC"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "tools/call" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "tools/call")
	}
	if pctx.Extensions.MCP.RPCID != float64(1) {
		t.Errorf("RPCID = %v, want 1", pctx.Extensions.MCP.RPCID)
	}
	if pctx.Extensions.MCP.Params["name"] != "get_weather" {
		t.Errorf("Params[name] = %v, want %q", pctx.Extensions.MCP.Params["name"], "get_weather")
	}
	args, ok := pctx.Extensions.MCP.Params["arguments"].(map[string]any)
	if !ok {
		t.Fatal("Params[arguments] is not a map")
	}
	if args["city"] != "NYC" {
		t.Errorf("Params[arguments][city] = %v, want %q", args["city"], "NYC")
	}
}

func TestMCPParser_ResourceRead(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"resources/read","id":2,"params":{"uri":"file:///tmp/data.csv"}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "resources/read" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "resources/read")
	}
	if pctx.Extensions.MCP.Params["uri"] != "file:///tmp/data.csv" {
		t.Errorf("Params[uri] = %v, want %q", pctx.Extensions.MCP.Params["uri"], "file:///tmp/data.csv")
	}
}

func TestMCPParser_PromptGet(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"prompts/get","id":3,"params":{"name":"summarize","arguments":{"style":"brief"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "prompts/get" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "prompts/get")
	}
	if pctx.Extensions.MCP.Params["name"] != "summarize" {
		t.Errorf("Params[name] = %v, want %q", pctx.Extensions.MCP.Params["name"], "summarize")
	}
	args, ok := pctx.Extensions.MCP.Params["arguments"].(map[string]any)
	if !ok {
		t.Fatal("Params[arguments] is not a map")
	}
	if args["style"] != "brief" {
		t.Errorf("Params[arguments][style] = %v, want %q", args["style"], "brief")
	}
}

func TestMCPParser_AnyMethod(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"notifications/initialized","id":4}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "notifications/initialized" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "notifications/initialized")
	}
}

func TestMCPParser_NilBody(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{Body: nil}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Error("Extensions.MCP should be nil when body is nil")
	}
}

func TestMCPParser_EmptyBody(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{Body: []byte{}}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Error("Extensions.MCP should be nil when body is empty")
	}
}

func TestMCPParser_InvalidJSON(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{Body: []byte("not json")}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Error("Extensions.MCP should be nil for invalid JSON")
	}
}

func TestMCPParser_MissingParams(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"tools/call","id":5}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.MCP == nil {
		t.Fatal("Extensions.MCP is nil")
	}
	if pctx.Extensions.MCP.Method != "tools/call" {
		t.Errorf("Method = %q, want %q", pctx.Extensions.MCP.Method, "tools/call")
	}
	if pctx.Extensions.MCP.Params != nil {
		t.Errorf("Params = %v, want nil when params not present", pctx.Extensions.MCP.Params)
	}
}

func TestMCPParser_OnResponse(t *testing.T) {
	p := NewMCPParser()
	action := p.OnResponse(context.Background(), &pipeline.Context{})
	if action.Type != pipeline.Continue {
		t.Errorf("OnResponse should return Continue, got %v", action.Type)
	}
}
