package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// fakeUsageServer answers GET /v1/usage with body and 404s everything else, so a
// test states exactly what the proxy reported and nothing else can satisfy the
// command by accident.
//
// A server rather than an injected decoder: the command's job includes reaching the
// endpoint and turning a failure into a recovery hint, and a stubbed transport would
// test everything except that.
func fakeUsageServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
}

func TestRunCost_PrintsAHumanSummary(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"tokens":218100000,"costMicros":4170000,"pricedRequests":306,`+
		`"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"$4.17", "today", "318"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunCost_DisclosesTheCoverageGap(t *testing.T) {
	// 306 of 318 priced. Presenting the dollar total without the gap presents a
	// subtotal as the whole spend — the failure the coverage counters exist for.
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":306,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if !strings.Contains(out.String(), "12") {
		t.Errorf("output does not disclose the 12 unpriced requests:\n%s", out.String())
	}
}

func TestRunCost_FullyPricedCarriesNoWarning(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if strings.Contains(strings.ToLower(out.String()), "unpriced") {
		t.Errorf("fully priced output still warns:\n%s", out.String())
	}
}

// An inexact total must SAY it is inexact. A truncated stream's figure is a floor,
// and printing it beside an exact-looking "$4.17" claims a precision the data does
// not have — the claim usage.Counts.IncompleteRequests exists to withdraw.
func TestRunCost_DisclosesAnInexactTotal(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318,`+
		`"incompleteRequests":4},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if !strings.Contains(got, "inexact") {
		t.Errorf("output does not say the total is inexact:\n%s", got)
	}
	if !strings.Contains(got, "4") {
		t.Errorf("output does not name how many figures are inexact:\n%s", got)
	}
	// Disclosed, not deducted: the dollar total still stands.
	if !strings.Contains(got, "$4.17") {
		t.Errorf("output withheld the figure instead of qualifying it:\n%s", got)
	}
}

// And an exact total must not carry the caveat, for the reason a fully priced one
// carries no coverage warning.
func TestRunCost_ExactTotalCarriesNoInexactWarning(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if strings.Contains(out.String(), "inexact") {
		t.Errorf("an exact total still warns:\n%s", out.String())
	}
}

func TestRunCost_NothingPricedSaysUnavailableNotZero(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,`+
		`"priceableRequests":10},"priced":false}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 — nothing priced is not an error", code)
	}
	got := out.String()
	if !strings.Contains(got, "unavailable") {
		t.Errorf("output does not say cost is unavailable:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("output renders $0.00 for an unknown cost:\n%s", got)
	}
}

// No inference traffic at all is a finding, not an absence. Saying so beats a
// coverage line reading "0 of 0", which looks like a bug.
func TestRunCost_NoPriceableTrafficSaysSo(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":4},"priced":false}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if !strings.Contains(got, "no priceable traffic") {
		t.Errorf("output does not say there was nothing to price:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("output renders $0.00 for an unknown cost:\n%s", got)
	}
}

func TestRunCost_JSONUsesTheCountsFieldNames(t *testing.T) {
	// The schema rule: one vocabulary from parser to aggregate to ledger to CLI.
	// An unattended workload parses this, so the names are the contract.
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"inputTokens":100,"cacheReadTokens":2000,"cacheWriteTokens":50,`+
		`"outputTokens":30,"costMicros":250000,"pricedRequests":2,`+
		`"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	for _, field := range []string{"inputTokens", "cacheReadTokens", "cacheWriteTokens", "outputTokens", "costMicros"} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("--json output missing %q:\n%s", field, out.String())
		}
	}
	// The window the SERVER served is part of the JSON too, so a script learns which
	// span its number covers without asking again.
	if decoded["window"] != "today" {
		t.Errorf("window = %v, want \"today\"", decoded["window"])
	}
}

