package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// costFetchTimeout bounds the one request this command makes. Longer than the
// TUI's 5s poll because there is no next poll to recover on: a slow answer beats
// telling a user their proxy is down when it is merely busy.
const costFetchTimeout = 15 * time.Second

// runCost answers "what did today cost" in a few lines.
//
// It exists because the figure is otherwise only reachable through the TUI, and
// the question is asked from a shell — at the end of a session, or from a script
// that wants the number without a terminal. Same shape as runTools / runPricing:
// its own FlagSet writing to stderr, an exit code returned rather than os.Exit,
// so main owns process exit and a test can call this directly.
func runCost(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("abctl cost", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false,
		"emit the totals, their provenance and the coverage gaps as JSON, with usage.Counts' own field names")
	window := fs.String("window", usage.WindowToday,
		"window to report: today, 7d, or a duration such as 1h or 6h")
	endpoint := fs.String("endpoint", "",
		"session API URL of the proxy (default: the Cortex installed on this machine)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `abctl cost — what your agents have spent

Usage:
  abctl cost                     today's spend, from local midnight
  abctl cost --window 7d         the last seven days
  abctl cost --window 1h         a rolling hour, from the in-memory ring
  abctl cost --json              the totals as JSON, for a script
  abctl cost --endpoint URL      ask a specific proxy rather than the local one

"today" and "7d" are served from Cortex's durable cost ledger, which is on for a
local install and off in Kubernetes. Where it is off, the proxy answers with the
longest window it does hold and this command prints THAT window, never the one you
asked for — a six-hour figure labelled "today" would be a wrong number wearing a
right label.

A duration window (1h, 6h) comes from a different place than "today" and "7d", and
the two can disagree slightly about the same traffic: the in-memory ring prices a
request nothing else priced, from the rate table, while the ledger reports it as
unpriced instead. Where a request arrives with a settled cost — which is every
inference response on a normal pipeline — they agree. Do not subtract one from the
other and call the difference spend.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// --help is a successful request for help, not a usage error. fs.Parse has
		// already written the usage block to stderr.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	target := *endpoint
	if target == "" {
		target = localSessionEndpoint()
	}
	if target == "" {
		fmt.Fprintln(stderr, "abctl cost: no --endpoint given and no local Cortex is configured")
		fmt.Fprintln(stderr, "  is Cortex installed and running? `abctl service status`")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), costFetchTimeout)
	defer cancel()
	// resolution 0 omits the parameter: a symbolic window is served as one bucket and
	// the server's default is right for every other case. group none — this command
	// reports one total, and a breakdown belongs in the TUI's Cost pane where there
	// is room for a table.
	snap, err := apiclient.New(target).GetUsageWindow(ctx, *window, 0, "", usage.GroupNone)
	if err != nil {
		fmt.Fprintf(stderr, "abctl cost: %v\n", err)
		switch {
		case errors.Is(err, apiclient.ErrNotFound):
			// A reachable proxy with no aggregator. Different problem, different fix.
			fmt.Fprintln(stderr, "  this proxy has no usage aggregation — is session tracking enabled?")
			fmt.Fprintln(stderr, "  is Cortex running? `abctl service status`")
		case errors.Is(err, apiclient.ErrBadRequest):
			// The proxy answered. It understood the request and refused it, so telling a
			// user to go and check whether Cortex is running sends them to the one place
			// that has nothing wrong with it. An older proxy predating window=today, or a
			// --window this one does not accept, are the two real causes.
			fmt.Fprintf(stderr, "  this proxy does not accept --window %q\n", *window)
			fmt.Fprintln(stderr, "  it may predate the today/7d windows; try --window 1h, or a duration it does hold")
		default:
			// A user whose proxy is down needs the next command, not a bare dial error.
			fmt.Fprintln(stderr, "  is Cortex running? `abctl service status`")
		}
		return 1
	}

	if *asJSON {
		return writeCostJSON(snap, stdout, stderr)
	}
	writeCostSummary(snap, stdout)
	return 0
}

