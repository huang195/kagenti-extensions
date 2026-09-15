package forwardproxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// getUA issues a proxied request carrying a specific User-Agent. An empty ua
// suppresses the header entirely rather than sending a blank one — net/http omits
// the line when the value is "" — which is the "no client" case the recorders have
// to keep as absence.
func getUA(t *testing.T, client *http.Client, backendURL, ua string) {
	t.Helper()
	req, err := http.NewRequest("POST", backendURL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
}

// TestForwardProxy_BothPhasesCarryTheClient is the end-to-end population guard for
// the two recorders on the live request path: the request event built inline in
// serveOutbound and the response event built by recordOutboundResponseEvent.
//
// Driven through a real proxied request rather than by calling the recorders
// directly, because serveOutbound's event is constructed inside the handler and has
// no callable seam — and because this is the only assertion that proves the
// User-Agent actually survives the hop from r.Header into pctx.Headers, which is
// where ClientInfo reads it. BOTH phases are asserted: a turn produces two events,
// and attributing only one of them would halve every per-agent figure.
func TestForwardProxy_BothPhasesCarryTheClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	_, client, backendURL := newProbedProxy(t, store)

	getUA(t, client, backendURL, "claude-cli/2.1.14 (external, cli)")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 2 {
		t.Fatalf("expected a request and a response event, got %+v", v)
	}
	var sawRequest, sawResponse bool
	for _, ev := range v.Events {
		if ev.Client == nil {
			t.Fatalf("phase %s: Client is nil; the recorder does not copy pctx.ClientInfo()", ev.Phase)
		}
		if ev.Client.Name != "claude-code" || ev.Client.Version != "2.1.14" {
			t.Errorf("phase %s: Client = %+v, want claude-code/2.1.14", ev.Phase, ev.Client)
		}
		// Raw survives the whole path, which is what makes an agent this parser does
		// not recognise still nameable in a breakdown.
		if ev.Client.Raw != "claude-cli/2.1.14 (external, cli)" {
			t.Errorf("phase %s: Raw = %q, want the verbatim UA", ev.Phase, ev.Client.Raw)
		}
		switch ev.Phase {
		case pipeline.SessionRequest:
			sawRequest = true
		case pipeline.SessionResponse:
			sawResponse = true
		}
	}
	if !sawRequest {
		t.Error("no request-phase event recorded; serveOutbound's site is unasserted")
	}
	if !sawResponse {
		t.Error("no response-phase event recorded; recordOutboundResponseEvent's site is unasserted")
	}
}

// TestForwardProxy_NoUserAgentStaysAbsent pins that absence is preserved rather
// than filled in. A recorder that substituted a placeholder would satisfy the test
// above and invent an agent for traffic that named none — which in a cost table
// reads as a real program that spent real money.
//
// DISCRIMINATING, not one-sided: both requests go through the same proxy into the same
// store, and the events have to split into named ones and absent ones. Asserting only
// "want nil" made this test pass for the wrong reason — deleting the recorders' `Client:`
// assignment outright left it green, because nil was all it checked for — so the nil it
// reports now is the absence of the HEADER rather than the absence of the wiring.
//
// The nil's Label() is deliberately NOT asserted here. Label is nil-safe by construction,
// so `ev.Client.Label() == "unknown"` restates pipeline.EventClient.Label's own contract,
// which its "nil is unknown" table row pins in that package, and it cannot fail for
// anything a recorder does or omits. What a recorder CAN get wrong is which requests carry
// a client at all, and that is what the counts below assert.
func TestForwardProxy_NoUserAgentStaysAbsent(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	_, client, backendURL := newProbedProxy(t, store)

	// The positive control goes first, so both shapes are recorded by the same server into
	// one view and the comparison is between the two REQUESTS rather than between two runs.
	getUA(t, client, backendURL, "claude-cli/2.1.14 (external, cli)")
	getUA(t, client, backendURL, "")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 4 {
		t.Fatalf("expected a request and a response event from each of two requests, got %+v", v)
	}
	var named, absent int
	for _, ev := range v.Events {
		if ev.Client == nil {
			absent++
			continue
		}
		named++
		if ev.Client.Name != "claude-code" || ev.Client.Version != "2.1.14" {
			t.Errorf("phase %s: Client = %+v, want claude-code/2.1.14", ev.Phase, ev.Client)
		}
	}
	if named == 0 {
		t.Error("no event carried a client at all; the recorders do not copy pctx.ClientInfo(), so the absence here is missing WIRING and not a missing User-Agent")
	}
	if absent == 0 {
		t.Error("every event carried a client; the UA-less request was given one, and an invented agent in a cost table reads as a real program that spent real money")
	}
	if named != absent {
		t.Errorf("%d events named a client and %d did not; the two requests record the same events, so an uneven split means one phase is attributed and the other is not — which halves or doubles every per-agent figure",
			named, absent)
	}
}