// costJSON.Window claims to be what the SERVER served, so "a script learns it got six hours
// rather than a day without having to ask a second question". Nothing tested that claim:
// TestRunCost_JSONUsesTheCountsFieldNames asks for "today" against a fixture that also
// answers "today", so it cannot tell reporting the answer from echoing the request —
// hardcoding costJSON{Window: "today"} passed the entire suite. The human path had the
// equivalent check (TestRunCost_NoLedgerServesAShorterWindowAndSaysSo); the machine path did
// not, and the machine path is the one nobody eyeballs.
//
// A proxy with no durable cost ledger — Kubernetes by design — is where this happens.
func TestRunCost_JSONReportsTheWindowServedNotTheOneRequested(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"6h0m0s","totals":{"requests":5,`+
		`"costMicros":100000,"pricedRequests":5,"priceableRequests":5},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--window", "today", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if decoded["window"] != "6h0m0s" {
		t.Errorf("window = %v, want \"6h0m0s\": the span the server served, not the \"today\" that was asked for", decoded["window"])
	}
	// And it must not be the request under any spelling — "today" appearing anywhere in the
	// window field would mean the request leaked into the answer.
	if w, _ := decoded["window"].(string); strings.Contains(w, "today") {
		t.Errorf("window = %q echoes the requested window; a script would report a six-hour figure as a day's spend", w)
	}
}

// costJSON described itself as the totals verbatim and omitted pricedBy and unpricedBy, so
// the one reader that cannot eyeball anything was the one reader that could not tell a
// MODELLED total from a BILLED one — the distinction usage_render.go's comment says
// matters, and which the human summary, the strip and the Cost pane all label.
func TestRunCost_JSONCarriesProvenanceAndTheNamedGaps(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,`+
		`"costMicros":1240000,"pricedRequests":7,"priceableRequests":10},"priced":true,`+
		`"pricedBy":{"authoritative":4,"bundled":3},`+
		`"unpricedBy":{"api.openai.com gpt-5":3}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		PricedBy   map[string]int64 `json:"pricedBy"`
		UnpricedBy map[string]int64 `json:"unpricedBy"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	// A total that is part modelled is not the same figure as one a gateway billed, and
	// $1.24 says nothing about which this is.
	if decoded.PricedBy["bundled"] != 3 || decoded.PricedBy["authoritative"] != 4 {
		t.Errorf("pricedBy = %v, want the provenance split the server reported:\n%s",
			decoded.PricedBy, out.String())
	}
	// "3 requests unpriced" is not actionable; the endpoint and model name the pricing
	// entry that would close the gap.
	if decoded.UnpricedBy["api.openai.com gpt-5"] != 3 {
		t.Errorf("unpricedBy = %v, want the named gap:\n%s", decoded.UnpricedBy, out.String())
	}
}

// Both maps are omitempty, so a response carrying neither prints exactly what it printed
// before — a null or an empty object would make a script that checks for presence read
// "there were no gaps", which is a claim the ledger path in particular cannot make.
func TestRunCost_JSONOmitsTheMapsWhenTheServerSentNone(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"costMicros":250000,"pricedRequests":2,"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, absent := range []string{"pricedBy", "unpricedBy"} {
		if strings.Contains(out.String(), absent) {
			t.Errorf("--json emitted %q for a response that carried none:\n%s", absent, out.String())
		}
	}
}

func TestRunCost_UnreachableProxyExitsNonZeroAndSaysWhatToRun(t *testing.T) {
	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", "http://127.0.0.1:1"}, &out, &errOut)

	if code == 0 {
		t.Fatal("exit = 0 for an unreachable proxy")
	}
	// A user whose proxy is down needs the next command, not a bare dial error.
	if !strings.Contains(errOut.String(), "abctl service status") {
		t.Errorf("stderr does not name the recovery command:\n%s", errOut.String())
	}
}

// A 400 is the proxy ANSWERING: it understood the request and refused it. Telling the
// user to go and check whether Cortex is running sends them to the one place that has
// nothing wrong with it. The real causes are an older proxy that predates today/7d and
// a --window this one does not accept.
func TestRunCost_RefusedWindowBlamesTheWindowNotTheProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(`{"error":"bad window (want a duration such as 10m, 1h or 6h)"}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", srv.URL, "--window", "today"}, &out, &errOut)

	if code == 0 {
		t.Fatal("exit = 0 for a refused window")
	}
	stderr := errOut.String()
	if !strings.Contains(stderr, "does not accept --window") {
		t.Errorf("stderr does not blame the window:\n%s", stderr)
	}
	if strings.Contains(stderr, "abctl service status") {
		t.Errorf("stderr sends the user to check a proxy that just answered:\n%s", stderr)
	}
	// The server's own words reach the user: every 400 message this endpoint returns is
	// a fixed string authored server-side, so it is the most specific thing available.
	if !strings.Contains(stderr, "bad window") {
		t.Errorf("stderr drops the server's own explanation:\n%s", stderr)
	}
}

