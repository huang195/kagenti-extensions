package parity

// The gRPC receive limit: the ceiling in front of extproc's body caps that no
// fixture in this package can reach.
//
// Not a parity or a divergence fixture, and it sits here rather than in
// listener/extproc because it is the third entry in divergence_test.go's table
// of limits and is only legible beside the other two. The proxy drivers run
// real HTTP servers, so their caps are exercised end to end; the extproc driver
// calls Server.Process directly over a mockStream (see drivers_test.go) and
// therefore has no gRPC transport at all. Everything this package knows about
// extproc's caps, it knows with that limit absent.
//
// WHAT THE LIMIT IS. cmd/cortex-envoy's newExtProcServer builds its ext_proc
// server with a bare grpc.NewServer() and passes no options, so grpc-go's
// default MaxRecvMsgSize — 4 MiB — applies to every ProcessingRequest Envoy
// sends it. newExtprocGRPCServer below reconstructs that call site: a bare
// grpc.NewServer(), the real extproc.Server registered on it, a real stream
// over bufconn. Passing grpc.MaxRecvMsgSize here in either direction would make
// this file measure itself.
//
// Reconstructs, not reaches — and the difference is load-bearing. core cannot
// import package main, so nothing here would notice newExtProcServer gaining a
// grpc.MaxRecvMsgSize option: every assertion below would stay green while the
// real ceiling moved. What this file pins is grpc-go's DEFAULT and what the
// listener does under it. The call site is pinned where it lives, by
// cmd/cortex-envoy's TestStartGRPCExtProcKeepsDefaultRecvLimit — and that
// function exists so a test can reach the construction without a socket, with
// TestExtProcServerIsTheOnlyGRPCServerConstruction keeping it the binary's only
// one. Both halves are needed, and neither substitutes for the other.
//
// WHY IT MATTERS, AND IN WHICH ENVOY CONFIG. This ceiling sits BEHIND Envoy's
// buffer limit. On the shipped config nothing reaches it: Envoy buffers at most
// 1 MiB, so the ProcessingRequest cannot approach 4 MiB, and #864's 5,250,133-byte
// request gets Envoy's own 413 — the left column of divergence_test.go's cap
// table, unchanged. The band these fixtures are about is 1-32 MiB, so the
// operator whose traffic lives there raises per_connection_buffer_limit_bytes to
// get past that, and THAT is where this limit decides the outcome: the wall
// moves from 1 MiB to 4 MiB rather than away, so a 5 MiB body still never meets
// the listener's own check and still never produces the 413
// TestDivergence_OutboundRequestBodyCap pins. What the operator gets instead is
// an empty 500 from Envoy; ResourceExhausted is visible only to ext_proc, in the
// sidecar's own logs.
//
// AND THE TWO DIRECTIONS FAIL DIFFERENTLY, which is the part worth having in a
// test rather than a comment — it was measured here, not assumed:
//
//   - An over-limit REQUEST vanishes. The stream dies before the listener builds
//     a pipeline.Context, so the teardown flush has nothing to record and no
//     session row exists in either direction. Same observable shape as the 413
//     one message earlier, and for the same reason (both leave pctx nil), which
//     is why the request cases below assert zero rows for both. What differs is
//     what the caller is told: the 413 is a real status with the listener's own
//     body, whereas here Envoy has no answer to give and sends an empty 500.
//   - An over-limit RESPONSE books a 200 — while the client gets that same empty
//     500. By then the request phase has run and ResponseHeaders have been seen,
//     so Process's deferred flush fires its (sawResponseHeaders ||
//     sawResponseBody) gate and records an ordinary response row: status 200, no
//     error, and the response body reported as ZERO bytes. So this is not a row
//     that merely lost a body. It is the same contradiction the last two
//     divergence fixtures pin for Envoy's own refusal — the event claims a clean
//     200 the caller never saw — reached one layer further in, and on the
//     outbound leg it is also a request whose spend silently becomes nothing.
//
// Not covered here, deliberately: a 2 MiB response, which is over the
// listener's cap and under gRPC's. That is extproc's appendBoundedBody
// truncation and TestDivergence_OutboundResponseBodyCap already pins it against
// the proxies, which is where it is a divergence rather than a limit.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/extproc"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/session"
)

