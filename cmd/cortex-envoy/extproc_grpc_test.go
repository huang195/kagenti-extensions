package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// The gRPC receive limit THIS binary actually runs with.
//
// startGRPCExtProc passes grpc.NewServer() no options, so grpc-go's 4 MiB
// default MaxRecvMsgSize is the real ceiling on every ProcessingRequest Envoy
// sends — and in envoy-sidecar mode it is the third limit a large body meets,
// behind Envoy's per_connection_buffer_limit_bytes and ahead of the listener's
// own 1 MiB cap. core/listener/parity/grpclimit_test.go documents the three
// together and pins what the listener does under each.
//
// WHY THAT TEST IS NOT ENOUGH, AND WHY THIS ONE IS HERE. The parity package
// cannot import package main, so it builds its own bare grpc.NewServer() and
// reasons that production matches. It therefore pins grpc-go's DEFAULT; it
// cannot see this call site. Adding one option here —
// grpc.NewServer(grpc.MaxRecvMsgSize(16 << 20)) — moves the real ceiling and
// leaves every assertion over there green, while three comments in that package
// go on saying "a bare grpc.NewServer() and no options". This test is the half
// that fails when that happens.
//
// Pinning the figure rather than inspecting the options is deliberate: grpc.Server
// exposes no accessor for its receive limit, and an assertion on the ServerOption
// slice would pass a server that was handed the option twice or had it overridden
// elsewhere. The transport's own refusal is the property that matters.
//
// RAISING THE LIMIT IS NOT FORBIDDEN — it is one of the three things a cap
// unification has to raise, and #864's 5 MiB Claude Code requests are the reason
// to. What is forbidden is raising it silently: this failing is the signal to
// update the cap table in core/listener/parity and this test's own figure
// together.
func TestStartGRPCExtProcKeepsDefaultRecvLimit(t *testing.T) {
	// grpc-go's default, as a number, because that is what the transport names
	// in the error it returns.
	const wantLimit = "4194304"

	empty, err := plugins.BuildWithDeps(nil, plugins.Deps{})
	if err != nil {
		t.Fatalf("BuildWithDeps(nil): %v", err)
	}

	// startGRPCExtProc listens itself, so a port has to be picked for it — hence
	// bind-then-release rather than ":0". The window between Close and the
	// server's own Listen is a race nothing here can close, and losing it is
	// log.Fatalf inside startGRPCExtProc, which takes the test binary down with
	// it. A loopback ephemeral port is not reassigned that fast in practice; a
	// mysterious "ext_proc listen" fatal in CI is this and not the property under
	// test.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}

	// Empty pipelines both ways and no session store: the refusal happens in the
	// transport, before the handler reads a message, so nothing below depends on
	// what the listener would have done with the body.
	srv := startGRPCExtProc(pipeline.NewHolder(empty), pipeline.NewHolder(empty), nil, nil, nil, addr)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// WaitForReady because startGRPCExtProc's net.Listen runs in a goroutine it
	// does not synchronise on, so the port may not be accepting yet. Without it
	// this test is flaky-by-construction.
	stream, err := extprocv3.NewExternalProcessorClient(conn).Process(ctx, grpc.WaitForReady(true))
	if err != nil {
		t.Fatalf("opening the Process stream: %v", err)
	}

	// 5 MiB: over grpc-go's 4 MiB default and in the band #864 measured. The
	// Send is not asserted on — grpc-go does not police an outgoing message
	// against the peer's limit, so the refusal arrives as the stream's status on
	// Recv.
	_ = stream.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{
		RequestBody: &extprocv3.HttpBody{Body: make([]byte, 5<<20), EndOfStream: true},
	}})

	_, err = stream.Recv()
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), wantLimit) {
		t.Fatalf("startGRPCExtProc no longer refuses a 5 MiB message at grpc-go's %s-byte default; "+
			"if that is intentional, update the cap table in core/listener/parity and the figure here together: %v",
			wantLimit, err)
	}
}
