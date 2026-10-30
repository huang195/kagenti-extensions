package ledger

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The two tests in this file pin the ledger's agreement with the ring (issue #1566 items
// 1 and 2). The ring serves the duration windows; the ledger serves today/7d/month. The
// same traffic must count the same way on both, or the two surfaces an operator reads
// side by side disagree about coverage and about whether the proxy is failing.

// TestRecord_ZeroUsageSuccessIsPriceable is the coverage-gap case. A truncated
// OpenAI-dialect stream answers 200 with a model and no usage block — every counter
// lives on the final chunk that never arrived. inference-parser publishes no cost
// record for it, so the ledger's old rule ("model and a non-zero token count") left it
// out of PriceableRequests while the ring counted it: nine priced requests plus one of
// these read "9/9 priced" on today and "9/10, and here is the gap" on the 1h window.
func TestRecord_ZeroUsageSuccessIsPriceable(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	for i := 0; i < 9; i++ {
		w.Record("s1", costedEvent(t, "api.example", "m", 0.01, 100, 100))
	}
	// The truncated stream: recognised, successful, and carrying no usage at all.
	w.Record("s1", &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "api.example",
		Inference: &pipeline.InferenceExtension{Model: "m"},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var priceable int64
	for _, r := range readAllRows(t, dir) {
		priceable += r.PriceableRequests
	}
	if priceable != 10 {
		t.Errorf("PriceableRequests = %d, want 10: the ring's window=1h counts a successful "+
			"response with a model and no usage as priceable and names it in UnpricedBy. A "+
			"denominator of 9 here reads \"9/9 priced\" on today against \"9/10\" on 1h — full "+
			"coverage over a gap the other surface reports", priceable)
	}
}

// TestRecord_DestinationPrivateIsNotAnError is the error-rule case. The egress guard's
// 403 destination_private refusal is the guard doing what the operator configured, and
// the ring's countsAsError excludes it — with a parsed model, the ledger still writes
// the row (the request happened), so its error count must follow the same rule or the
// guard's own refusals read as a failing proxy on today but not on 1h.
func TestRecord_DestinationPrivateIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	w.Record("s1", &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 403, Host: "private.example",
		Inference: &pipeline.InferenceExtension{Model: "m"},
		Error:     &pipeline.EventError{Kind: pipeline.ErrorKindDestinationPrivate},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("ledger holds %d rows, want 1", len(rows))
	}
	if rows[0].Errors != 0 {
		t.Errorf("Errors = %d, want 0: the ring excludes ErrorKindDestinationPrivate from "+
			"its error count — the guard refusing a private destination is not a failing "+
			"proxy. Counting it here made today report an error 1h did not", rows[0].Errors)
	}
}