// grpcMaxRecvMsgSize is grpc-go's DEFAULT server-side receive cap — the one
// cmd/cortex-envoy takes by not overriding it, which is a separate fact pinned
// separately (see the file comment).
//
// Written as a number AND asserted below against the figure the transport
// reports, rather than only described in prose. That coupling is deliberate: a
// grpc-go release changing this default would otherwise move the real ceiling
// while every comment in this package went on naming 4 MiB — which is precisely
// the drift between comment and code that this PR's review caught twice. It
// covers the default moving under us; it cannot cover the call site opting out
// of the default, because the assertions here build their own server.
const grpcMaxRecvMsgSize = 4 << 20

// grpcLimitRow is one expected session row, in order.
type grpcLimitRow struct {
	phase      pipeline.SessionPhase
	statusCode int
	// pluginEvents is an EXACT set, as in divergence_test.go: every key listed
	// must be present with that JSON and no key outside the map may appear. A
	// digest of zero length is a different claim from an absent key — the first
	// says the response phase ran on an empty buffer, the second that it never
	// ran — and only an exact set can tell them apart.
	pluginEvents map[string]string
}

// TestExtprocGRPCReceiveLimit walks a body across both ceilings in both
// directions and pins which limit answers, what the caller sees, and what the
// session store is left holding.
func TestExtprocGRPCReceiveLimit(t *testing.T) {
	// Sizes straddle the two limits: maxBufferedBody (1 MiB, extproc's own cap)
	// and grpcMaxRecvMsgSize (4 MiB). The 5 MiB cases are #864's measured
	// 5,250,133-byte request rounded DOWN to a whole MiB — 5<<20 is 5,242,880,
	// which is not a power of two and is not meant to be. Rounding down keeps
	// the case on the same side of the 4 MiB ceiling as the real request while
	// making the size readable.
	reqDigest := spyPluginA + bodyReqDigestStrippedSuffix
	respDigest := spyPluginA + bodyRespDigestStrippedSuffix
	oneMiB := divergenceBody(maxBufferedBody)

	cases := []struct {
		name string
		size int
		// phase says which body message carries the payload: the request's, or
		// the upstream response's. Both are run because they fail differently —
		// see the file comment.
		phase string
		// wantGRPCError: the transport refuses the message before the listener
		// sees a byte of it.
		wantGRPCError bool
		// wantImmediate413: the listener's own cap answers, with its own body.
		wantImmediate413 bool
		// wantRows is the full ordered contents of the session, after the stream
		// has been torn down.
		wantRows []grpcLimitRow
	}{{
		name:  "request/under-both-ceilings",
		size:  maxBufferedBody,
		phase: "request",
		// The control for the case below it: at exactly the listener's cap the
		// body is carried whole, so the 413 next door is the cap and not the
		// transport.
		wantRows: []grpcLimitRow{{
			phase:        pipeline.SessionRequest,
			pluginEvents: map[string]string{reqDigest: digestJSON(oneMiB)},
		}},
	}, {
		name:             "request/over-listener-under-grpc",
		size:             2 << 20,
		phase:            "request",
		wantImmediate413: true,
		// No row at all, which is the finding TestDivergence_OutboundRequestBodyCap
		// states as extproc's side of the request-cap divergence: the refusal
		// happens before a pipeline.Context exists, so nothing reaches a
		// recording site and the request is invisible in /v1/sessions,
		// /v1/usage and agentop alike.
		wantRows: nil,
	}, {
		name:          "request/over-both-ceilings",
		size:          5 << 20,
		phase:         "request",
		wantGRPCError: true,
		// Same empty store as the 413 above, by the same mechanism (pctx is
		// still nil), but reached without the listener running at all. The two
		// are indistinguishable downstream and distinguishable only by what the
		// caller gets back — which is why both halves are asserted.
		wantRows: nil,
	}, {
		name:  "response/under-both-ceilings",
		size:  maxBufferedBody,
		phase: "response",
		// The control for the case below: the whole 1 MiB reaches the plugin and
		// the 200 is honest.
		wantRows: []grpcLimitRow{{
			phase:        pipeline.SessionRequest,
			pluginEvents: map[string]string{reqDigest: digestJSON(nil)},
		}, {
			phase:      pipeline.SessionResponse,
			statusCode: 200,
			pluginEvents: map[string]string{
				reqDigest:  digestJSON(nil),
				respDigest: digestJSON(oneMiB),
			},
		}},
	}, {
		name:          "response/over-both-ceilings",
		size:          5 << 20,
		phase:         "response",
		wantGRPCError: true,
		// THE ROW THAT LIES, and the reason this file asserts the store and not
		// just the status. Byte-identical to the honest 200 above except for the
		// response digest's length, which is where the whole response went.
		wantRows: []grpcLimitRow{{
			phase:        pipeline.SessionRequest,
			pluginEvents: map[string]string{reqDigest: digestJSON(nil)},
		}, {
			phase:      pipeline.SessionResponse,
			statusCode: 200,
			pluginEvents: map[string]string{
				reqDigest:  digestJSON(nil),
				respDigest: digestJSON(nil),
			},
		}},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, store, srv := newExtprocGRPCServer(t)
			stream, err := client.Process(context.Background())
			if err != nil {
				t.Fatalf("opening the Process stream: %v", err)
			}

			// content-length is what makes the listener ask Envoy for the request
			// body at all (requestHasBody). The response-phase cases declare a
			// body-less request so the request phase completes at headers and the
			// oversized message arrives later, which is the sequence that makes
			// the teardown flush reachable.
			reqLen := tc.size
			if tc.phase == "response" {
				reqLen = 0
			}
			if err := stream.Send(headersRequest(reqLen)); err != nil {
				t.Fatalf("sending RequestHeaders: %v", err)
			}
			if _, err := stream.Recv(); err != nil {
				t.Fatalf("reading the reply to RequestHeaders: %v", err)
			}

			var oversized *extprocv3.ProcessingRequest
			switch tc.phase {
			case "request":
				oversized = &extprocv3.ProcessingRequest{
					Request: &extprocv3.ProcessingRequest_RequestBody{
						RequestBody: &extprocv3.HttpBody{Body: divergenceBody(tc.size), EndOfStream: true},
					},
				}
			case "response":
				if err := stream.Send(responseHeadersRequest(tc.size)); err != nil {
					t.Fatalf("sending ResponseHeaders: %v", err)
				}
				if _, err := stream.Recv(); err != nil {
					t.Fatalf("reading the reply to ResponseHeaders: %v", err)
				}
				oversized = &extprocv3.ProcessingRequest{
					Request: &extprocv3.ProcessingRequest_ResponseBody{
						ResponseBody: &extprocv3.HttpBody{Body: divergenceBody(tc.size), EndOfStream: true},
					},
				}
			default:
				t.Fatalf("unknown phase %q", tc.phase)
			}

			// The Send is not asserted on: grpc-go does not police an outgoing
			// message against the PEER's limit, so an oversized one goes out and
			// the refusal comes back as the stream's status. A Send that did fail
			// would surface as the Recv below.
			_ = stream.Send(oversized)

			resp, recvErr := stream.Recv()
			switch {
			case tc.wantGRPCError:
				if recvErr == nil {
					t.Fatalf("a %d-byte %s body was accepted; want the transport to refuse it over grpc-go's %d-byte default (got reply %T)",
						tc.size, tc.phase, grpcMaxRecvMsgSize, resp.GetResponse())
				}
				if got := status.Code(recvErr); got != codes.ResourceExhausted {
					// Deliberately not the codes.Unknown Process wraps its own
					// Recv error in: grpc-go tears the stream down at the
					// transport, so the handler's error never reaches the peer.
					t.Errorf("status code = %v, want %v (full error: %v)", got, codes.ResourceExhausted, recvErr)
				}
				// The FIGURE, not the wording. grpc-go says "received message
				// larger than max (N vs. 4194304)", and this is what fails loudly
				// if that 4194304 ever moves.
				if want := fmt.Sprintf("%d", grpcMaxRecvMsgSize); !strings.Contains(recvErr.Error(), want) {
					t.Errorf("error does not name the %s-byte limit every comment here calls 4 MiB; grpc-go's default may have moved: %v", want, recvErr)
				}
			case tc.wantImmediate413:
				if recvErr != nil {
					t.Fatalf("a %d-byte %s body was refused by the transport; want it under grpc-go's %d-byte default so the listener's own cap answers: %v",
						tc.size, tc.phase, grpcMaxRecvMsgSize, recvErr)
				}
				imm := resp.GetImmediateResponse()
				if imm == nil {
					t.Fatalf("a %d-byte %s body got reply %T; want the listener's ImmediateResponse 413 over its %d-byte cap",
						tc.size, tc.phase, resp.GetResponse(), maxBufferedBody)
				}
				if code := imm.GetStatus().GetCode(); code != 413 {
					t.Errorf("ImmediateResponse status = %d, want 413", code)
				}
				// The body is pinned because it is the listener's OWN string: an
				// Envoy-generated 413 would not carry it, so this is what tells
				// the two refusal mechanisms in the cap table apart.
				if body := string(imm.GetBody()); body != `{"error":"request body too large"}` {
					t.Errorf("ImmediateResponse body = %q, want the listener's own refusal payload", body)
				}
			default:
				if recvErr != nil {
					t.Fatalf("a %d-byte %s body was refused; want it carried, being at neither ceiling: %v", tc.size, tc.phase, recvErr)
				}
				if imm := resp.GetImmediateResponse(); imm != nil {
					t.Fatalf("a %d-byte %s body — at the listener's cap, not over it — was refused with %d; want it carried",
						tc.size, tc.phase, imm.GetStatus().GetCode())
				}
			}

			// TORN DOWN BEFORE THE STORE IS READ, and that ordering is what makes
			// the row assertions deterministic rather than a race. Process records
			// the response row from a DEFERRED flush, so on the over-limit
			// response case the row appears only once the handler has unwound;
			// CloseSend ends the stream for the cases where the handler is still
			// parked on Recv, and GracefulStop waits for it to return. Both are
			// no-ops for what is asserted on the cases that record nothing in the
			// flush, which is why every case goes through the same two calls
			// instead of some reading the store early.
			_ = stream.CloseSend()
			srv.GracefulStop()

			assertGRPCLimitRows(t, store, tc.wantRows)
		})
	}
}

