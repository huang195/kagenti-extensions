package pipeline

import "testing"

// TestOutcomeFromContext_ALateRefusalIsNotADenial covers the pipeline half of a defect found in
// ext_proc's teardown flush: a plugin refusing a response that has already gone downstream.
//
// The refusal is real and stays on the record — a gate that would have blocked a response after it
// shipped is exactly what a rollout wants to see — but it cannot be the request's OUTCOME, because
// the request was answered. Reported as a denial, it tells every Finisher, audit row and dashboard
// that a 200 was blocked.
//
// BOTH DENY PATHS ARE COVERED, because they are two answers to one question and either one alone
// leaves the other lying: the rejecting-plugin state that RunResponse sets, and the deny Invocation
// that lastDenyingPlugin walks. The subtests below drive them separately.
func TestOutcomeFromContext_ALateRefusalIsNotADenial(t *testing.T) {
	t.Run("a recorded deny invocation", func(t *testing.T) {
		pctx := &Context{StatusCode: 200}
		pctx.SetCurrentPlugin("gate", InvocationPhaseResponse)
		pctx.MarkResponseDelivered()
		pctx.Record(Invocation{Action: ActionDeny, Reason: "too late"})

		out := OutcomeFromContext(pctx)
		if out.FinalAction == OutcomeDeny {
			t.Errorf("FinalAction = %v on a delivered response (status %d): a refusal recorded after delivery is not a denial",
				out.FinalAction, out.StatusCode)
		}
		// MARKED, NOT DROPPED. The distinction only exists if the row survives to carry it.
		invs := pctx.Extensions.Invocations
		if invs == nil || len(invs.Inbound)+len(invs.Outbound) != 1 {
			t.Fatalf("invocations = %+v, want exactly one", invs)
		}
		got := append(append([]Invocation{}, invs.Inbound...), invs.Outbound...)[0]
		if !got.Late {
			t.Errorf("Late = false on %+v: without the marker nothing downstream can tell a refusal that took effect from one that could not", got)
		}
	})

	t.Run("the rejecting-plugin state", func(t *testing.T) {
		pctx := &Context{StatusCode: 200}
		pctx.MarkResponseDelivered()
		pctx.setRejectingPlugin("gate")

		if out := OutcomeFromContext(pctx); out.FinalAction == OutcomeDeny {
			t.Errorf("FinalAction = %v via the rejecting-plugin branch: the two deny paths have to agree, or the same request has two outcomes",
				out.FinalAction)
		}
		// The evidence is kept: what changed is what it MEANS, not whether it happened.
		if pctx.RejectingPlugin() != "gate" {
			t.Errorf("RejectingPlugin = %q, want \"gate\"", pctx.RejectingPlugin())
		}
	})

	t.Run("the control: before delivery it IS a denial", func(t *testing.T) {
		pctx := &Context{StatusCode: 403}
		pctx.SetCurrentPlugin("gate", InvocationPhaseResponse)
		pctx.Record(Invocation{Action: ActionDeny, Reason: "blocked"})
		pctx.setRejectingPlugin("gate")

		out := OutcomeFromContext(pctx)
		if out.FinalAction != OutcomeDeny || out.DenyingPlugin != "gate" {
			t.Errorf("outcome = %+v, want a denial by gate: an ordinary refusal must still read as one, or this fix has broken enforcement reporting",
				out)
		}
	})
}
