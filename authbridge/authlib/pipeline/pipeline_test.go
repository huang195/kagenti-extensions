package pipeline

import (
	"context"
	"testing"
)

// stubPlugin is a minimal Plugin implementation for testing.
type stubPlugin struct {
	name   string
	caps   PluginCapabilities
	onReq  func(ctx context.Context, pctx *Context) Action
	onResp func(ctx context.Context, pctx *Context) Action
}

func (s *stubPlugin) Name() string                     { return s.name }
func (s *stubPlugin) Capabilities() PluginCapabilities { return s.caps }
func (s *stubPlugin) OnRequest(ctx context.Context, pctx *Context) Action {
	if s.onReq != nil {
		return s.onReq(ctx, pctx)
	}
	return Action{Type: Continue}
}
func (s *stubPlugin) OnResponse(ctx context.Context, pctx *Context) Action {
	if s.onResp != nil {
		return s.onResp(ctx, pctx)
	}
	return Action{Type: Continue}
}

func TestPipelineRun_EmptyPipeline(t *testing.T) {
	p, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil) returned error: %v", err)
	}
	pctx := &Context{}
	action := p.Run(context.Background(), pctx)
	if action.Type != Continue {
		t.Errorf("empty pipeline returned %v, want Continue", action.Type)
	}
}

func TestPipelineRun_Sequential(t *testing.T) {
	var order []string
	p1 := &stubPlugin{
		name: "first",
		onReq: func(_ context.Context, pctx *Context) Action {
			order = append(order, "first")
			pctx.Extensions.Custom = map[string]any{"key": "value"}
			return Action{Type: Continue}
		},
	}
	p2 := &stubPlugin{
		name: "second",
		caps: PluginCapabilities{Reads: []string{"custom"}},
		onReq: func(_ context.Context, pctx *Context) Action {
			order = append(order, "second")
			if pctx.Extensions.Custom["key"] != "value" {
				t.Error("second plugin did not see mutation from first")
			}
			return Action{Type: Continue}
		},
	}
	p1.caps = PluginCapabilities{Writes: []string{"custom"}}

	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.Run(context.Background(), pctx)
	if action.Type != Continue {
		t.Errorf("got %v, want Continue", action.Type)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("execution order = %v, want [first second]", order)
	}
}

func TestPipelineRun_Reject(t *testing.T) {
	called := false
	p1 := &stubPlugin{
		name: "rejecter",
		onReq: func(_ context.Context, _ *Context) Action {
			return Action{Type: Reject, Status: 403, Reason: "forbidden"}
		},
	}
	p2 := &stubPlugin{
		name: "never-called",
		onReq: func(_ context.Context, _ *Context) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.Run(context.Background(), pctx)
	if action.Type != Reject {
		t.Errorf("got %v, want Reject", action.Type)
	}
	if action.Status != 403 {
		t.Errorf("status = %d, want 403", action.Status)
	}
	if action.Reason != "forbidden" {
		t.Errorf("reason = %q, want %q", action.Reason, "forbidden")
	}
	if called {
		t.Error("second plugin was called after first rejected")
	}
}

func TestPipelineRunResponse_ReverseOrder(t *testing.T) {
	var order []string
	p1 := &stubPlugin{
		name: "first",
		onResp: func(_ context.Context, _ *Context) Action {
			order = append(order, "first")
			return Action{Type: Continue}
		},
	}
	p2 := &stubPlugin{
		name: "second",
		onResp: func(_ context.Context, _ *Context) Action {
			order = append(order, "second")
			return Action{Type: Continue}
		},
	}
	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.RunResponse(context.Background(), pctx)
	if action.Type != Continue {
		t.Errorf("got %v, want Continue", action.Type)
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Errorf("response order = %v, want [second first]", order)
	}
}

func TestPipelineRunResponse_Reject(t *testing.T) {
	called := false
	p1 := &stubPlugin{
		name: "first",
		onResp: func(_ context.Context, _ *Context) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	p2 := &stubPlugin{
		name: "rejecter",
		onResp: func(_ context.Context, _ *Context) Action {
			return Action{Type: Reject, Status: 500, Reason: "response blocked"}
		},
	}
	pipe, err := New([]Plugin{p1, p2})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.RunResponse(context.Background(), pctx)
	if action.Type != Reject {
		t.Errorf("got %v, want Reject", action.Type)
	}
	if called {
		t.Error("first plugin OnResponse was called after second rejected (reverse order)")
	}
}

func TestNew_ValidCapabilities(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "writer",
			caps: PluginCapabilities{Writes: []string{"mcp"}},
		},
		&stubPlugin{
			name: "reader",
			caps: PluginCapabilities{Reads: []string{"mcp"}},
		},
	}
	_, err := New(plugins)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestNew_InvalidCapabilities_ReadBeforeWrite(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "reader",
			caps: PluginCapabilities{Reads: []string{"mcp"}},
		},
		&stubPlugin{
			name: "writer",
			caps: PluginCapabilities{Writes: []string{"mcp"}},
		},
	}
	_, err := New(plugins)
	if err == nil {
		t.Fatal("expected error for read-before-write, got nil")
	}
}

