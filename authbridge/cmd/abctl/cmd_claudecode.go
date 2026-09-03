package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
)

// The three variables Claude Code needs to route through Cortex. Claude Code
// reads env vars from its own settings file, which is not merely more convenient
// than exporting them in a shell — it is more correct. The supervisor is one
// process shared by every terminal and inherits the environment of whichever
// shell cold-started it, so a shell export reaches background agents only by
// luck. Settings reach every session on the machine.
const (
	envProxy     = "HTTPS_PROXY"
	envCACerts   = "NODE_EXTRA_CA_CERTS"
	envNoTelem   = "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"
	settingsRel  = ".claude/settings.json"
	cortexCfgRel = ".cortex/config.yaml"
)

// managedKeys is exactly what enable writes and disable removes. Nothing else in
// the file is touched — notably not ANTHROPIC_BASE_URL or any auth token, which
// commonly live in the same env block.
var managedKeys = []string{envProxy, envCACerts, envNoTelem}

const claudeCodeUsage = `abctl claude-code — route Claude Code through Cortex without shell env vars

Usage:
  abctl claude-code enable  [--yes] [--settings PATH] [--config PATH]
  abctl claude-code disable [--yes] [--settings PATH]
  abctl claude-code status  [--settings PATH]

enable writes HTTPS_PROXY, NODE_EXTRA_CA_CERTS and
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC into the "env" block of
~/.claude/settings.json, reading the addresses from ~/.cortex/config.yaml so they
always match the running proxy. Afterwards, plain "claude" goes through Cortex.

Only those three keys are added; every other setting, including any other env
entry, is left exactly as it was. The previous file is copied to
settings.json.bak first. disable removes only those three keys.

Note: while enabled, Claude Code needs Cortex running — its requests go to the
proxy address. "abctl claude-code disable" is the off switch.

Flags:
  --yes           do not prompt for confirmation
  --settings PATH Claude Code settings file (default ~/.claude/settings.json)
  --config PATH   Cortex config to read addresses from (default ~/.cortex/config.yaml)
`

func runClaudeCode(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, claudeCodeUsage)
		return 2
	}
	action := args[0]

	fs := flag.NewFlagSet("claude-code "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "do not prompt for confirmation")
	settingsPath := fs.String("settings", "", "Claude Code settings file")
	cortexCfg := fs.String("config", "", "Cortex config file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "abctl: cannot determine your home directory: %v\n", err)
		return 1
	}
	if *settingsPath == "" {
		*settingsPath = filepath.Join(home, settingsRel)
	}
	if *cortexCfg == "" {
		*cortexCfg = filepath.Join(home, cortexCfgRel)
	}

	switch action {
	case "enable":
		return claudeCodeEnable(*settingsPath, *cortexCfg, *yes, stdout, stderr)
	case "disable":
		return claudeCodeDisable(*settingsPath, *yes, stdout, stderr)
	case "status":
		return claudeCodeStatus(*settingsPath, stdout)
	default:
		fmt.Fprintf(stderr, "abctl: unknown claude-code action %q (enable, disable, status)\n", action)
		return 2
	}
}

// wanted derives the three values from the Cortex config, so they cannot drift
// from the proxy that is actually running. Hardcoding 47600 here would silently
// point Claude Code at nothing the moment someone edited their config.
func wanted(cortexCfgPath string) (map[string]string, error) {
	cfg, err := config.Load(cortexCfgPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", cortexCfgPath, err)
	}
	addr := cfg.Listener.ForwardProxyAddr
	if addr == "" {
		return nil, fmt.Errorf("%s has no listener.forward_proxy_addr; Claude Code needs a forward proxy to point at", cortexCfgPath)
	}
	// A bind address is not a URL: ":8081" and "127.0.0.1:47600" both need a
	// host that a client can actually dial.
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		return nil, fmt.Errorf("listener.forward_proxy_addr %q is not host:port", addr)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	out := map[string]string{
		envProxy:   "http://" + host + ":" + port,
		envNoTelem: "1",
	}
	if cfg.TLSBridge.CADir != "" {
		ca, aerr := filepath.Abs(filepath.Join(cfg.TLSBridge.CADir, "ca.crt"))
		if aerr != nil {
			return nil, aerr
		}
		out[envCACerts] = ca
	}
	return out, nil
}

func claudeCodeEnable(settingsPath, cortexCfgPath string, yes bool, stdout, stderr io.Writer) int {
	want, err := wanted(cortexCfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	if _, ok := want[envCACerts]; !ok {
		fmt.Fprintf(stderr, "abctl: %s has no tls_bridge.ca_dir, so Claude Code has no CA to trust;\n"+
			"  requests would fail certificate verification. Enable the TLS bridge first.\n", cortexCfgPath)
		return 1
	}

	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	env := envBlock(doc)

	// Refuse to overwrite a value the user set to something else — most likely a
	// corporate proxy. Silently replacing it would break their network access and
	// give no clue why.
	for _, k := range managedKeys {
		if cur, ok := env[k]; ok && cur != want[k] && !isCortexValue(k, cur) {
			fmt.Fprintf(stderr, "abctl: %s is already set to %q in %s.\n"+
				"  Refusing to overwrite a value you set. Remove it first, or edit the file by hand.\n",
				k, cur, settingsPath)
			return 1
		}
	}

	var changes []string
	for _, k := range managedKeys {
		if env[k] != want[k] {
			changes = append(changes, fmt.Sprintf("  %s=%s", k, want[k]))
		}
	}
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "Already enabled: %s routes Claude Code through Cortex.\n", settingsPath)
		return 0
	}

	fmt.Fprintf(stdout, "This will add to the \"env\" block of %s:\n%s\n\n",
		settingsPath, strings.Join(changes, "\n"))
	fmt.Fprintf(stdout, "Everything else in the file is left alone, and the current version is\n"+
		"copied to %s.bak first. Afterwards, run Claude Code as plain `claude`.\n\n", settingsPath)
	fmt.Fprintf(stdout, "While enabled, Claude Code needs Cortex running. Undo with:\n"+
		"  abctl claude-code disable\n\n")
	if !yes && !confirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return 1
	}

	for _, k := range managedKeys {
		env[k] = want[k]
	}
	doc["env"] = env
	if err := writeSettings(settingsPath, doc); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\nEnabled. Run `claude` — no environment variables needed.\n")
	return 0
}