// TestForwardProxy_UnrecognisedAgentIsStillNameable is the reason Raw is kept
// alongside Name. A coding agent this parser has never heard of must appear in the
// breakdown under its own User-Agent the day someone runs it, not pool with
// untagged traffic until a parser update ships.
func TestForwardProxy_UnrecognisedAgentIsStillNameable(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	_, client, backendURL := newProbedProxy(t, store)

	getUA(t, client, backendURL, "SomeNewAgent/9.9")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) == 0 {
		t.Fatalf("expected events, got %+v", v)
	}
	ev := v.Events[0]
	if ev.Client == nil {
		t.Fatal("Client is nil; a User-Agent was sent, so this is unrecognised rather than absent")
	}
	if ev.Client.Name != "" {
		t.Errorf("Name = %q, want empty: the parser must not guess a canonical name", ev.Client.Name)
	}
	if got := ev.Client.Label(); got != "SomeNewAgent/9.9" {
		t.Errorf("Label() = %q, want the raw UA — never %q, which is reserved for absence", got, "unknown")
	}
}

// TestRecordOutboundReject_CarriesTheClient covers the third forwardproxy site.
// Called directly: a denial is a terminal event on a path that needs a rejecting
// plugin to reach, and the recorder is the unit under test.
func TestRecordOutboundReject_CarriesTheClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	h := http.Header{}
	h.Set("User-Agent", "claude-cli/2.1.14")
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "POST",
		Host:      "api.anthropic.com",
		Path:      "/v1/messages",
		Headers:   h,
		// The recorder skips a denial that carries no invocations — a content-free
		// SessionDenied would be noise without attribution — so the fixture has to
		// supply one to reach the construction site at all.
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Outbound: []pipeline.Invocation{{
					Plugin: "rate-limit", Phase: pipeline.InvocationPhaseRequest,
					Action: pipeline.ActionDeny, Reason: "over budget",
				}},
			},
		},
	}
	s.recordOutboundReject(pctx, pipeline.Action{Type: pipeline.Reject}, session.DefaultSessionID)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 denial event, got %+v", v)
	}
	if got := v.Events[0].Client.Label(); got != "claude-code/2.1.14" {
		t.Errorf("Client.Label() = %q, want claude-code/2.1.14", got)
	}
}

// TestRecordTunnelOpened_CarriesTheClient covers the transparent.go site.
//
// A tunnel-open is the one recorder whose client may legitimately be absent even
// from a real agent: a transparently redirected connection has no HTTP request to
// read a header from. Asserted with the header present because the proxied-CONNECT
// half of the same function does have one, and an unattributed tunnel is the
// weaker of the two claims to pin.
func TestRecordTunnelOpened_CarriesTheClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	h := http.Header{}
	h.Set("User-Agent", "claude-cli/2.1.14")
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "CONNECT",
		Host:      "api.anthropic.com:443",
		Headers:   h,
	}
	s.recordTunnelOpened(pctx, pipeline.TunnelPassthroughHost)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 tunnel-open event, got %+v", v)
	}
	if got := v.Events[0].Client.Label(); got != "claude-code/2.1.14" {
		t.Errorf("Client.Label() = %q, want claude-code/2.1.14", got)
	}
}

// TestRecordTunnelOpened_TransparentRedirectHasNoClient pins the accurate negative:
// a transparently redirected connection carries no HTTP headers at all, so its
// tunnel row genuinely has no client. Without this test someone "fixing" the
// missing attribution would be fixing a correct answer — the same asymmetry
// TestRecordTunnelOpened_EmptyPathIsAccurate pins for HTTPPath.
func TestRecordTunnelOpened_TransparentRedirectHasNoClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	// Mirrors HandleTransparentConn's context: synthetic CONNECT, no Headers.
	s.recordTunnelOpened(&pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "CONNECT",
		Host:      "api.anthropic.com:443",
	}, pipeline.TunnelPassthroughHost)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 tunnel-open event, got %+v", v)
	}
	if ev := v.Events[0]; ev.Client != nil {
		t.Errorf("Client = %+v, want nil: opaque redirected bytes carry no headers", ev.Client)
	}
}