// costJSON is the --json shape: the window actually served plus the totals
// verbatim, and the two maps that say where the totals came from and what they miss.
//
// Totals is usage.Counts embedded, NOT re-keyed and NOT re-cased. An unattended
// workload parses this, and the whole point of the shared schema is that the CLI,
// /v1/usage and the ledger on disk say the same words for the same quantity. A
// friendlier spelling here would be a fourth vocabulary for the same numbers.
//
// PricedBy and UnpricedBy are INCLUDED rather than the self-description being
// corrected, and the choice is deliberate. Both readings were available: drop the
// "verbatim" claim and admit this is a subset, or make the claim true. The claim is worth
// making true, because it is the human path's own reasoning applied to the machine path —
// usage_render.go's comment says "$12.40 assembled from a gateway's own numbers and
// $12.40 modelled from a shipped vendor-list table are not equally trustworthy figures",
// and the TUI, the Cost pane and the human summary all label the difference. A scripted
// consumer, the one nobody eyeballs, was the only reader that could not tell a modelled
// total from a billed one, and could not name a coverage gap it was told the size of.
// Both are omitempty on the wire, so a response that carries neither is byte-identical to
// what this printed before.
type costJSON struct {
	// Window is what the SERVER served, so a script reading this learns it got six
	// hours rather than a day without having to ask a second question.
	Window string `json:"window"`
	// Priced false means no figure at all, and a consumer must not read CostMicros'
	// absence as zero spend.
	Priced bool         `json:"priced"`
	Totals usage.Counts `json:"totals"`
	// PricedBy counts the priced requests by the provenance of their figure —
	// "authoritative" when the gateway reported it, otherwise the rate table's level. It
	// is what makes CostMicros interpretable rather than merely readable.
	PricedBy map[string]int64 `json:"pricedBy,omitempty"`
	// UnpricedBy names the coverage gaps, keyed "<endpoint> <model>". Totals already
	// says how many requests went unpriced; this says which pricing entry would close
	// them, which is the only form of that fact a script can act on.
	//
	// Absent is not a claim that there were no gaps: it is never present on a
	// ledger-backed window at all, where a per-minute row cannot distinguish the
	// unpriced pairs from the priced ones. Compare Totals.PricedRequests with
	// Totals.PriceableRequests for that, exactly as the human summary does.
	UnpricedBy map[string]int64 `json:"unpricedBy,omitempty"`
}

func writeCostJSON(snap *usage.Snapshot, stdout, stderr io.Writer) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	out := costJSON{
		Window:     snap.Window,
		Priced:     snap.Priced,
		Totals:     snap.Totals,
		PricedBy:   snap.PricedBy,
		UnpricedBy: snap.UnpricedBy,
	}
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(stderr, "abctl cost: writing JSON: %v\n", err)
		return 1
	}
	return 0
}

// Token-kind bits, matching pipeline.InferenceExtension.PresentKinds and
// parsercommon.Kind. Declared here rather than imported because abctl decodes a
// wire shape; the bit layout is what the JSON contract pins.
const (
	kindInput uint8 = 1 << iota
	kindCacheRead
	kindCacheWrite
	kindOutput
	kindReasoning
)

// writeCostSummary renders the human answer: a headline, a split, and only the
// caveats that actually apply.
//
// Three rules it must obey, all established elsewhere on this branch:
//
//   - Print the window the SERVER reported, never the one requested. A "today"
//     label over six hours of data is the one output that is worse than no output.
//   - "cost unavailable" rather than $0.00 when nothing was priced. A zero cost and
//     an unknown cost are different answers, and only one means the traffic was free.
//   - No caveat line when its number is zero. A permanent warning with nothing to
//     act on is what teaches an operator to ignore the one signal that matters.
func writeCostSummary(snap *usage.Snapshot, stdout io.Writer) {
	t := snap.Totals
	fmt.Fprintf(stdout, "COST — %s\n", costWindowLabel(snap.Window))

	headline := "cost unavailable"
	if snap.Priced {
		headline = costUSD(float64(t.CostMicros) / 1e6)
	}
	fmt.Fprintf(stdout, "  %-14s %s requests   %s tokens\n",
		headline, plainCount(t.Requests), compactTokens(t.Tokens))

	if split := tokenSplit(t); split != "" {
		fmt.Fprintf(stdout, "  %s\n", split)
	}

	// The exactness caveat before the coverage one: it qualifies the dollar figure
	// itself, where coverage qualifies how much of the traffic the figure covers.
	// Both can be true at once and they are different claims.
	if t.IncompleteRequests > 0 {
		fmt.Fprintf(stdout, "  ! %s of %s priced requests carry an inexact figure — the total is not exact\n",
			plainCount(t.IncompleteRequests), plainCount(t.PricedRequests))
	}
	if gap := t.PriceableRequests - t.PricedRequests; gap > 0 {
		fmt.Fprintf(stdout, "  ! %s of %s priceable requests unpriced — the total covers only the priced ones\n",
			plainCount(gap), plainCount(t.PriceableRequests))
	}
	if !snap.Priced && t.PriceableRequests == 0 {
		// Not a gap and not a failure: there was no inference traffic to price. Said out
		// loud so an empty answer reads as a finding rather than as a broken command.
		fmt.Fprintln(stdout, "  no priceable traffic in this window")
	}
}

