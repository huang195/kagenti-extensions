package pipeline

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseUserAgent(t *testing.T) {
	for _, tc := range []struct {
		name, ua          string
		wantName, wantVer string
		wantNil           bool
	}{
		{"absent is nil", "", "", "", true},
		{"claude code", "claude-cli/2.1.14 (external, cli)", "claude-code", "2.1.14", false},
		{"claude code bare", "claude-cli/2.1.14", "claude-code", "2.1.14", false},
		{"unrecognised keeps raw only", "SomeNewAgent/9.9", "", "", false},
		{"curl is not a coding agent", "curl/8.4.0", "", "", false},
		{"whitespace only is nil", "   ", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUserAgent(tc.ua)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("ParseUserAgent(%q) = %+v, want nil", tc.ua, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseUserAgent(%q) = nil, want a client", tc.ua)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if got.Version != tc.wantVer {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVer)
			}
			// Raw is always kept: an unrecognised agent we can still NAME is data.
			if got.Raw == "" {
				t.Error("Raw is empty; the verbatim UA must be kept")
			}
		})
	}
}

func TestParseUserAgent_CapsTheRetainedValue(t *testing.T) {
	// The header is caller-controlled and the value is retained in bucket label
	// maps for a full ring lap. Without a cap a caller parks arbitrary bytes in
	// memory from off-host, on the synchronous session-append path — the same
	// reasoning as usage.maxLabelLen.
	long := strings.Repeat("A", 10_000)
	got := ParseUserAgent(long)
	if got == nil {
		t.Fatal("ParseUserAgent returned nil for a long UA")
	}
	if len(got.Raw) > maxClientLen {
		t.Errorf("Raw retained %d bytes, want <= %d", len(got.Raw), maxClientLen)
	}
}

func TestParseUserAgent_CapsBeforeMatching(t *testing.T) {
	// A caller cannot buy an unbounded Version either: the version is cut out of
	// the already-capped string, so no field on EventClient can exceed the cap.
	got := ParseUserAgent("claude-cli/" + strings.Repeat("9", 10_000))
	if got == nil {
		t.Fatal("ParseUserAgent returned nil")
	}
	if len(got.Version) > maxClientLen {
		t.Errorf("Version retained %d bytes, want <= %d", len(got.Version), maxClientLen)
	}
	if len(got.Label()) > maxClientLen+len(got.Name)+1 {
		t.Errorf("Label() is %d bytes; the cap must bound every derived string", len(got.Label()))
	}
}

func TestEventClient_Label(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *EventClient
		want string
	}{
		{"nil is unknown", nil, "unknown"},
		{"recognised", &EventClient{Name: "claude-code", Version: "2.1.14"}, "claude-code/2.1.14"},
		{"no version", &EventClient{Name: "claude-code"}, "claude-code"},
		{"unrecognised falls back to raw", &EventClient{Raw: "SomeNewAgent/9.9"}, "SomeNewAgent/9.9"},
		// The zero value cannot name anything, so it must not invent a name. This is
		// the case the "unknown" label exists for: absence, never a parse failure
		// wearing a plausible-looking agent name.
		{"empty struct is unknown", &EventClient{}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.Label(); got != tc.want {
				t.Errorf("Label() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContextClientInfo_ParsesFromTheRequestHeaders(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	c.Headers.Set("User-Agent", "claude-cli/2.1.14 (external, cli)")

	got := c.ClientInfo()

	if got == nil {
		t.Fatal("ClientInfo() = nil for a request carrying a User-Agent")
	}
	if got.Name != "claude-code" || got.Version != "2.1.14" {
		t.Errorf("ClientInfo() = %+v, want claude-code/2.1.14", got)
	}
}

func TestContextClientInfo_NilWhenNoUserAgent(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v, want nil", got)
	}
}

func TestContextClientInfo_NilHeadersDoesNotPanic(t *testing.T) {
	// A Context built by a listener that never set Headers. This runs on the
	// request hot path; a nil map read is fine but a nil *Context is not, and the
	// event sites call this unconditionally.
	c := &Context{}
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v, want nil", got)
	}
}

func TestContextClientInfo_MemoizesIncludingTheNilAnswer(t *testing.T) {
	// The subtle one. A nil result is a VALID memoized answer, so a bare nil check
	// as the memo guard would re-parse on every call for exactly the requests that
	// have nothing to parse — and every turn calls this at least twice, once per
	// event.
	c := &Context{Headers: http.Header{}}
	c.Headers.Set("User-Agent", "claude-cli/2.1.14")

	first := c.ClientInfo()
	// Mutating the header after the first call must not change the answer; if it
	// does, the value is being re-derived rather than memoized.
	c.Headers.Set("User-Agent", "something-else/9.9")
	second := c.ClientInfo()

	if first != second {
		t.Errorf("ClientInfo() returned different pointers across calls: %p then %p", first, second)
	}
	if second.Name != "claude-code" {
		t.Errorf("second call re-parsed the mutated header: %+v", second)
	}
}

func TestContextClientInfo_MemoizesTheNilCase(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	_ = c.ClientInfo() // nil
	c.Headers.Set("User-Agent", "claude-cli/2.1.14")
	if got := c.ClientInfo(); got != nil {
		t.Errorf("ClientInfo() = %+v after a memoized nil; want the nil to stick", got)
	}
}