func TestRunCost_NoLedgerServesAShorterWindowAndSaysSo(t *testing.T) {
	// Kubernetes, or a local install with the ledger disabled. The server answers
	// with the window it actually served; the CLI must print THAT, not "today".
	srv := fakeUsageServer(t, `{"window":"6h0m0s","totals":{"requests":5,`+
		`"costMicros":100000,"pricedRequests":5,"priceableRequests":5},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if strings.Contains(got, "today") {
		t.Errorf("output claims \"today\" when the server served 6h:\n%s", got)
	}
	if !strings.Contains(got, "6h") {
		t.Errorf("output does not name the window actually served:\n%s", got)
	}
}

// The split line is the shape of the bill for a long-running agent: cache reads are
// roughly a tenth of uncached input and cache writes a quarter more, so one scalar
// cannot explain a total.
func TestRunCost_PrintsTheTokenSplitThatWasReported(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"tokens":2180,"inputTokens":100,"cacheReadTokens":2000,"outputTokens":80,`+
		`"presentKinds":11,"costMicros":250000,"pricedRequests":2,`+
		`"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	for _, want := range []string{"input 100", "cache-read 2k", "output 80"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// presentKinds 0b1011 leaves cache-write unset, and its value is 0. Printing
	// "cache-write 0" would assert this traffic wrote no cache when the truth is that
	// nothing reported the counter.
	if strings.Contains(got, "cache-write") {
		t.Errorf("output prints a kind nothing reported:\n%s", got)
	}
}

// A real charge below half a cent must not print as $0.00 — the one string this
// command is forbidden to print for an unknown cost, so it must not be reachable for
// a known small one either.
func TestRunCost_SubCentChargeIsNotRenderedAsZero(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":1,`+
		`"costMicros":1200,"pricedRequests":1,"priceableRequests":1},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if strings.Contains(got, "$0.00") {
		t.Errorf("a sub-cent charge rendered as $0.00:\n%s", got)
	}
	if !strings.Contains(got, "<$0.01") {
		t.Errorf("output does not floor a sub-cent charge:\n%s", got)
	}
}

func TestRunCost_HelpListsTheFlags(t *testing.T) {
	var out, errOut strings.Builder
	if code := runCost([]string{"--help"}, &out, &errOut); code != 0 {
		t.Errorf("exit = %d for --help, want 0", code)
	}
	combined := out.String() + errOut.String()
	for _, want := range []string{"--json", "--window", "--endpoint"} {
		if !strings.Contains(combined, want) {
			t.Errorf("help does not mention %q:\n%s", want, combined)
		}
	}
	// The ring-versus-ledger difference has to be somewhere a user comparing two
	// figures on screen will actually look. It was documented only at the top of a Go
	// source file, which is the one place they will not.
	if !strings.Contains(combined, "can disagree") {
		t.Errorf("help does not warn that a duration window and today/7d can differ:\n%s", combined)
	}
}

// --window is forwarded, not ignored. Without this the default made every test pass
// while `--window 7d` quietly reported today.
func TestRunCost_ForwardsTheRequestedWindow(t *testing.T) {
	var gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWindow = r.URL.Query().Get("window")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"window":"7d","totals":{"requests":1},"priced":false}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL, "--window", "7d"}, &out, &errOut)

	if gotWindow != "7d" {
		t.Errorf("server saw window=%q, want \"7d\"", gotWindow)
	}
}

// The default window is today: the question this command exists to answer.
func TestRunCost_DefaultsToToday(t *testing.T) {
	var gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWindow = r.URL.Query().Get("window")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"window":"today","totals":{"requests":1},"priced":false}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if gotWindow != "today" {
		t.Errorf("server saw window=%q, want \"today\"", gotWindow)
	}
}

// TestRunCost_ANegativeTotalIsNotPrintedAsARefund.
//
// The CLI is a money surface too, and it had the hole the TUI's Cost pane closed in two
// places: costUSD is faithful about the sign, so a negative total printed "$-5.00" in the
// headline. The session API refuses to publish one, so this can only be a broken producer
// — and inheriting a guarantee silently is how it stops holding.
//
// "cost unavailable" plus a line naming the real cause. The headline on its own points a
// reader at pricing coverage, which is the ordinary reason for that string and the wrong
// place to look here.
func TestRunCost_ANegativeTotalIsNotPrintedAsARefund(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":-5000000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	if strings.Contains(got, "$-") {
		t.Errorf("output prints a negative total as an amount:\n%s", got)
	}
	if !strings.Contains(got, "cost unavailable") {
		t.Errorf("output neither showed a figure nor declined one:\n%s", got)
	}
	if !strings.Contains(got, "negative") {
		t.Errorf("output does not name the reason there is no figure:\n%s", got)
	}
}

// TestRunCost_APositiveTotalStillPrints is the mirror. Without it the guard above could be
// satisfied by never printing a figure at all.
func TestRunCost_APositiveTotalStillPrints(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)
	got := out.String()
	if !strings.Contains(got, "$4.17") {
		t.Errorf("output lost a legitimate figure:\n%s", got)
	}
	if strings.Contains(got, "negative") {
		t.Errorf("output carries a caveat with nothing to act on:\n%s", got)
	}
}

// TestRunCost_DisclosesADamagedLedgerRead.
//
// usage.Snapshot.Degraded says the answer is MISSING ROWS. The server populates it and logs
// a warning; nothing in cmd/abctl read it, so a damaged read printed a total byte-identical
// to a clean one — a short figure under priced:true with no caveat anywhere in it, which is
// the failure the field's own doc says it exists to prevent.
//
// The default path: --window defaults to today, and today is one of the two windows the
// durable cost ledger serves, which is the only place the field can be populated.
func TestRunCost_DisclosesADamagedLedgerRead(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true,`+
		`"degraded":{"skippedLines":3,"truncatedDays":1}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	// The figure stays: it is short, not wrong, and withholding it would report a day of
	// known spend as unavailable.
	if !strings.Contains(got, "$4.17") {
		t.Errorf("output withheld a figure that is short rather than unknown:\n%s", got)
	}
	// Both counters, named. "Incomplete" alone gives an operator nothing to act on.
	for _, want := range []string{"SHORT", "3", "1 day file"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q — the damage is not disclosed:\n%s", want, got)
		}
	}
}

// TestRunCost_ADamagedReadIsNotSpelledAsAnInexactOne.
//
// usage.Snapshot.Degraded's doc is explicit that this is a DIFFERENT claim from
// Totals.IncompleteRequests and that the two must not be merged or shown with one marker:
// that counter says a figure the answer CARRIES is inexact, this says rows are missing from
// the sum. Both live in this fixture, and each has to be recognisable on its own.
func TestRunCost_ADamagedReadIsNotSpelledAsAnInexactOne(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":300,"priceableRequests":318,`+
		`"incompleteRequests":7},"priced":true,"degraded":{"skippedLines":3}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)
	got := out.String()

	// Three caveat lines, three claims, one each. Merged into one line — or one dropped —
	// this count is wrong.
	if n := strings.Count(got, "\n  ! "); n != 3 {
		t.Errorf("got %d caveat lines, want 3 (damaged, inexact, coverage):\n%s", n, got)
	}
	// The inexactness line still says what it always said, in its own words.
	if !strings.Contains(got, "inexact figure") {
		t.Errorf("the inexactness caveat lost its own wording:\n%s", got)
	}
	// And the damage line does not borrow them.
	dmg := ""
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "SHORT") {
			dmg = l
		}
	}
	if dmg == "" {
		t.Fatalf("no damage line at all:\n%s", got)
	}
	if strings.Contains(dmg, "inexact") {
		t.Errorf("the damage line is worded as an inexactness caveat: %q", dmg)
	}
	// The damage line leads: it is the only one of the three saying the SUM is incomplete.
	if i, j := strings.Index(got, "SHORT"), strings.Index(got, "inexact figure"); i > j {
		t.Errorf("the damage line follows the inexactness one (%d > %d):\n%s", i, j, got)
	}
}

// TestRunCost_ACleanReadCarriesNoDamageLine is the mirror, and it is the half that keeps the
// disclosure worth reading: a permanent warning with nothing to act on is what teaches an
// operator to ignore the one signal that matters.
func TestRunCost_ACleanReadCarriesNoDamageLine(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)
	if got := out.String(); strings.Contains(got, "SHORT") {
		t.Errorf("a clean read carries a damage caveat:\n%s", got)
	}
}

// TestRunCost_JSONCarriesTheDamageDisclosure.
//
// The machine path is the one this matters most on: a human can read the server's log line
// if they know to look, a script cannot. It was also the reader with no other way to tell a
// short total from a complete one — priced:true and a plausible figure look identical.
//
// Verbatim field names, because the schema rule is one vocabulary from parser to aggregate
// to ledger to CLI.
func TestRunCost_JSONCarriesTheDamageDisclosure(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,`+
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true,`+
		`"degraded":{"skippedLines":3,"truncatedDays":1}}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	var decoded struct {
		Degraded *struct {
			SkippedLines  int64 `json:"skippedLines"`
			TruncatedDays int64 `json:"truncatedDays"`
		} `json:"degraded"`
	}
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if decoded.Degraded == nil {
		t.Fatalf("--json dropped the damage disclosure entirely:\n%s", out.String())
	}
	if decoded.Degraded.SkippedLines != 3 || decoded.Degraded.TruncatedDays != 1 {
		t.Errorf("degraded = %+v, want skippedLines 3 and truncatedDays 1:\n%s",
			*decoded.Degraded, out.String())
	}
}

