package pricing

import (
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// IncompleteReason decides whether a modelled figure may be presented as an exact
// total, so the cases below are the boundary of what the system claims to know about
// its own spend. Both directions matter: a missed floor understates the bill silently,
// and a false one attaches a permanent caveat to figures that are exact.
func TestIncompleteReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		inf  *pipeline.InferenceExtension
		want string
	}{{
		// THE BUG. message_start landed, message_delta never did: prompt counted,
		// output zero, no stop reason.
		//
		// PresentKinds here is what a real truncated Anthropic stream carries —
		// Input|CacheRead|Output, with the Output BIT set and its TALLY zero, because
		// toNeutral asserts Input|Output unconditionally and
		// mergeAnthropicPromptMaxSeen ORs the mask without ever assigning Output. The
		// fixture keeps that bit set on purpose: it is what makes this case
		// indistinguishable from a reported zero by mask alone, and any future
		// "simplification" to the bitmask has to fail here.
		name: "truncated Anthropic stream: prompt counted, output bit set but never tallied",
		inf: &pipeline.InferenceExtension{
			InputTokens: 1000, CacheReadTokens: 500,
			PromptTokens: 1500,
			PresentKinds: presentInput | 1<<1 | presentOutput,
		},
		want: ReasonOutputUncounted,
	}, {
		name: "complete stream: output tallied and a stop reason",
		inf: &pipeline.InferenceExtension{
			InputTokens: 1000, OutputTokens: 240,
			PromptTokens: 1000, CompletionTokens: 240,
			PresentKinds: presentInput | presentOutput,
			FinishReason: "end_turn",
		},
		want: "",
	}, {
		// A stop reason is the provider stating the tally finished, so a genuine
		// zero-output turn is exact rather than a floor.
		name: "stop reason with zero output is exact",
		inf: &pipeline.InferenceExtension{
			InputTokens: 1000, PromptTokens: 1000,
			PresentKinds: presentInput | presentOutput,
			FinishReason: "max_tokens",
		},
		want: "",
	}, {
		// The tally is what the figure needs; the reason is only the signal that the
		// tally is final. Having the tally without the reason is still exact.
		name: "output tallied without a stop reason is exact",
		inf: &pipeline.InferenceExtension{
			InputTokens: 1000, OutputTokens: 12,
			PromptTokens: 1000, CompletionTokens: 12,
			PresentKinds: presentInput | presentOutput,
		},
		want: "",
	}, {
		// Legacy aggregates from a provider that reports no split. Output is there.
		name: "legacy completion aggregate alone counts as output",
		inf: &pipeline.InferenceExtension{
			PromptTokens: 900, CompletionTokens: 40,
		},
		want: "",
	}, {
		// A gateway reporting only total_tokens. UsageFromInference attributes that
		// total wholly to uncached input, so the figure is approximate in no known
		// direction — NOT a floor, and a different conversation from a truncated
		// stream. This is the branch where the presence mask IS the right instrument.
		name: "total-only gateway is approximate, not partial",
		inf: &pipeline.InferenceExtension{
			TotalTokens: 1700,
		},
		want: ReasonSplitUnreported,
	}, {
		// Distinguishing the two above is the whole reason for a reason string: this
		// one is a standing property of the gateway, the truncated stream is an
		// incident. One boolean could not tell them apart.
		name: "total-only with a stop reason is still approximate",
		inf: &pipeline.InferenceExtension{
			TotalTokens: 1700, FinishReason: "stop",
		},
		want: ReasonSplitUnreported,
	}, {
		// No counters at all: no figure exists, so there is nothing to qualify.
		// costing publishes nothing for this request. See the settle-time test in
		// authlib/costing for the path that proves it never reaches the flag.
		name: "no counters at all is unpriced, not incomplete",
		inf:  &pipeline.InferenceExtension{Model: "claude-opus-5"},
		want: "",
	}, {
		// A cancelled turn mid-tool-arguments: the model generated tokens the wire
		// never tallied. Genuinely a floor.
		name: "tool call captured but no output tally",
		inf: &pipeline.InferenceExtension{
			InputTokens: 800, PromptTokens: 800,
			ToolCalls: []pipeline.InferenceToolCall{{ID: "t1", Name: "Read"}},
		},
		want: ReasonOutputUncounted,
	}, {
		// A producer that filled the split without the derived aggregate. Read the
		// split rather than trusting Fill to have run.
		name: "cache-write-only prompt, split without aggregate",
		inf: &pipeline.InferenceExtension{
			CacheWriteTokens: 4096,
		},
		want: ReasonOutputUncounted,
	}, {
		name: "nil extension",
		inf:  nil,
		want: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IncompleteReason(tc.inf); got != tc.want {
				t.Errorf("IncompleteReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIncompleteReason_MaskAloneCannotDiscriminate is the tripwire for the
// "simplification" this predicate exists to resist.
//
// It asserts the property directly: on a truncated Anthropic stream the Output presence
// bit is SET while the tally is zero, so `PresentKinds & KindOutput == 0` is FALSE for
// the exact case the bug lives on. Anyone who replaces the stop-reason discriminator
// with the bitmask fails here with a message saying why, rather than shipping a check
// that cannot fire for its own defect.
//
// The asymmetry is dialect-specific and that is the trap: for OpenAI the mask WOULD
// work, because inferenceUsage.toNeutral gates each bit on a pointer. Checking the mask
// against the OpenAI path finds it correct and invites the conclusion that it
// generalizes — and Anthropic, which is what Claude Code speaks, is where it does not.
func TestIncompleteReason_MaskAloneCannotDiscriminate(t *testing.T) {
	// Anthropic, truncated: mask says output was reported, the tally says zero.
	truncated := &pipeline.InferenceExtension{
		InputTokens: 1000, PromptTokens: 1000,
		PresentKinds: presentInput | presentOutput,
	}
	if truncated.PresentKinds&presentOutput == 0 {
		t.Fatal("fixture does not carry the Output bit; it no longer represents a truncated Anthropic stream and the rest of this test proves nothing")
	}
	if got := IncompleteReason(truncated); got != ReasonOutputUncounted {
		t.Errorf("IncompleteReason = %q, want %q: the Output BIT is set here while the TALLY is zero, so a bitmask check cannot see this case — the stop reason is what discriminates", got, ReasonOutputUncounted)
	}

	// A genuine reported zero on the same dialect: bit-identical to the above, and the
	// stop reason is the ONLY thing separating them.
	reportedZero := &pipeline.InferenceExtension{
		InputTokens: 1000, PromptTokens: 1000,
		PresentKinds: presentInput | presentOutput,
		FinishReason: "end_turn",
	}
	if reportedZero.PresentKinds != truncated.PresentKinds {
		t.Fatal("the two fixtures must be bit-identical for this test to mean anything")
	}
	if got := IncompleteReason(reportedZero); got != "" {
		t.Errorf("IncompleteReason = %q, want \"\": a stop reason means the tally finished, so this total is exact", got)
	}
}

// A floor must still PRICE. Refusing to price it would be the wrong fix: the prompt cost
// is real, and dropping it understates spend by more than presenting it as exact ever
// did. This is the property costing.Settle relies on to publish a figure at all.
func TestIncompleteReason_FloorStillPrices(t *testing.T) {
	inf := &pipeline.InferenceExtension{
		InputTokens: 1000, CacheReadTokens: 500, PromptTokens: 1500,
	}
	if IncompleteReason(inf) != ReasonOutputUncounted {
		t.Fatal("fixture is not a floor; the rest of this test proves nothing")
	}
	var r Rates
	for _, tier := range []Tier{TierInput, TierCacheWrite, TierCacheRead, TierOutput} {
		r.Base[tier], r.Set[tier] = 1e-6, true
	}
	micros, ok := Cost(r, UsageFromInference(inf))
	if !ok {
		t.Fatal("Cost refused a floor; a lower bound is still a figure, and dropping it loses real spend")
	}
	if micros != 1500 {
		t.Errorf("micros = %d, want 1500 (1000 input + 500 cache-read at 1e-6)", micros)
	}
}