// costWindowLabel tidies a duration window for reading and passes anything else
// through untouched.
//
// time.Duration.String() emits every unit, so the aggregator's own Window field
// reads "6h0m0s" where "6h" would do. A symbolic window ("today", "7d") is not a
// duration at all and must survive verbatim — it is the server's own word for what
// it served, and rewriting it is how a label stops matching its data.
func costWindowLabel(window string) string {
	d, err := time.ParseDuration(window)
	if err != nil || d <= 0 {
		return window
	}
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return d.String()
	}
}

// costUSD formats a dollar figure for a headline.
//
// Two decimals, because this is a session or a day total and cents are the unit a
// human reasons in. A real charge below half a cent renders as "<$0.01" rather than
// "$0.00": the floor exists so a small non-zero figure is never printed as the one
// string this command is forbidden to print for an unknown cost.
func costUSD(v float64) string {
	if v > 0 && v < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

// compactTokens renders a token count the way a headline has room for: 218.1M, not
// 218100000.
//
// Zero renders "0", which is honest here in a way "$0.00" is not: a token count of
// zero is measured, not unknown — the counters are present whether or not any rate
// covered them.
func compactTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return trimZero(float64(n)/1e9) + "B"
	case n >= 1_000_000:
		return trimZero(float64(n)/1e6) + "M"
	case n >= 1_000:
		return trimZero(float64(n)/1e3) + "k"
	default:
		return fmt.Sprintf("%d", n)
	}
}

// trimZero renders one decimal place, dropping a trailing ".0" so "215M" does not
// read as "215.0M".
func trimZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}

// plainCount renders a count in full. Request counts are small enough to read
// exactly, and rounding "318" to "0.3k" would lose the number the coverage line is
// about.
func plainCount(n int64) string { return fmt.Sprintf("%d", n) }

// tokenSplit renders the four billed kinds plus reasoning, omitting any kind
// nothing reported.
//
// Omitted rather than printed as zero, because a zero has two readings — "this
// traffic wrote no cache" and "nothing here reports cache writes" — and only
// PresentKinds distinguishes them. Printing "cache-write 0" for a provider that
// does not expose the counter asserts the first when the truth is the second.
//
// A non-zero value with its bit unset still prints: that is an event from a
// producer predating PresentKinds, where the value is the only evidence available
// and dropping it would hide a real number.
//
// Returns "" when nothing qualifies, and the caller omits the line entirely.
func tokenSplit(t usage.Counts) string {
	var parts []string
	add := func(bit uint8, label string, v int64) {
		if t.PresentKinds&bit == 0 && v == 0 {
			return
		}
		parts = append(parts, label+" "+compactTokens(v))
	}
	add(kindInput, "input", t.InputTokens)
	add(kindCacheRead, "cache-read", t.CacheReadTokens)
	add(kindCacheWrite, "cache-write", t.CacheWriteTokens)
	add(kindOutput, "output", t.OutputTokens)
	// Reasoning is a SUBSET of output, not a sibling: the provider reports how much
	// of what it generated was reasoning. Labelled so nobody adds the two.
	add(kindReasoning, "reasoning (of output)", t.ReasoningTokens)
	return strings.Join(parts, " · ")
}