// assertGRPCLimitRows compares the default session's events to want, in order.
// Exact: a row the fixture did not list is a failure, because "this request
// left nothing behind" and "this request was recorded as a healthy 200" are the
// two answers this file exists to tell apart.
func assertGRPCLimitRows(t *testing.T, store *session.Store, want []grpcLimitRow) {
	t.Helper()

	var got []pipeline.SessionEvent
	if v := store.View(session.DefaultSessionID); v != nil {
		got = v.Events
	}
	if len(got) != len(want) {
		for i, e := range got {
			t.Logf("  row %d: phase=%v status=%d plugins=%v", i, e.Phase, e.StatusCode, e.Plugins)
		}
		t.Fatalf("session holds %d row(s), want %d", len(got), len(want))
	}
	for i, w := range want {
		e := got[i]
		where := fmt.Sprintf("row %d (%v)", i, w.phase)
		if e.Phase != w.phase {
			t.Errorf("%s: phase = %v, want %v", where, e.Phase, w.phase)
		}
		if e.StatusCode != w.statusCode {
			t.Errorf("%s: StatusCode = %d, want %d", where, e.StatusCode, w.statusCode)
		}
		if e.Error != nil {
			// Stated as an assertion rather than left implicit: the whole point
			// of the over-limit response row is that it carries NO error, so a
			// future change that started recording one would be an improvement
			// this test must notice rather than ignore.
			t.Errorf("%s: carried an error: kind=%q code=%q msg=%q", where, e.Error.Kind, e.Error.Code, e.Error.Message)
		}
		for key, wantJSON := range w.pluginEvents {
			raw, ok := e.Plugins[key]
			if !ok {
				t.Errorf("%s: missing expected plugin event %q", where, key)
				continue
			}
			if !jsonEqual(string(raw), wantJSON) {
				t.Errorf("%s: plugin event %q\n  got:  %s\n  want: %s", where, key, raw, wantJSON)
			}
		}
		for key, raw := range e.Plugins {
			if _, ok := w.pluginEvents[key]; !ok {
				t.Errorf("%s: unexpected plugin event %q: %s", where, key, raw)
			}
		}
	}
}

