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
// telling a user their proxy is down when it is merely busy — and the slow answer is
// a real case rather than a hypothetical one, because a symbolic window is served by
// reading day files off disk instead of a ring out of memory.
//
// It is also the bound the request ACTUALLY gets, which it was not. apiclient carried
// a fixed 10s http.Client.Timeout applying to every call it made; that and a context
// deadline are both hard stops and the shorter one wins, so every fetch died at 10s
// and the paragraph above described behaviour that could not happen. The client now
// sets no timeout of its own and supplies a default only for a caller that passed no
// deadline (apiclient.restDefaultTimeout), so this figure is what binds. Do not
// lengthen it much further: a CLI that appears to hang is its own kind of wrong answer.
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
		"emit the totals, their provenance, the coverage gaps and any incomplete-read "+
			"disclosure as JSON, with usage.Counts' own field names")
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
// verbatim, the two maps that say where the totals came from and what they miss, and the
// ledger's own admission when the read that produced them was incomplete.
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
//
// Degraded is on this struct for exactly that argument taken one step further: it is the
// only field here that says the totals are INCOMPLETE rather than merely qualified, and a
// script summing CostMicros across days had no way to know one of them was short. The
// server logs a warning for it, which is a line no scripted consumer can read.
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
	// Degraded says the totals above are MISSING ROWS — a day file that lost lines, or one
	// whose scan was abandoned part-way — so CostMicros is short by an amount nothing in
	// this document can state. See costDegradedText for the claim in full and for why it is
	// not Totals.IncompleteRequests.
	//
	// Here because the log line the server writes reaches nobody on this path. The human
	// summary can be read by the person who ran it; a scripted consumer is the one nobody
	// eyeballs, and it was the only reader that could not tell a damaged read from a clean
	// one. That is the same argument PricedBy and UnpricedBy are on this struct for.
	//
	// VERBATIM as *usage.Degraded, not flattened into two fields of our own: the schema rule
	// is one vocabulary from parser to aggregate to ledger to CLI, and "skippedLines" here
	// has to be the "skippedLines" on the wire. omitempty on a POINTER, so a clean read
	// serialises nothing and a response carrying no disclosure is byte-identical to what
	// this printed before — absence keeps meaning "the read was clean" rather than becoming
	// zeros a consumer has to interpret.
	Degraded *usage.Degraded `json:"degraded,omitempty"`
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
		Degraded:   snap.Degraded,
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

	// A NEGATIVE total is not a total, and gets the headline an unpriced window gets. The
	// session API refuses to publish one — cost is a sum of per-request figures that are
	// themselves non-negative — so THE GUARANTEE IS UPSTREAM and this is DEFENCE IN DEPTH:
	// a refusal to print a figure that contradicts a promise made on the other side of the
	// wire. The TUI's Cost pane, spend strip, sessions COST cell and Usage pane cell all
	// make the same refusal (see tui.negativeCost, which is its one spelling there); this
	// surface printed "$-5.00", which reads as a refund nobody issued. Unavailable rather
	// than clamped to zero, because $0.00 would assert the traffic was free.
	negative := snap.Priced && t.CostMicros < 0
	headline := "cost unavailable"
	if snap.Priced && !negative {
		headline = costUSD(float64(t.CostMicros) / 1e6)
	}
	fmt.Fprintf(stdout, "  %-14s %s requests   %s tokens\n",
		headline, plainCount(t.Requests), compactTokens(t.Tokens))
	if negative {
		// Said out loud, because "cost unavailable" on its own points a reader at pricing
		// coverage — the ordinary cause — when the real cause is a producer publishing an
		// impossible figure. Different problem, different fix.
		fmt.Fprintln(stdout,
			"  ! the server reported a negative total, which cannot be spend — no figure is shown")
	}

	if split := tokenSplit(t); split != "" {
		fmt.Fprintf(stdout, "  %s\n", split)
	}

	// The damage disclosure leads, ahead of both, because it is the only one of the three
	// that says the SUM ITSELF is incomplete. The two below qualify a figure this answer
	// carries; this one says spend is missing from it. usage.Snapshot.Degraded's own doc
	// forbids merging the two claims or showing them under one marker, so they are three
	// separate lines with three sets of words and no shared prefix beyond the "!".
	//
	// This is the DEFAULT path, not an edge of one: --window defaults to today, and today
	// (with 7d) is the only window the durable cost ledger serves, which is the only place
	// the field can be populated at all.
	if snap.Degraded != nil {
		fmt.Fprintf(stdout, "  ! %s\n", costDegradedText(snap.Degraded))
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

// costDegradedText says what a damaged ledger read could not deliver.
//
// usage.Snapshot.Degraded means the answer is known to be MISSING ROWS: a day file that
// lost lines, or one whose scan was abandoned part-way. The server populates it and logs a
// warning; until now nothing in abctl read it, so a damaged read printed a total
// byte-identical to a clean one — a short figure with no caveat anywhere in it, which is
// precisely what the field exists to prevent.
//
// A DIFFERENT CLAIM from Totals.IncompleteRequests and worded so it cannot be mistaken for
// it. That counter says a figure this answer CARRIES is inexact — a floor, or an
// approximation — and the request is still counted and still priced; this says rows are
// missing from the sum entirely, so the shortfall is not merely unmeasured but unstatable.
// The two are never merged and never share a form of words.
//
// PRESENCE is the claim, not the counters. The field is a pointer so a clean read serialises
// nothing, and a present object whose counters are both zero still reports damage: a
// producer that sent the object is saying it found some. Zeros read as "checked, fine" would
// be the same false reassurance as "$0.00" over unpriced traffic.
//
// A truncated day is called out as worse than a skipped line because it is worse by an
// unbounded amount — a line is one request, a file is a whole day.
//
// The TUI states the same fact in its own two verbosities (tui.costDamagedNote for the Cost
// pane, tui.damagedNote plus a one-cell marker for the spend strip). Three spellings for one
// fact is the same arrangement the coverage gap already has, and for the same reason: this
// is a package boundary, and each surface has a different amount of room.
func costDegradedText(d *usage.Degraded) string {
	switch {
	case d.SkippedLines > 0 && d.TruncatedDays > 0:
		return fmt.Sprintf("this total is SHORT — the cost ledger skipped %s unreadable line%s "+
			"and abandoned %s day file%s part-way; that spend happened and is missing from the "+
			"sum, by an amount nothing here can state",
			plainCount(d.SkippedLines), plainPlural(d.SkippedLines),
			plainCount(d.TruncatedDays), plainPlural(d.TruncatedDays))
	case d.SkippedLines > 0:
		return fmt.Sprintf("this total is SHORT — the cost ledger skipped %s unreadable line%s; "+
			"that spend happened and is missing from the sum, by an amount nothing here can state",
			plainCount(d.SkippedLines), plainPlural(d.SkippedLines))
	case d.TruncatedDays > 0:
		return fmt.Sprintf("this total is SHORT — the cost ledger abandoned %s day file%s "+
			"part-way; a file holds a whole day, so the amount missing from the sum is unbounded",
			plainCount(d.TruncatedDays), plainPlural(d.TruncatedDays))
	default:
		return "this total is SHORT — the cost ledger reported an incomplete read without " +
			"saying how much it lost; rows are missing from the sum"
	}
}

// plainPlural is the "s" a count needs, for the one message in this file that has to
// agree with a number it does not control.
func plainPlural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
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
