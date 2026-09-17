package sseframe

import (
	"io"
	"strings"
	"testing"
)

// bom is U+FEFF as it arrives on the wire, written in escapes because a literal one in a Go
// source file is a compile error ("illegal byte order mark").
const bom = "\xef\xbb\xbf"

// anthropicTurn is the two events that matter for money: the prompt tally, then the output one.
const anthropicTurn = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":600}}}\n\n" +
	"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n"

// TestReadFrame_LeadingBOM is must-fix 3 of review round 7.
//
// A leading byte-order mark made the first line parse as a field whose NAME begins with those
// three bytes, matching neither "data" nor "event", so ReadFrame skipped the entire FIRST EVENT.
// Measured
// before the fix: 2 events without the BOM, 1 with it — and the one that vanished was
// message_start, which carries the whole prompt and cache-read tally. The turn then settles from
// the output count alone, which is a silent underbill on a body that parses cleanly everywhere
// else.
//
// WHY IT BELONGS IN THE READER. inferenceparser.normalizeSSE strips a BOM as well, but only for
// the two whole-body parsers; every PER-FRAME path — ext_proc's buffered re-parse, both proxies'
// streaming paths — goes through this reader instead, so that fix covered none of the streaming
// traffic. Fixing it here also fixes the shape detector, which reads through the same code.
func TestReadFrame_LeadingBOM(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"no BOM", anthropicTurn, 2},
		{"one leading BOM", bom + anthropicTurn, 2},
		// TWO of them is still one at the start plus a stray inside the first line, so the first
		// event is lost — this reader removes the one the spec names and does not go hunting.
		// Pinned so the boundary is a decision rather than an accident.
		{"a doubled BOM keeps only the spec's rule", bom + bom + anthropicTurn, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readAll(t, tc.in)
			if len(got) != tc.want {
				t.Fatalf("got %d events, want %d: %q", len(got), tc.want, got)
			}
			if tc.want == 2 && !strings.Contains(got[0], "input_tokens") {
				t.Errorf("first event = %q, want the prompt tally: losing message_start settles the turn from the output count alone", got[0])
			}
		})
	}
}

// TestReadFrame_BOMIsNotStrippedMidStream pins the other half of "once, at the start".
//
// The flag lives on the Reader so the check cannot re-run per frame. Scanning further would mean
// deleting those three bytes from any payload that legitimately contains them — a real risk here,
// since these frames carry arbitrary JSON, including whatever text a model generated.
func TestReadFrame_BOMIsNotStrippedMidStream(t *testing.T) {
	// AT A LINE START, not mid-value, which is the only position that discriminates. With the BOM
	// inside the payload ("data: <bom>second") the check at the top of ReadFrame never looks there,
	// so `if !r.bomChecked` → `if true` stayed green and this test proved nothing about the flag it
	// documents. Here the BOM begins the second event's line, exactly where a per-frame check would
	// strip it — and stripping it would make that line parse as a data field.
	got := readAll(t, "data: first\n\n"+bom+"data: second\n\n")
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 — the second event's field name carries the BOM, so it is not \"data\" and the event is skipped: %q", len(got), got)
	}
	if got[0] != "first" {
		t.Errorf("first event = %q, want %q", got[0], "first")
	}
}

func readAll(t *testing.T, in string) []string {
	t.Helper()
	r := NewReader(strings.NewReader(in), 1<<20)
	var out []string
	for {
		frame, err := r.ReadFrame()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		out = append(out, string(frame))
	}
}