func TestNew_InvalidCapabilities_UnknownSlot(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "bad-reader",
			caps: PluginCapabilities{Reads: []string{"nonexistent"}},
		},
	}
	_, err := New(plugins)
	if err == nil {
		t.Fatal("expected error for unknown slot, got nil")
	}
}

func TestNew_MultipleWriters(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "writer-1",
			caps: PluginCapabilities{Writes: []string{"security"}},
		},
		&stubPlugin{
			name: "writer-2",
			caps: PluginCapabilities{Writes: []string{"security"}},
		},
		&stubPlugin{
			name: "reader",
			caps: PluginCapabilities{Reads: []string{"security"}},
		},
	}
	_, err := New(plugins)
	if err != nil {
		t.Errorf("multiple writers should be valid, got: %v", err)
	}
}

func TestNew_NoCapabilities(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{name: "simple"},
	}
	_, err := New(plugins)
	if err != nil {
		t.Errorf("plugin with no capabilities should be valid, got: %v", err)
	}
}

func TestNew_CustomSlot(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "custom-writer",
			caps: PluginCapabilities{Writes: []string{"custom"}},
		},
		&stubPlugin{
			name: "custom-reader",
			caps: PluginCapabilities{Reads: []string{"custom"}},
		},
	}
	_, err := New(plugins)
	if err != nil {
		t.Errorf("custom slot should be valid, got: %v", err)
	}
}

func TestNew_WithSlots(t *testing.T) {
	plugins := []Plugin{
		&stubPlugin{
			name: "cpex-bridge",
			caps: PluginCapabilities{Writes: []string{"cpex.completion"}},
		},
		&stubPlugin{
			name: "consumer",
			caps: PluginCapabilities{Reads: []string{"cpex.completion"}},
		},
	}
	// Without WithSlots, this should fail
	_, err := New(plugins)
	if err == nil {
		t.Fatal("expected error for unregistered slot without WithSlots")
	}

	// With WithSlots, this should succeed
	_, err = New(plugins, WithSlots("cpex.completion"))
	if err != nil {
		t.Errorf("expected no error with WithSlots, got: %v", err)
	}
}

func TestDelegationExtension_AppendHop(t *testing.T) {
	d := &DelegationExtension{}

	d.AppendHop(DelegationHop{SubjectID: "alice", Scopes: []string{"read", "write"}})
	if d.Depth() != 1 {
		t.Errorf("Depth() = %d, want 1", d.Depth())
	}
	if d.Origin != "alice" {
		t.Errorf("origin = %q, want %q", d.Origin, "alice")
	}
	if d.Actor != "alice" {
		t.Errorf("actor = %q, want %q", d.Actor, "alice")
	}

	d.AppendHop(DelegationHop{SubjectID: "bob", Scopes: []string{"read"}})
	if d.Depth() != 2 {
		t.Errorf("Depth() = %d, want 2", d.Depth())
	}
	if d.Origin != "alice" {
		t.Errorf("origin should stay %q, got %q", "alice", d.Origin)
	}
	if d.Actor != "bob" {
		t.Errorf("actor = %q, want %q", d.Actor, "bob")
	}
	chain := d.Chain()
	if len(chain) != 2 {
		t.Errorf("Chain() length = %d, want 2", len(chain))
	}
}

func TestDelegationExtension_ChainIsCopy(t *testing.T) {
	d := &DelegationExtension{}
	d.AppendHop(DelegationHop{SubjectID: "alice"})

	chain := d.Chain()
	chain[0].SubjectID = "tampered"

	original := d.Chain()
	if original[0].SubjectID != "alice" {
		t.Errorf("Chain() returned reference to backing slice, mutation leaked")
	}
}

func TestPipelineRun_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	p1 := &stubPlugin{
		name: "should-not-run",
		onReq: func(_ context.Context, _ *Context) Action {
			called = true
			return Action{Type: Continue}
		},
	}
	pipe, err := New([]Plugin{p1})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	pctx := &Context{}
	action := pipe.Run(ctx, pctx)
	if action.Type != Reject {
		t.Errorf("got %v, want Reject for cancelled context", action.Type)
	}
	if action.Status != 499 {
		t.Errorf("status = %d, want 499", action.Status)
	}
	if called {
		t.Error("plugin was called despite cancelled context")
	}
}

func TestPipeline_Plugins_ReturnsCopy(t *testing.T) {
	a := &stubPlugin{name: "a", caps: PluginCapabilities{Writes: []string{"custom"}}}
	b := &stubPlugin{name: "b", caps: PluginCapabilities{Reads: []string{"custom"}}}
	p, err := New([]Plugin{a, b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := p.Plugins()
	if len(got) != 2 {
		t.Fatalf("Plugins() len = %d, want 2", len(got))
	}
	if got[0].Name() != "a" || got[1].Name() != "b" {
		t.Errorf("Plugins() names = [%s %s], want [a b]", got[0].Name(), got[1].Name())
	}

	// Mutating the returned slice must not corrupt the pipeline's backing
	// storage — callers get a decorative accessor, not a handle.
	got[0] = nil
	again := p.Plugins()
	if again[0] == nil || again[0].Name() != "a" {
		t.Errorf("Plugins() returned aliased slice; backing data was mutated")
	}
}
