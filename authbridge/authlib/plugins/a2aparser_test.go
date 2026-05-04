package plugins

import (
	"context"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

func TestA2AParser_Capabilities(t *testing.T) {
	p := NewA2AParser()

	if p.Name() != "a2a-parser" {
		t.Errorf("Name() = %q, want %q", p.Name(), "a2a-parser")
	}

	caps := p.Capabilities()
	if !caps.BodyAccess {
		t.Error("BodyAccess should be true")
	}
	if len(caps.Writes) != 1 || caps.Writes[0] != "a2a" {
		t.Errorf("Writes = %v, want [a2a]", caps.Writes)
	}
}

func TestA2AParser_MessageSend(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/send","id":"req-1","params":{"message":{"role":"user","parts":[{"kind":"text","text":"Hello agent"}],"messageId":"msg-001"},"sessionId":"sess-abc"}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.A2A == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	ext := pctx.Extensions.A2A
	if ext.Method != "message/send" {
		t.Errorf("Method = %q, want %q", ext.Method, "message/send")
	}
	if ext.RPCID != "req-1" {
		t.Errorf("RPCID = %v, want %q", ext.RPCID, "req-1")
	}
	if ext.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", ext.SessionID, "sess-abc")
	}
	if ext.MessageID != "msg-001" {
		t.Errorf("MessageID = %q, want %q", ext.MessageID, "msg-001")
	}
	if ext.Role != "user" {
		t.Errorf("Role = %q, want %q", ext.Role, "user")
	}
	if len(ext.Parts) != 1 {
		t.Fatalf("Parts len = %d, want 1", len(ext.Parts))
	}
	if ext.Parts[0].Kind != "text" {
		t.Errorf("Parts[0].Kind = %q, want %q", ext.Parts[0].Kind, "text")
	}
	if ext.Parts[0].Content != "Hello agent" {
		t.Errorf("Parts[0].Content = %q, want %q", ext.Parts[0].Content, "Hello agent")
	}
}

func TestA2AParser_MessageStream(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/stream","id":42,"params":{"message":{"role":"user","parts":[{"kind":"text","text":"What is the weather?"}],"messageId":"msg-002"},"sessionId":"sess-xyz"}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if ext.Method != "message/stream" {
		t.Errorf("Method = %q, want %q", ext.Method, "message/stream")
	}
	if ext.RPCID != float64(42) {
		t.Errorf("RPCID = %v, want 42", ext.RPCID)
	}
	if ext.SessionID != "sess-xyz" {
		t.Errorf("SessionID = %q, want %q", ext.SessionID, "sess-xyz")
	}
}

func TestA2AParser_MultipleParts(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/send","id":"req-3","params":{"message":{"role":"user","parts":[{"kind":"text","text":"First"},{"kind":"text","text":"Second"}],"messageId":"msg-003"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if len(ext.Parts) != 2 {
		t.Fatalf("Parts len = %d, want 2", len(ext.Parts))
	}
	if ext.Parts[0].Content != "First" {
		t.Errorf("Parts[0].Content = %q, want %q", ext.Parts[0].Content, "First")
	}
	if ext.Parts[1].Content != "Second" {
		t.Errorf("Parts[1].Content = %q, want %q", ext.Parts[1].Content, "Second")
	}
}

func TestA2AParser_FilePart(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/send","id":"req-4","params":{"message":{"role":"user","parts":[{"kind":"file","data":"base64-encoded-content"}],"messageId":"msg-004"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if len(ext.Parts) != 1 {
		t.Fatalf("Parts len = %d, want 1", len(ext.Parts))
	}
	if ext.Parts[0].Kind != "file" {
		t.Errorf("Parts[0].Kind = %q, want %q", ext.Parts[0].Kind, "file")
	}
	if ext.Parts[0].Content != "base64-encoded-content" {
		t.Errorf("Parts[0].Content = %q, want %q", ext.Parts[0].Content, "base64-encoded-content")
	}
}

func TestA2AParser_AnyMethod(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"tasks/get","id":"req-5","params":{"taskId":"task-123"}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if ext.Method != "tasks/get" {
		t.Errorf("Method = %q, want %q", ext.Method, "tasks/get")
	}
	if ext.RPCID != "req-5" {
		t.Errorf("RPCID = %v, want %q", ext.RPCID, "req-5")
	}
	if len(ext.Parts) != 0 {
		t.Errorf("Parts should be empty when no params.message, got %d", len(ext.Parts))
	}
}

