package extproc

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// TestBytes_SplitResponseBodySumsEveryMessage is this listener's reason for
// counting response bytes in an accumulator instead of measuring the buffer at the
// record site (#1309).
//
// Envoy may deliver one response body in several ResponseBody messages, and on the
// SSE arm handleResponseBody REPLACES pctx.ResponseBody each time rather than
// appending — it carries the SSE tail forward, not the whole response. So by the
// time the row is recorded the buffer holds the LAST message only, and
// len(pctx.ResponseBody) would report a streamed inference response as the size of
// its final chunk. On this two-message fixture that is a figure roughly half the
// truth, reported with no indication anything was missing — on exactly the traffic
// the BYTES column was asked for.
//
// Reuses splitStreamRequests from the split-body cost test rather than scripting a
// second two-message stream: it is already the shape that gets this wrong, and
// pinning both the charge and the byte count against one fixture keeps them from
// drifting into disagreement about what that response was.
//
// The parity suite cannot stand in for this. Swapping the accumulator back for
// len(pctx.ResponseBody) leaves listener/parity's bytes fixtures GREEN — its driver
// delivers each body in a single message, where the buffer and the sum are the same
// number. Only a multi-message body tells them apart, and only this listener has
// one.
func TestBytes_SplitResponseBodySumsEveryMessage(t *testing.T) {
	srv, store := newStreamedServer(t)
	reqs := splitStreamRequests()

	var (
		wantUp   int64
		wantDown int64
		lastDown int64
	)
	for _, r := range reqs {
		switch m := r.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestBody:
			wantUp += int64(len(m.RequestBody.Body))
		case *extprocv3.ProcessingRequest_ResponseBody:
			lastDown = int64(len(m.ResponseBody.Body))
			wantDown += lastDown
		}
	}
	if lastDown == wantDown {
		t.Fatalf("fixture delivers the response in one message; it cannot distinguish the sum from the trailing chunk")
	}

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)
	if stream.recvIdx != len(reqs) {
		t.Fatalf("consumed %d of %d messages; the listener bailed", stream.recvIdx, len(reqs))
	}

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a split streamed body")
	}
	switch ev.BytesDown {
	case wantDown:
	case lastDown:
		t.Errorf("BytesDown = %d — the TRAILING MESSAGE only, which is all pctx.ResponseBody "+
			"holds once the SSE arm has replaced it. The whole body was %d bytes",
			ev.BytesDown, wantDown)
	default:
		t.Errorf("BytesDown = %d, want %d (the sum of every ResponseBody message)", ev.BytesDown, wantDown)
	}

	v := store.View(session.DefaultSessionID)
	var reqRow *pipeline.SessionEvent
	for i := range v.Events {
		if v.Events[i].Phase == pipeline.SessionRequest && v.Events[i].Direction == pipeline.Outbound {
			reqRow = &v.Events[i]
			break
		}
	}
	if reqRow == nil {
		t.Fatalf("no outbound request row recorded; events = %+v", v.Events)
	}
	if reqRow.BytesUp != wantUp || reqRow.BytesDown != 0 {
		t.Errorf("request row = ↑%d ↓%d, want ↑%d ↓0", reqRow.BytesUp, reqRow.BytesDown, wantUp)
	}
}

// TestBytes_HeaderOnlyResponseReportsZero pins the honest zero on the listener that
// has the least room to do better: Envoy sends no ResponseBody message for a
// response that ends on its headers, so there is nothing for the accumulator to add
// and nothing to infer from.
//
// Zero means the listener counted nothing — the event contract has no third value
// for "unknown", and agentop renders it blank rather than as 0B. A content-length
// header would be a plausible-looking substitute and is the wrong one: it is absent
// or -1 on everything streamed, which is most of what passes through here.
func TestBytes_HeaderOnlyResponseReportsZero(t *testing.T) {
	srv, store := newStreamedServer(t)
	reqs := []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "GET",
					":path", "/v1/models",
					":authority", "litellm.local",
				),
				EndOfStream: true,
			},
		}},
		{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					":status", "200",
					// Load-bearing, not scenery: this listener records a row only
					// when the pipeline left something on pctx, and on a response
					// with no body the gateway's cost header is the only thing the
					// inference-parser can claim. Without it there is no row to
					// assert the zero on — which is a different fact from the zero.
					"x-litellm-response-cost", "0.002",
				),
				// The one fact that makes this a header-only response.
				EndOfStream: true,
			},
		}},
	}

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)

	ev := responseEvent(t, store)
	if ev == nil {
		t.Fatal("no outbound response row recorded for a header-only response")
	}
	if ev.BytesUp != 0 || ev.BytesDown != 0 {
		t.Errorf("response row = ↑%d ↓%d, want ↑0 ↓0 — no body message arrived, so there is nothing to count", ev.BytesUp, ev.BytesDown)
	}
}
