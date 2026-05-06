package pipeline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDirectionString(t *testing.T) {
	if Inbound.String() != "inbound" {
		t.Errorf("Inbound.String() = %q, want %q", Inbound.String(), "inbound")
	}
	if Outbound.String() != "outbound" {
		t.Errorf("Outbound.String() = %q, want %q", Outbound.String(), "outbound")
	}
	if got := Direction(42).String(); got != "unknown" {
		t.Errorf("unknown direction = %q, want %q", got, "unknown")
	}
}

func TestSessionEvent_MarshalJSON_ReadableEnums(t *testing.T) {
	e := SessionEvent{
		At:        time.Unix(1700000000, 0).UTC(),
		Direction: Inbound,
		Phase:     SessionResponse,
		Duration:  250 * time.Millisecond,
		Host:      "weather-tool-mcp:9090",
		A2A:       &A2AExtension{Method: "message/stream"},
	}

	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	// Enums must be strings, not numbers.
	if !strings.Contains(s, `"direction":"inbound"`) {
		t.Errorf("direction not serialized as string: %s", s)
	}
	if !strings.Contains(s, `"phase":"response"`) {
		t.Errorf("phase not serialized as string: %s", s)
	}
	// Duration must be emitted in ms, not the default nanosecond form.
	if !strings.Contains(s, `"durationMs":250`) {
		t.Errorf("durationMs missing or wrong: %s", s)
	}
}

// TestSessionEvent_JSONRoundTrip locks in the round-trip contract between
// MarshalJSON and UnmarshalJSON. A second Marshal of the decoded event must
// produce byte-identical JSON — that's the canary that catches fields added
// to SessionEvent + the wire struct + MarshalJSON but forgotten in
// UnmarshalJSON (the dropped field would silently vanish on the client).
func TestSessionEvent_JSONRoundTrip(t *testing.T) {
	stream := true
	maxTok := 256
	orig := SessionEvent{
		SessionID: "sess-xyz",
		At:        time.Unix(1700000000, 0).UTC(),
		Direction: Outbound,
		Phase:     SessionResponse,
		Duration:  1600 * time.Millisecond,
		Host:      "api.openai.com",
		A2A: &A2AExtension{
			Method: "message/stream", RPCID: "r-1", SessionID: "ctx-1",
			TaskID: "t-1", FinalStatus: "completed", Artifact: "hello",
			Role: "user", Parts: []A2APart{{Kind: "text", Content: "hi"}},
		},
		MCP: &MCPExtension{Method: "tools/call", RPCID: "m-2"},
		Inference: &InferenceExtension{
			Model: "gpt-4", Stream: stream, MaxTokens: &maxTok,
			Messages:  []InferenceMessage{{Role: "user", Content: "hi"}},
			Tools:     []InferenceTool{{Name: "get_weather", Description: "d"}},
			ToolCalls: []InferenceToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"NYC"}`}},
			Completion: "Hello, world!", FinishReason: "stop", TotalTokens: 17,
		},
		Identity:       &EventIdentity{Subject: "alice", ClientID: "agent-1"},
		StatusCode:     200,
		Error:          &EventError{Kind: "upstream", Message: "timeout"},
		TargetAudience: "github-tool",
	}

	first, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("first Marshal: %v", err)
	}
	var decoded SessionEvent
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	second, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("second Marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("round-trip drifted:\n  first:  %s\n  second: %s", first, second)
	}
}

func TestSessionEvent_MarshalJSON_OmitsEmpty(t *testing.T) {
	e := SessionEvent{Direction: Outbound, Phase: SessionRequest}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	for _, field := range []string{"a2a", "mcp", "inference", "identity", "statusCode", "error", "host", "targetAudience", "durationMs"} {
		if strings.Contains(s, `"`+field+`":`) {
			t.Errorf("expected %q omitted when zero: %s", field, s)
		}
	}
}
