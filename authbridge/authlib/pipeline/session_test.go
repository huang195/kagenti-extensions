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
