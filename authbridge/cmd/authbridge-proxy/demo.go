package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

// Everything Cortex writes for a user lives under ~/.cortex, so a laptop ends up
// with exactly one directory holding config, CA and keys rather than a CA
// scattered into whichever directory each command happened to run from.
const (
	cortexDirName = ".cortex"
	// demoDirName keeps --demo's throwaway state out of the persistent config's
	// way. --demo regenerates its config, so it must not share a directory with
	// the config a user maintains by hand.
	demoDirName = "demo"
	// demoCADirFallback is used only when the home directory cannot be
	// determined, which is the historical cwd-relative behaviour.
	demoCADirFallback = "cortex-ca"
)

// defaultDemoCADir returns the directory --demo works in: ~/.cortex/demo, or
// ./cortex-ca if there is no resolvable home directory.
//
// This used to be cwd-relative unconditionally, on the reasoning that no
// absolute path should be baked into the binary. Resolving $HOME at runtime
// satisfies that while keeping the private key in one predictable place — and
// the cwd default had a real cost: it dropped a CA and private key into
// whatever directory the demo was started from, including checkouts.
func defaultDemoCADir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return demoCADirFallback
	}
	return filepath.Join(home, cortexDirName, demoDirName)
}

// demoConfigYAML returns the built-in --demo config with caDir interpolated: a
// forward-only proxy with the TLS bridge on (auto-generated CA in caDir) and
// the LLM / MCP / A2A parsers, so an agent's egress is decrypted and parsed.
// Kept in sync with the root README.
//
// Every listener the demo uses is pinned to loopback on an uncommon port. This
// runs on a laptop, so (a) a wildcard bind would expose an open forward proxy,
// the stats endpoint, and the unauthenticated session API (which carries
// decrypted bodies and any injected tokens) to the LAN, and (b) the usual
// 8081/909x ports collide with common dev tools. The preset only fills empty
// addresses, so these explicit values win — keep them in sync with the ports
// the installer probes and prints (authbridge/install-demo.sh). The
// enforce-redirect transparent listener isn't used here (no iptables) and
// main.go skips starting it under --demo.
//
// The YAML body is flush-left on purpose — a raw string literal preserves
// leading whitespace, so indenting these lines in source would corrupt the YAML.
func demoConfigYAML(caDir string) string {
	return `# Built-in config for: authbridge-proxy --demo
# Forward-only proxy + TLS bridge (auto-generated CA) + LLM/MCP/A2A parsers.
# The running proxy watches this file — edit it to hot-reload.
mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
stats:
  address: 127.0.0.1:47602
tls_bridge:
  mode: enabled
  ca_dir: "` + caDir + `"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: mcp-parser
      - name: a2a-parser
      # tool-prune drops unused tool definitions from the outbound manifest.
      # The empty remove list is the off switch: with nothing named it does
      # nothing at all. Fill it in and it takes effect immediately --
      #   abctl tools scan --write <this file>
      # -- and the config is hot-reloaded, so no restart.
      #
      # Watch the Metrics section of abctl's plugin pane for what it saved. If
      # you ever suspect the plugin of breaking a request, set
      # on_error: observe here: it then counts what it *would* remove while
      # leaving every byte on the wire untouched, which settles the question
      # without unconfiguring anything.
      #
      # Keep it last: it rewrites the request body, and body readers must
      # precede the mutator so they see the original bytes.
      - name: tool-prune
        on_error: enforce
        config:
          remove: []
`
}

// writeDemoConfig ensures the built-in --demo config exists next to the CA (in
// caDir) and returns its path, so --demo reuses the normal file-based load +
// hot-reload path. caDir is caller-resolved (cwd-relative by default, or
// --ca-dir); no absolute path is baked into the binary.
//
// An existing file is KEPT, not overwritten. The config's own header invites
// editing it, and `abctl tools scan --write` writes a prune list into it — and
// this function runs before any port is bound, so an unconditional write meant
// that even a --demo start which then failed on a port clash silently destroyed
// those edits. Delete the file to regenerate the preset.
func writeDemoConfig(caDir string) (string, error) {
	// 0700: this directory holds the demo CA's private key.
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(caDir, "demo.yaml")
	if _, err := os.Stat(path); err == nil {
		slog.Info("demo mode — keeping the existing config (edits and any prune list are preserved)",
			"path", path, "hint", "delete it to regenerate the built-in preset")
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.WriteFile(path, []byte(demoConfigYAML(caDir)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
