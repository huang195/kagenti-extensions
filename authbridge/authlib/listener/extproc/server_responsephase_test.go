package extproc

import (
	"context"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/inferenceparser"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// responseCounter counts response-phase dispatches. Deliberately NOT a StreamingResponder: it
// implements no OnResponseFrame, so RunResponse is the only hook that reaches it — which is
// exactly the population the teardown flush can double.
type responseCounter struct {
	mu    sync.Mutex
	calls int
}

func (p *responseCounter) Name() string { return "response-counter" }
func (p *responseCounter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *responseCounter) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseCounter) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	// An Observe per pass, so a doubled phase is visible in the recorded row too and not only
	// in this counter.
	pctx.Observe("response phase")
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseCounter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestExtProc_TornStreamRunsTheResponsePhaseOnce is suggestion 4 of review round 6.
//
// THE GUARD WAS SET IN ONE OF THE TWO PLACES THAT RUN THE PHASE. handleResponseHeaders marks
// it; handleResponseBody ran RunResponse and marked nothing, so on the route where body
// messages arrive but Envoy never says end-of-stream, the teardown flush asked "did the phase
// run" and got the wrong answer. Every non-streaming plugin then ran its response phase a
// SECOND time — opa, cpex, lineage, sparc in the shipped pipelines — and the extra Invocation
// rows landed in the recorded snapshot.
//
// The comment at the flush asserted the opposite ("the phase never ran... there is nothing to
// double"), which is the reason to pin it with a test rather than to re-read the comment.
//
// MONEY WAS NEVER AT RISK HERE, and saying so is part of the claim: the cost owner is a
// StreamingResponder, and RunResponse skips those, so the settle latch was never the thing
// standing between this and a double charge. What was at risk is the audit trail — a request
// whose policy plugins are recorded as having decided twice.
func TestExtProc_TornStreamRunsTheResponsePhaseOnce(t *testing.T) {
	counter := &responseCounter{}
	parser := inferenceparser.NewInferenceParser()
	parser.SetPricingResolver(streamedRates(t))
	// The parser is present because it is what makes the pipeline need a body at all: without
	// it handleResponseHeaders would not defer the phase, and this route would not exist.
	p, err := pipeline.New([]pipeline.Plugin{parser, counter})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	empty, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("pipeline.New(nil): %v", err)
	}
	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		InboundPipeline:  pipeline.NewHolder(empty),
		Sessions:         store,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := tornStreamRequests()
	stream := &cancelOnEndStream{mockStream: &mockStream{ctx: ctx, requests: reqs}, cancel: cancel}

	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed before the teardown", stream.recvIdx, len(reqs))
	}
	if ctx.Err() == nil {
		t.Fatal("the stream context is not cancelled, so this test is not exercising teardown")
	}

	if got := counter.count(); got != 1 {
		t.Errorf("response phases = %d, want 1: a body message ran the phase without marking it, so the teardown flush ran every non-streaming plugin again", got)
	}
}