func TestA2AParser_FutureMethodWithMessage(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/resume","id":"req-7","params":{"message":{"role":"user","parts":[{"kind":"text","text":"Continue"}],"messageId":"msg-007"},"sessionId":"sess-future"}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if ext.Method != "message/resume" {
		t.Errorf("Method = %q, want %q", ext.Method, "message/resume")
	}
	if ext.SessionID != "sess-future" {
		t.Errorf("SessionID = %q, want %q", ext.SessionID, "sess-future")
	}
	if ext.Role != "user" {
		t.Errorf("Role = %q, want %q", ext.Role, "user")
	}
	if len(ext.Parts) != 1 {
		t.Fatalf("Parts len = %d, want 1", len(ext.Parts))
	}
	if ext.Parts[0].Content != "Continue" {
		t.Errorf("Parts[0].Content = %q, want %q", ext.Parts[0].Content, "Continue")
	}
}

func TestA2AParser_DataPart(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/send","id":"req-8","params":{"message":{"role":"user","parts":[{"kind":"data","data":{"key":"value","num":42}}],"messageId":"msg-008"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if len(ext.Parts) != 1 {
		t.Fatalf("Parts len = %d, want 1", len(ext.Parts))
	}
	if ext.Parts[0].Kind != "data" {
		t.Errorf("Parts[0].Kind = %q, want %q", ext.Parts[0].Kind, "data")
	}
	if ext.Parts[0].Content == "" {
		t.Error("Parts[0].Content should not be empty for data part")
	}
}

func TestA2AParser_FilePartURI(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/send","id":"req-9","params":{"message":{"role":"user","parts":[{"kind":"file","uri":"https://example.com/doc.pdf"}],"messageId":"msg-009"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if len(ext.Parts) != 1 {
		t.Fatalf("Parts len = %d, want 1", len(ext.Parts))
	}
	if ext.Parts[0].Kind != "file" {
		t.Errorf("Parts[0].Kind = %q, want %q", ext.Parts[0].Kind, "file")
	}
	if ext.Parts[0].Content != "https://example.com/doc.pdf" {
		t.Errorf("Parts[0].Content = %q, want %q", ext.Parts[0].Content, "https://example.com/doc.pdf")
	}
}

func TestA2AParser_NilBody(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{Body: nil}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.A2A != nil {
		t.Error("Extensions.A2A should be nil when body is nil")
	}
}

func TestA2AParser_EmptyBody(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{Body: []byte{}}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.A2A != nil {
		t.Error("Extensions.A2A should be nil when body is empty")
	}
}

func TestA2AParser_InvalidJSON(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{Body: []byte("not json")}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pctx.Extensions.A2A != nil {
		t.Error("Extensions.A2A should be nil for invalid JSON")
	}
}

func TestA2AParser_MissingMessage(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/send","id":"req-6","params":{"sessionId":"sess-1"}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if ext.Method != "message/send" {
		t.Errorf("Method = %q, want %q", ext.Method, "message/send")
	}
	if ext.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", ext.SessionID, "sess-1")
	}
	if ext.Role != "" {
		t.Errorf("Role = %q, want empty", ext.Role)
	}
	if len(ext.Parts) != 0 {
		t.Errorf("Parts len = %d, want 0", len(ext.Parts))
	}
}

func TestA2AParser_MalformedParts(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/send","id":"req-10","params":{"message":{"role":"user","parts":[{"kind":0,"text":"bad"},{"text":"missing-kind"},{"kind":"text","text":"valid"}],"messageId":"msg-010"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if len(ext.Parts) != 1 {
		t.Fatalf("Parts len = %d, want 1 (only the valid part)", len(ext.Parts))
	}
	if ext.Parts[0].Kind != "text" {
		t.Errorf("Parts[0].Kind = %q, want %q", ext.Parts[0].Kind, "text")
	}
	if ext.Parts[0].Content != "valid" {
		t.Errorf("Parts[0].Content = %q, want %q", ext.Parts[0].Content, "valid")
	}
}

func TestA2AParser_MalformedContentValues(t *testing.T) {
	p := NewA2AParser()
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","method":"message/send","id":"req-11","params":{"message":{"role":"user","parts":[{"kind":"text","text":false},{"kind":"file","data":0,"uri":{}},{"kind":"data","data":null}],"messageId":"msg-011"}}}`),
	}

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.A2A
	if ext == nil {
		t.Fatal("Extensions.A2A is nil")
	}
	if len(ext.Parts) != 3 {
		t.Fatalf("Parts len = %d, want 3", len(ext.Parts))
	}
	if ext.Parts[0].Content != "" {
		t.Errorf("text part with bool value: Content = %q, want empty", ext.Parts[0].Content)
	}
	if ext.Parts[1].Content != "" {
		t.Errorf("file part with numeric data and object uri: Content = %q, want empty", ext.Parts[1].Content)
	}
	if ext.Parts[2].Content != "" {
		t.Errorf("data part with null data: Content = %q, want empty", ext.Parts[2].Content)
	}
}

func TestA2AParser_OnResponse(t *testing.T) {
	p := NewA2AParser()
	action := p.OnResponse(context.Background(), &pipeline.Context{})
	if action.Type != pipeline.Continue {
		t.Errorf("OnResponse should return Continue, got %v", action.Type)
	}
}
