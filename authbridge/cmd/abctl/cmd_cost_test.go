package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
