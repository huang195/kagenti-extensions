package sessionapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

// newTestServer wires a Server backed by a fresh Store onto httptest so tests
// hit the real mux and real handlers. Returns a teardown closer.
func newTestServer(t *testing.T, opts ...Option) (*httptest.Server, *session.Store) {
	t.Helper()
	store := session.New(5*time.Minute, 100, 0)
	// Use a tiny heartbeat by default so SSE tests don't hang.
	opts = append([]Option{WithHeartbeatInterval(50 * time.Millisecond)}, opts...)
	srv := New(":0", store, opts...)
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return ts, store
}

func TestHandleList_Empty(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(body.Sessions))
	}
}

func TestHandleList_ShowsAppendedSessions(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})
	store.Append("s2", pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})
	store.Append("s2", pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})

	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d (%+v)", len(body.Sessions), body.Sessions)
	}
	// Most recently updated first.
	if body.Sessions[0].ID != "s2" {
		t.Errorf("first = %q, want s2", body.Sessions[0].ID)
	}
	if body.Sessions[0].EventCount != 2 {
		t.Errorf("s2 eventCount = %d, want 2", body.Sessions[0].EventCount)
	}
	if !body.Sessions[0].Active {
		t.Errorf("s2 should be marked active")
	}
}

func TestHandleGet_NotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/sessions/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleGet_ReturnsEventsAndReadableEnums(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", pipeline.SessionEvent{
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionResponse,
		A2A:       &pipeline.A2AExtension{Method: "message/stream"},
	})

	resp, err := http.Get(ts.URL + "/v1/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"direction":"inbound"`) {
		t.Errorf("expected string enums in payload: %s", raw)
	}
	if !strings.Contains(string(raw), `"phase":"response"`) {
		t.Errorf("expected string phase in payload: %s", raw)
	}
}

func TestHandleStream_DeliversAppendedEvent(t *testing.T) {
	ts, store := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}

	// Scanner with a 4MB buffer; SSE frames can be large.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 8192), 4<<20)

	// Wait for the initial ": ok" comment to know the handler is ready.
	waitForLine(t, sc, ":", "initial comment", 2*time.Second)

	// Now append an event and expect it in the stream.
	store.Append("s1", pipeline.SessionEvent{
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/stream"},
	})

	// Expect event/id/data lines, in order, within 2s.
	sawEvent := scanUntil(t, sc, "event: session-event", 2*time.Second)
	if !sawEvent {
		t.Fatal("event: session-event line never arrived")
	}
	sawID := scanUntil(t, sc, "id: 1", time.Second)
	if !sawID {
		t.Error("id: 1 line never arrived")
	}
	sawData := scanUntilPrefix(t, sc, "data: ", time.Second)
	if sawData == "" {
		t.Fatal("data: line never arrived")
	}
	if !strings.Contains(sawData, `"method":"message/stream"`) {
		t.Errorf("data line missing expected field: %s", sawData)
	}
}

func TestHandleStream_Heartbeat(t *testing.T) {
	// With heartbeat=25ms, we should see at least one heartbeat comment
	// within 200ms even with no events appended.
	ts, _ := newTestServer(t, WithHeartbeatInterval(25*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	// Consume the initial ": ok", then expect ": heartbeat" within the window.
	waitForLine(t, sc, ":", "initial", time.Second)
	got := scanUntilExact(t, sc, ": heartbeat", 500*time.Millisecond)
	if !got {
		t.Error("no heartbeat observed within 500ms")
	}
}

func TestHandleStream_SessionFilter(t *testing.T) {
	ts, store := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events?session=keep", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 8192), 4<<20)
	waitForLine(t, sc, ":", "initial", time.Second)

	// Event with a different SessionID — should be filtered out.
	store.Append("drop", pipeline.SessionEvent{
		A2A: &pipeline.A2AExtension{Method: "message/stream"},
	})
	// Event with matching SessionID — should come through.
	store.Append("keep", pipeline.SessionEvent{
		A2A: &pipeline.A2AExtension{Method: "message/stream"},
	})

	data := scanUntilPrefix(t, sc, "data: ", time.Second)
	if data == "" {
		t.Fatal("no data frame received")
	}
	if !strings.Contains(data, `"sessionId":"keep"`) {
		t.Errorf("expected keep session in stream, got: %s", data)
	}
	if strings.Contains(data, `"sessionId":"drop"`) {
		t.Errorf("unfiltered drop event leaked: %s", data)
	}
}

func TestHandleStream_SessionFilter_WorksOnOutboundMCP(t *testing.T) {
	// Prior to SessionEvent.SessionID being populated at Append time, this
	// test would have failed: the outbound MCP event has no A2A extension
	// and the old filter check bailed when A2A was nil, letting foreign
	// events through. Guards against regression.
	ts, store := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events?session=keep", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 8192), 4<<20)
	waitForLine(t, sc, ":", "initial", time.Second)

	// Outbound MCP call appended to the wrong bucket — must be filtered.
	store.Append("drop", pipeline.SessionEvent{
		MCP: &pipeline.MCPExtension{Method: "tools/call"},
	})
	// Outbound MCP call in the target bucket — must pass.
	store.Append("keep", pipeline.SessionEvent{
		MCP: &pipeline.MCPExtension{Method: "tools/list"},
	})

	data := scanUntilPrefix(t, sc, "data: ", time.Second)
	if data == "" {
		t.Fatal("no data frame received within deadline")
	}
	if !strings.Contains(data, `"sessionId":"keep"`) {
		t.Errorf("expected sessionId:keep, got: %s", data)
	}
	if !strings.Contains(data, `"method":"tools/list"`) {
		t.Errorf("expected MCP tools/list event, got: %s", data)
	}
}

func TestHandleHealthz(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("ok")) {
		t.Errorf("body = %q, want to contain \"ok\"", body)
	}
}

// --- helpers -------------------------------------------------------------

// waitForLine scans until a line equal to want appears or the deadline fires.
func waitForLine(t *testing.T, sc *bufio.Scanner, want, label string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			t.Fatalf("scanner closed before %s line; err=%v", label, sc.Err())
		}
		if sc.Text() == want || strings.HasPrefix(sc.Text(), want) {
			return
		}
	}
	t.Fatalf("deadline waiting for %s line (%q)", label, want)
}

// scanUntil returns true if line == want appears before the deadline.
func scanUntil(t *testing.T, sc *bufio.Scanner, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			return false
		}
		if sc.Text() == want {
			return true
		}
	}
	return false
}

// scanUntilExact returns true if the exact line appears before the deadline.
func scanUntilExact(t *testing.T, sc *bufio.Scanner, exact string, d time.Duration) bool {
	return scanUntil(t, sc, exact, d)
}

// scanUntilPrefix returns the line (once) if it starts with prefix, else "".
func scanUntilPrefix(t *testing.T, sc *bufio.Scanner, prefix string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			return ""
		}
		if strings.HasPrefix(sc.Text(), prefix) {
			return sc.Text()
		}
	}
	return ""
}