func claudeCodeDisable(settingsPath string, yes bool, stdout, stderr io.Writer) int {
	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	env := envBlock(doc)
	var present []string
	for _, k := range managedKeys {
		if _, ok := env[k]; ok {
			present = append(present, k)
		}
	}
	if len(present) == 0 {
		fmt.Fprintf(stdout, "Nothing to do: none of the Cortex variables are set in %s.\n", settingsPath)
		return 0
	}
	fmt.Fprintf(stdout, "This will remove from %s: %s\n\n", settingsPath, strings.Join(present, ", "))
	if !yes && !confirm(stdout) {
		fmt.Fprintln(stdout, "Not changed.")
		return 1
	}
	for _, k := range present {
		delete(env, k)
	}
	// Drop an env block we just emptied rather than leaving "env": {} behind.
	if len(env) == 0 {
		delete(doc, "env")
	} else {
		doc["env"] = env
	}
	if err := writeSettings(settingsPath, doc); err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\nDisabled. Claude Code no longer routes through Cortex.\n")
	return 0
}

func claudeCodeStatus(settingsPath string, stdout io.Writer) int {
	doc, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stdout, "not enabled (%v)\n", err)
		return 0
	}
	env := envBlock(doc)
	set := 0
	keys := make([]string, 0, len(managedKeys))
	keys = append(keys, managedKeys...)
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := env[k]; ok {
			fmt.Fprintf(stdout, "  %s=%s\n", k, v)
			set++
		} else {
			fmt.Fprintf(stdout, "  %s (unset)\n", k)
		}
	}
	if set == len(managedKeys) {
		fmt.Fprintf(stdout, "enabled in %s\n", settingsPath)
	} else {
		fmt.Fprintf(stdout, "not fully enabled in %s (%d of %d set)\n", settingsPath, set, len(managedKeys))
	}
	return 0
}

// isCortexValue reports whether an existing value looks like one we wrote, so a
// port change in the Cortex config updates cleanly instead of tripping the
// overwrite guard.
func isCortexValue(key, val string) bool {
	switch key {
	case envNoTelem:
		return val == "1"
	case envCACerts:
		return strings.Contains(val, ".cortex"+string(os.PathSeparator)) || strings.Contains(val, "cortex-ca")
	case envProxy:
		return strings.Contains(val, "localhost:476") || strings.Contains(val, "127.0.0.1:476")
	}
	return false
}

// readSettings decodes into a generic map so every key the file already has
// survives the round trip, including ones this version of abctl knows nothing
// about. A missing file is an empty document, not an error.
func readSettings(path string) (map[string]any, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%w); fix or move it before enabling", path, err)
	}
	return doc, nil
}

func envBlock(doc map[string]any) map[string]string {
	out := map[string]string{}
	raw, ok := doc["env"].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// writeSettings backs the file up, then replaces it atomically. Claude Code
// watches this file and reloads it, so a half-written file would be read.
func writeSettings(path string, doc map[string]any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if cur, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // operator-supplied path
		if werr := os.WriteFile(path+".bak", cur, 0o600); werr != nil {
			return fmt.Errorf("writing backup %s.bak: %w", path, werr)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600: this file commonly holds API tokens in the same env block.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// confirm reads a yes/no from the terminal.
//
// It opens /dev/tty rather than reading stdin because the documented entry point
// is `curl ... | sh`: there stdin is the script itself, so reading it would
// consume the script or hit EOF and silently decline. When there is no
// controlling terminal — CI, a container, a non-interactive shell — it says so
// and declines, which callers treat as "skipped" rather than failed.
func confirm(stdout io.Writer) bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		fmt.Fprintln(stdout, "Not a terminal, so not prompting. Re-run with --yes to apply.")
		return false
	}
	defer tty.Close()
	return confirmFrom(tty, stdout)
}

// confirmFrom is the answer-parsing half, split out so it is testable: a test
// process has no controlling terminal to open, so confirm itself cannot be
// exercised directly.
//
// Anything that is not an explicit yes declines, EOF included. The prompt says
// [y/N] and the destructive direction here is writing to a file that holds API
// tokens, so silence must mean no.
func confirmFrom(r io.Reader, stdout io.Writer) bool {
	fmt.Fprint(stdout, "Apply? [y/N] ")
	var answer string
	if _, err := fmt.Fscanln(r, &answer); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}
