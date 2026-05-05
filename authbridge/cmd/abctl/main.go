// Command abctl is an interactive terminal UI for inspecting AuthBridge's
// in-memory session store. It connects to the session API exposed by a
// sidecar (default http://localhost:9094, reached via kubectl port-forward)
// and renders three panes: active sessions, per-session event stream, and
// event JSON detail. See README.md for usage.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "abctl: TUI not yet implemented")
	os.Exit(1)
}
