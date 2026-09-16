package extproc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
)

// cancelOnEndStream cancels the stream's context the moment the stream runs out of
// messages, which is what Envoy tearing an ext_proc stream down looks like from here: a
// client hangup, a filter timeout, or the proxy shutting down mid-response.
//
// The distinction from the fixtures next to this file is the whole point. There, the last
// ResponseBody carries EndOfStream and the response settles on that message. Here the
// response never gets a terminal message, so the deferred flush at the top of Process is the
// only thing left that can settle it — and it runs AFTER the context it was handed is done.
type cancelOnEndStream struct {
	*mockStream
	cancel context.CancelFunc
}

func (m *cancelOnEndStream) Recv() (*extprocv3.ProcessingRequest, error) {
	req, err := m.mockStream.Recv()
	if err != nil {
		// No more messages: the stream is gone, so its context is done. Cancelling here
		// rather than before Process means every earlier phase ran normally, and only the
		// flush sees a cancelled context.
		m.cancel()
	}
	return req, err
}

// tornStreamRequests is the split-body fixture with the terminal message removed: prompt
// counters arrived, output and stop reason never did, and Envoy never said end-of-stream.
func tornStreamRequests() []*extprocv3.ProcessingRequest {
	reqs := splitStreamRequests()
	// Drop the message_delta / message_stop message, and unset end-of-stream on what is
	// left, so nothing in the loop finalizes the response.
	reqs = reqs[:len(reqs)-1]
	if body := reqs[len(reqs)-1].GetResponseBody(); body != nil {
		body.EndOfStream = false
	}
	return reqs
}

// THE FLUSH MUST SURVIVE THE STREAM IT IS FLUSHING.
//
// pipeline.RunResponseFrame refuses a cancelled context before it calls any plugin — it
// returns Deny("pipeline.cancelled") — so a terminal frame dispatched on the stream's own
// context is a no-op in exactly the case the deferred flush exists for. The parsers keep
// their accumulated prompt counters, nothing settles, and the request's cost reaches no
// aggregate, no ledger and no budget.
//
// The fix is context.WithoutCancel, which is what forwardproxy's finish path and the reverse
// proxy's finalCtx already do, and what RunFinish does internally — this was the one
// detached-work site that had not been detached.
//
// The figure asserted is the FLOOR, not the whole cost: the output tally never arrived, so a
// floor is the honest answer and Incomplete says so. What is under test is that a figure
// exists at all.
func TestExtProc_ATornDownStreamStillSettlesItsCost(t *testing.T) {
	srv, store := newStreamedServer(t)

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

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a torn-down stream")
	}
	rec, ok := costevent.Record(ev)
	if !ok {
		t.Fatalf("no cost record on the response row of a torn-down stream (Plugins keys = %v): "+
			"the terminal frame was dispatched on the cancelled stream context, so the parsers "+
			"never settled and this request's spend reaches no aggregate, ledger or budget", pluginKeys(ev))
	}
	if rec.CostUSD != streamedFloorUSD {
		t.Errorf("CostUSD = %v, want the prompt-only floor %v", rec.CostUSD, streamedFloorUSD)
	}
	if !rec.Incomplete {
		t.Error("Incomplete = false on a stream that died before its output tally; the figure is " +
			"a floor and has to say so")
	}
}
