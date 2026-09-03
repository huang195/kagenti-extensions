// Command abctl is an interactive terminal UI for inspecting AuthBridge's
// in-memory session store.
//
// Default mode opens a Namespaces → Pods picker, port-forwards the
// selected pod, and renders the session-events view. Pass --endpoint
// to skip the picker and connect directly (the pre-picker behavior).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/cluster"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/edit"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// version is the abctl build version, overridden at release time via
// -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

func main() {
	// Subcommand dispatch happens before flag.Parse: a non-flag first
	// argument selects a subcommand, and anything else falls through to the
	// terminal UI, preserving the original flags-only invocation.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "tools":
			os.Exit(runTools(os.Args[2:], os.Stdout, os.Stderr))
		case "claude-code":
			os.Exit(runClaudeCode(os.Args[2:], os.Stdout, os.Stderr))
		default:
			fmt.Fprintf(os.Stderr, "abctl: unknown subcommand %q (known: tools, claude-code)\n", os.Args[1])
			os.Exit(2)
		}
	}

	endpoint := flag.String("endpoint", "",
		"AuthBridge session API URL (e.g. http://localhost:9094). When omitted, abctl opens a Namespaces → Pods picker.")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("abctl", version)
		return
	}

	// Best-effort sweep of edit-tempfiles older than 24h. Tempfiles are
	// intentionally left in place on every exit path (success / abort /
	// crash) so a user can recover an in-progress edit; the sweep keeps
	// $TMPDIR bounded for users who edit often.
	_ = edit.SweepStaleTempfiles()

	// Friendly check: if picker mode and no kubectl, fail fast with a
	// clear message instead of a stack trace later.
	if *endpoint == "" {
		if _, err := exec.LookPath("kubectl"); err != nil {
			fmt.Fprintln(os.Stderr, "abctl: kubectl not found on PATH; install it or pass --endpoint http://...")
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()

	opts := tui.RunOptions{Endpoint: *endpoint}
	if *endpoint == "" {
		opts.Lister = cluster.NewLister()
		opts.PortForwarder = cluster.NewPortForwarder()
	}
	if err := tui.Run(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "abctl: %v\n", err)
		os.Exit(1)
	}
}