// TestRunCost_JSONOmitsTheDamageDisclosureWhenTheReadWasClean.
//
// The pointer's whole point: absence means the read was clean, so a clean answer has to be
// byte-identical to what this printed before the field existed. Zeros in an always-present
// object would read as "checked, fine" — the same false reassurance as $0.00 over unpriced
// traffic.
func TestRunCost_JSONOmitsTheDamageDisclosureWhenTheReadWasClean(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,`+
		`"costMicros":250000,"pricedRequests":2,"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out.String(), "degraded") {
		t.Errorf("--json emitted \"degraded\" for a clean read:\n%s", out.String())
	}
}

// TestCostDegradedText_NamesWhatEachKindOfDamageLost.
//
// Each branch on its own, because the three sentences make different claims and the
// unbounded one is the point: a skipped line is one request, an abandoned file is a day.
//
// The zero-counter case is a disclosure too — presence is the claim, not the counters, since
// the field is a pointer so that a clean read serialises nothing.
func TestCostDegradedText_NamesWhatEachKindOfDamageLost(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   usage.Degraded
		want []string
		not  []string
	}{
		{"lines only", usage.Degraded{SkippedLines: 3},
			[]string{"SHORT", "3 unreadable lines"}, []string{"day file", "unbounded"}},
		{"one line", usage.Degraded{SkippedLines: 1},
			[]string{"1 unreadable line;"}, []string{"lines"}},
		{"days only", usage.Degraded{TruncatedDays: 2},
			[]string{"SHORT", "2 day files", "unbounded"}, []string{"unreadable line"}},
		{"both", usage.Degraded{SkippedLines: 3, TruncatedDays: 1},
			[]string{"3 unreadable lines", "1 day file"}, nil},
		{"neither", usage.Degraded{},
			[]string{"SHORT", "without saying how much"}, []string{"unreadable line", "day file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := costDegradedText(&tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q missing %q", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("%q contains %q, which does not apply", got, n)
				}
			}
		})
	}
}
