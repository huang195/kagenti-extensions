package plugins

import (
	"context"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

func TestInferenceParser_Capabilities(t *testing.T) {
	p := NewInferenceParser()

	if p.Name() != "inference-parser" {
		t.Errorf("Name() = %q, want %q", p.Name(), "inference-parser")
	}

	caps := p.Capabilities()
	if !caps.BodyAccess {
		t.Error("BodyAccess should be true")
	}
	if len(caps.Writes) != 1 || caps.Writes[0] != "inference" {
		t.Errorf("Writes = %v, want [inference]", caps.Writes)
	}
}

func TestInferenceParser_ChatCompletions(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{
			"model": "llama3.1",
			"messages": [
				{"role": "system", "content": "You are a helpful assistant."},
				{"role": "user", "content": "What is the weather in NYC?"}
			],
			"temperature": 0.7,
			"max_tokens": 1024,
			"stream": false,
			"tools": [
				{"type": "function", "function": {"name": "get_weather"}},
				{"type": "function", "function": {"name": "get_forecast"}}
			]
		}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if ext.Model != "llama3.1" {
		t.Errorf("Model = %q, want %q", ext.Model, "llama3.1")
	}
	if len(ext.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(ext.Messages))
	}
	if ext.Messages[0].Role != "system" || ext.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("Messages[0] = %+v", ext.Messages[0])
	}
	if ext.Messages[1].Role != "user" || ext.Messages[1].Content != "What is the weather in NYC?" {
		t.Errorf("Messages[1] = %+v", ext.Messages[1])
	}
	if ext.Temperature == nil || *ext.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", ext.Temperature)
	}
	if ext.MaxTokens == nil || *ext.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %v, want 1024", ext.MaxTokens)
	}
	if ext.Stream {
		t.Error("Stream should be false")
	}
	if len(ext.Tools) != 2 || ext.Tools[0] != "get_weather" || ext.Tools[1] != "get_forecast" {
		t.Errorf("Tools = %v, want [get_weather get_forecast]", ext.Tools)
	}
}

func TestInferenceParser_StreamRequest(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{"model": "gpt-4", "messages": [{"role": "user", "content": "hi"}], "stream": true}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if !pctx.Extensions.Inference.Stream {
		t.Error("Stream should be true")
	}
}

func TestInferenceParser_SystemMessage(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{
			"model": "llama3.1",
			"messages": [
				{"role": "system", "content": "You are a weather expert."},
				{"role": "user", "content": "Tell me about hurricanes."},
				{"role": "assistant", "content": "Hurricanes are..."},
				{"role": "user", "content": "What about typhoons?"}
			]
		}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if len(ext.Messages) != 4 {
		t.Fatalf("Messages len = %d, want 4", len(ext.Messages))
	}
	if ext.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want %q", ext.Messages[0].Role, "system")
	}
	if ext.Messages[2].Role != "assistant" {
		t.Errorf("Messages[2].Role = %q, want %q", ext.Messages[2].Role, "assistant")
	}
}

func TestInferenceParser_WithTools(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte(`{
			"model": "llama3.1",
			"messages": [{"role": "user", "content": "check weather"}],
			"tools": [
				{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}},
				{"type": "function", "function": {"name": "search_web"}}
			]
		}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if len(ext.Tools) != 2 {
		t.Fatalf("Tools len = %d, want 2", len(ext.Tools))
	}
	if ext.Tools[0] != "get_weather" {
		t.Errorf("Tools[0] = %q, want %q", ext.Tools[0], "get_weather")
	}
	if ext.Tools[1] != "search_web" {
		t.Errorf("Tools[1] = %q, want %q", ext.Tools[1], "search_web")
	}
}

func TestInferenceParser_NonMatchingPath(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/api/other",
		Body: []byte(`{"model": "llama3.1", "messages": [{"role": "user", "content": "hi"}]}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Extensions.Inference should be nil for non-matching path")
	}
}

func TestInferenceParser_NilBody(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: nil,
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Extensions.Inference should be nil when body is nil")
	}
}

func TestInferenceParser_EmptyBody(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte{},
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Extensions.Inference should be nil when body is empty")
	}
}

func TestInferenceParser_InvalidJSON(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Body: []byte("not valid json"),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.Inference != nil {
		t.Error("Extensions.Inference should be nil for invalid JSON")
	}
}

func TestInferenceParser_LegacyCompletions(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/completions",
		Body: []byte(`{"model": "codellama", "prompt": "Write a function that", "max_tokens": 256}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil")
	}
	if ext.Model != "codellama" {
		t.Errorf("Model = %q, want %q", ext.Model, "codellama")
	}
	if ext.MaxTokens == nil || *ext.MaxTokens != 256 {
		t.Errorf("MaxTokens = %v, want 256", ext.MaxTokens)
	}
}

func TestInferenceParser_OnResponse(t *testing.T) {
	p := NewInferenceParser()
	action := p.OnResponse(context.Background(), &pipeline.Context{})
	if action.Type != pipeline.Continue {
		t.Errorf("OnResponse should return Continue, got %v", action.Type)
	}
}
