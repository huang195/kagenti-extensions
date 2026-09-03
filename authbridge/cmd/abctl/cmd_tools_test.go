package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const scanTestConfig = `mode: proxy-sidecar
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: tool-prune
        config:
          remove: []
`

// writeTranscript writes one JSONL transcript. withCall controls whether it
// contains a tool_use block, which is the scan's only evidence.
func writeTranscript(t *testing.T, dir, name string, withCall bool) {
	t.Helper()
	line := `{"timestamp":"` + nowStamp() + `","message":{"content":[{"type":"text","text":"hi"}]}}`
	if withCall {
		line = `{"timestamp":"` + nowStamp() + `","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func nowStamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// TestToolsScan_RefusesToWriteWithoutEvidence: with no observed tool calls,
// "tools you have not called" is every tool it knows, so writing that list
// unattended would propose removing tools the session needs. A new install is
// exactly this case, which is also when the default is most likely accepted.
func TestToolsScan_RefusesToWriteWithoutEvidence(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, tdir, "a.jsonl", false) // no tool_use anywhere

	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(scanTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := runTools([]string{"scan", "--dir", tdir, "--write", cfg}, &out, &errb)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero when there is no evidence")
	}
	if !strings.Contains(errb.String(), "no tool calls") {
		t.Errorf("stderr did not explain the refusal: %q", errb.String())
	}
	// The config must be untouched.
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != scanTestConfig {
		t.Errorf("config was modified despite the refusal:\n%s", after)
	}
	// The proposal is still printed — refusing to write it is not refusing to
	// show it.
	if !strings.Contains(out.String(), "remove:") {
		t.Errorf("stdout did not include the proposal: %q", out.String())
	}
}

// TestToolsScan_WritesWithEvidence is the paired positive case, so the guard
// above can't pass by simply never writing.
func TestToolsScan_WritesWithEvidence(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, tdir, "a.jsonl", true) // one real tool_use

	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(scanTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runTools([]string{"scan", "--dir", tdir, "--write", cfg}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0. stderr: %s", code, errb.String())
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == scanTestConfig {
		t.Error("config was not updated despite real evidence")
	}
}