// newExtprocGRPCServer stands up the real listener behind a real gRPC server
// over bufconn, reproducing cmd/cortex-envoy's newExtProcServer: a bare
// grpc.NewServer() with NO options, so the server inherits grpc-go's default
// receive limit as production does today.
//
// A reproduction, which is as close as this package can get — core cannot
// import package main. It means an option added to the real newExtProcServer
// would not fail anything here, so that call site has its own test beside it:
// cmd/cortex-envoy's TestStartGRPCExtProcKeepsDefaultRecvLimit.
//
// The outbound slot gets the body-reading spy and inbound an empty pipeline,
// matching the direction headersRequest declares. Outbound rather than inbound
// because that is the leg where a lost response is lost spend.
func newExtprocGRPCServer(t *testing.T) (extprocv3.ExternalProcessorClient, *session.Store, *grpc.Server) {
	t.Helper()

	spyPipe, err := plugins.BuildWithDeps([]config.PluginEntry{spyEntry(spyPluginA, spyConfig{
		ReadsBody:                true,
		RecordRequestBodyDigest:  true,
		RecordResponseBodyDigest: true,
	})}, plugins.Deps{})
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	emptyPipe, err := plugins.BuildWithDeps(nil, plugins.Deps{})
	if err != nil {
		t.Fatalf("BuildWithDeps(nil): %v", err)
	}

	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, &extproc.Server{
		InboundPipeline:  pipeline.NewHolder(emptyPipe),
		OutboundPipeline: pipeline.NewHolder(spyPipe),
		Sessions:         store,
	})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return extprocv3.NewExternalProcessorClient(conn), store, srv
}

// headersRequest is the RequestHeaders message Envoy sends for an outbound POST
// whose body is contentLength bytes. Zero announces a body-less request, which
// is how the response-phase cases get the request phase to complete at headers.
func headersRequest(contentLength int) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "POST",
					":path", "/grpc-limit/big-body",
					":authority", "parity.local",
					"content-length", fmt.Sprintf("%d", contentLength),
				),
			},
		},
	}
}

// responseHeadersRequest is the upstream's 200 with a body still to come —
// end_of_stream false, as Envoy sends it when a body follows.
func responseHeadersRequest(contentLength int) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				Headers: makeHeaders(
					":status", "200",
					"content-type", "application/json",
					"content-length", fmt.Sprintf("%d", contentLength),
				),
				EndOfStream: false,
			},
		},
	}
}
