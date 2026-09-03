package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/toolscan"
)

const toolsUsage = `abctl tools scan — derive a tool-prune remove list from local transcripts

Usage:
  abctl tools scan [--days N] [--keep Name,Name] [--dir PATH] [--write CONFIG]

Flags:
  --days N        window in days to consider a tool "used" (default 30)
  --keep LIST     comma-separated tool names to withhold from the candidate list
  --dir PATH      transcript directory (default ~/.claude/projects)
  --write CONFIG  patch the remove: list of the tool-prune entry in CONFIG in
                  place; without it, the YAML block is printed for you to paste

Transcripts record tools that were called, never tools that were offered, so a
name abctl does not recognise is never proposed for removal.
`

// runTools handles the `tools` subcommand. Returns the process exit code.
func runTools(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "scan" {
		fmt.Fprint(stderr, toolsUsage)
		return 2
	}

	fs := flag.NewFlagSet("tools scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	days := fs.Int("days", 30, "window in days")
	keep := fs.String("keep", "", "comma-separated tool names to keep")
	dir := fs.String("dir", "", "transcript directory (default ~/.claude/projects)")
	write := fs.String("write", "", "patch the tool-prune remove: list in this config file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *days <= 0 {
		fmt.Fprintln(stderr, "abctl: --days must be positive")
		return 2
	}

	scanDir := *dir
	if scanDir == "" {
		d, err := toolscan.DefaultProjectsDir()
		if err != nil {
			fmt.Fprintf(stderr, "abctl: locating transcripts: %v\n", err)
			return 1
		}
		scanDir = d
	}

	res, err := toolscan.Scan(scanDir, *days, strings.Split(*keep, ","))
	if err != nil {
		fmt.Fprintf(stderr, "abctl: scanning %s: %v\n", scanDir, err)
		return 1
	}
	if res.Files == 0 {
		fmt.Fprintf(stderr, "abctl: no transcripts found under %s — nothing to infer from\n", scanDir)
		return 1
	}

	fmt.Fprint(stdout, res.Summary(*days))
	if *write == "" {
		fmt.Fprintln(stdout)
		fmt.Fprint(stdout, res.YAMLBlock())
		return 0
	}

	// Refuse to write a list inferred from no evidence. With zero observed tool
	// calls, "tools you have not called" degrades to "every tool I know about",
	// and the proposal above is that list — including ones a session genuinely
	// needs. This is not a rare edge: it is exactly a new install, where there is
	// little or no transcript history yet, which is also when someone is most
	// likely to accept the default. Printing the proposal is still useful; the
	// refusal is only about writing it unattended.
	if len(res.Called) == 0 {
		fmt.Fprintln(stdout)
		fmt.Fprint(stdout, res.YAMLBlock())
		fmt.Fprintf(stderr, "\nabctl: not writing %s — the scan observed no tool calls at all in the\n"+
			"last %d day(s), so it has no evidence for what you do not use. Use Claude Code\n"+
			"for a while and re-run, widen the window with --days, or paste the block above\n"+
			"yourself once you have checked it.\n", *write, *days)
		return 1
	}

	changed, err := toolscan.PatchConfig(*write, res.Candidates)
	if err != nil {
		fmt.Fprintf(stderr, "abctl: %v\n", err)
		return 1
	}
	if changed {
		fmt.Fprintf(stdout, "\nUpdated remove: list in %s (%d tool(s)).\n", *write, len(res.Candidates))
		fmt.Fprintln(stdout, "The config is hot-reloaded; no restart needed.")
	} else {
		fmt.Fprintf(stdout, "\n%s already up to date.\n", *write)
	}
	return 0
}
