package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// settingsWithSecret is shaped like a real settings.json: unrelated top-level
// keys, and an env block already holding a gateway URL and an auth token. Those
// must survive untouched — the whole risk of this command is collateral damage to
// a file the user did not ask us to reorganise.
const settingsWithSecret = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(git:*)"]},
  "env": {
    "ANTHROPIC_BASE_URL": "https://gateway.example.com",
    "ANTHROPIC_AUTH_TOKEN": "sk-do-not-touch",
    "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"
  }
}`

const cortexCfg = `mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
  health_addr: 127.0.0.1:47604
tls_bridge:
  mode: enabled
  ca_dir: "CADIR"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`

func fixture(t *testing.T, settings string) (settingsPath, cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	settingsPath = filepath.Join(dir, "settings.json")
	if settings != "" {
		if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	body := strings.Replace(cortexCfg, "CADIR", filepath.Join(dir, "ca"), 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return settingsPath, cfgPath
}

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}
	out := map[string]string{}
	if e, ok := doc["env"].(map[string]any); ok {
		for k, v := range e {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// TestClaudeCodeEnable_PreservesEverythingElse is the property that matters most:
// this file routinely holds an API token, and we are editing it on the user's
// behalf.
func TestClaudeCodeEnable_PreservesEverythingElse(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	env := readEnv(t, settings)
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-do-not-touch" {
		t.Errorf("auth token altered: %q", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://gateway.example.com" {
		t.Errorf("base URL altered: %q", env["ANTHROPIC_BASE_URL"])
	}
	if env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] != "1" {
		t.Error("an unrelated env entry was dropped")
	}
	// Addresses come from the Cortex config, not a hardcoded constant.
	if env[envProxy] != "http://127.0.0.1:47600" {
		t.Errorf("%s = %q", envProxy, env[envProxy])
	}
	if !strings.HasSuffix(env[envCACerts], filepath.Join("ca", "ca.crt")) {
		t.Errorf("%s = %q, want it under the config's ca_dir", envCACerts, env[envCACerts])
	}
	if env[envNoTelem] != "1" {
		t.Errorf("%s = %q", envNoTelem, env[envNoTelem])
	}

	// Unrelated top-level keys survive.
	b, _ := os.ReadFile(settings)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"model", "permissions"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("top-level key %q was dropped", k)
		}
	}
	if _, err := os.Stat(settings + ".bak"); err != nil {
		t.Errorf("no backup written: %v", err)
	}
}

// TestClaudeCodeEnable_ReadsAddressesFromConfig: hardcoding 47600 would point
// Claude Code at nothing the moment someone edited their Cortex config.
func TestClaudeCodeEnable_ReadsAddressesFromConfig(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(string(body), "127.0.0.1:47600", "127.0.0.1:19999", 1)
	if err := os.WriteFile(cfg, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := readEnv(t, settings)[envProxy]; got != "http://127.0.0.1:19999" {
		t.Errorf("%s = %q, want the config's port", envProxy, got)
	}
}

// TestClaudeCodeEnable_RefusesToClobberForeignProxy: someone behind a corporate
// proxy already has HTTPS_PROXY set. Replacing it would break their network and
// give no clue why.
func TestClaudeCodeEnable_RefusesToClobberForeignProxy(t *testing.T) {
	settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code == 0 {
		t.Fatal("accepted a foreign HTTPS_PROXY")
	}
	if !strings.Contains(errb.String(), "Refusing to overwrite") {
		t.Errorf("error did not explain itself: %q", errb.String())
	}
	if got := readEnv(t, settings)[envProxy]; got != "http://corp:3128" {
		t.Errorf("value was changed to %q despite the refusal", got)
	}
}

// TestClaudeCodeDisable_RemovesOnlyOurKeys pairs with the enable test: the off
// switch must not take the user's own settings with it.
func TestClaudeCodeDisable_RemovesOnlyOurKeys(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("enable: %s", errb.String())
	}
	if code := claudeCodeDisable(settings, true, &out, &errb); code != 0 {
		t.Fatalf("disable: %s", errb.String())
	}
	env := readEnv(t, settings)
	for _, k := range managedKeys {
		if _, ok := env[k]; ok {
			t.Errorf("%s survived disable", k)
		}
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-do-not-touch" {
		t.Error("disable removed the user's token")
	}
	if env["ANTHROPIC_BASE_URL"] == "" {
		t.Error("disable removed the user's base URL")
	}
}

// TestClaudeCodeEnable_Idempotent: install.sh may run this on every invocation.
func TestClaudeCodeEnable_Idempotent(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("first: %s", errb.String())
	}
	first, _ := os.ReadFile(settings)
	out.Reset()
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("second: %s", errb.String())
	}
	second, _ := os.ReadFile(settings)
	if string(first) != string(second) {
		t.Error("second run changed the file")
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Errorf("second run did not report it was already done: %q", out.String())
	}
}

// TestClaudeCodeEnable_MissingSettingsFileIsCreated: a fresh machine may have no
// settings.json at all.
func TestClaudeCodeEnable_MissingSettingsFileIsCreated(t *testing.T) {
	settings, cfg := fixture(t, "")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := readEnv(t, settings)[envNoTelem]; got != "1" {
		t.Errorf("%s = %q", envNoTelem, got)
	}
}

// TestClaudeCodeEnable_RejectsBrokenJSON: overwriting a file we cannot parse
// would destroy settings we never read.
func TestClaudeCodeEnable_RejectsBrokenJSON(t *testing.T) {
	settings, cfg := fixture(t, `{"env": {`)
	var out, errb bytes.Buffer
	if code := claudeCodeEnable(settings, cfg, true, &out, &errb); code == 0 {
		t.Fatal("accepted unparseable settings")
	}
	if !strings.Contains(errb.String(), "not valid JSON") {
		t.Errorf("error did not name the problem: %q", errb.String())
	}
}

// TestConfirmFrom_OnlyExplicitYesApplies: the file holds API tokens, so anything
// ambiguous — including EOF — must decline.
func TestConfirmFrom_OnlyExplicitYesApplies(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true}, {"Y\n", true}, {"yes\n", true}, {"YES\n", true},
		{"n\n", false}, {"no\n", false}, {"\n", false}, {"", false},
		{"maybe\n", false}, {"ya\n", false},
	} {
		var out bytes.Buffer
		if got := confirmFrom(strings.NewReader(tc.in), &out); got != tc.want {
			t.Errorf("confirmFrom(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if !strings.Contains(out.String(), "[y/N]") {
			t.Errorf("prompt did not show the default: %q", out.String())
		}
	}
}
