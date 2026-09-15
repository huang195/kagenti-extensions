// Package costledger persists per-minute cost and token totals to disk so a
// question like "what did today cost" survives a restart.
//
// It exists because the usage aggregator's ring is 6 hours of in-memory buckets
// (usage.NumBuckets x usage.BucketWidth) and dies with the process. A coding
// session spans days; the release bar asks for numbers that are still there
// tomorrow.
//
// It is a SECOND session.Recorder, registered alongside the usage aggregator,
// rather than a reader of it. The aggregator keeps independent marginals —
// by-model, by-endpoint, by-plugin — not a joint distribution, so reading it
// could never reconstruct "this endpoint x this model x this provenance", and
// summing marginals would double-count. Recording independently also means this
// package cannot regress charting.
//
// TWO HALVES, ONE ANSWER. Closed minutes live on disk; the minute still
// accumulating lives in the Writer. Window returns both, and that is the only
// entry point a reader should use — see its doc for how non-overlap is enforced.
//
// The open minute comes from THIS package's accumulator and not from the usage
// aggregator's ring, though the ring also holds it. Four reasons, because an
// earlier draft of this doc claimed the ring and it would have been wrong:
//
//   - The ring prices independently. usage.Aggregator.costOf falls back to the
//     process rate table where this package does not (see the divergence paragraph
//     below), so a total assembled from both would have one minute priced by one
//     rule and the rest by another — an internally inconsistent figure, which is
//     harder to explain than either rule on its own.
//   - The ring's request denominator is different. It counts every response
//     carrying a Host, including MCP and health traffic; this package counts
//     inference only. Requests would jump for exactly one minute of the window.
//   - The ring is 6 hours. A minute held while the proxy sits idle overnight has
//     rotated out of the ring, but is still in the map right here.
//   - Ownership would become a timing question. The ring knows nothing about what
//     has been flushed, so any periodic flush could put a minute in both halves.
//     Reading the open minute from the writer that owns it makes the boundary exact
//     and provable rather than probable.
//
// It prices NOTHING. Every figure here is the one authlib/costing settled and
// inference-parser published on the event; this package only decodes and adds.
// That is the invariant cortex #972 exists to protect: one component turns
// tokens into dollars.
//
// CONSEQUENCES of pricing nothing, stated because they are real differences a reader
// will otherwise discover by comparing two totals on one screen. THREE of them, not
// one — the first is about dollars and the other two about the denominator those
// dollars are a fraction of:
//
//   - usage.Aggregator.costOf falls back to the process rate table for a request that
//     arrives with no settled record, and this package does not. Where that fallback
//     fires, /v1/usage over a ring window reports dollars the ledger records as
//     priceable-but-unpriced. On the live pipeline inference-parser settles every
//     inference response, so the two agree; a composition without it would show the
//     gap rather than a wrong number, which is the failure mode to prefer.
//
//   - PriceableRequests is counted differently. costOf sets priceable for ANY request
//     carrying a settled cost record, whatever its token counts, while the writer here
//     requires Model != "" and Tokens > 0. A settled record over zero tokens — a
//     gateway that reported a cost and no usage — therefore lands in the ring's
//     coverage denominator and not in the ledger's. It is priced in both, so the
//     dollars match and only the ratio differs.
//
//   - The token fields read here are the modern ones only. pricing.UsageFromInference
//     still falls back to InferenceExtension.PromptTokens and CompletionTokens when the
//     split counters are absent, so a producer emitting only the legacy pair yields a
//     ring figure with usage and a ledger row with Tokens == 0 — which then fails the
//     priceable test above. Not fixed by adding the legacy fields to Row: the schema
//     rule at the bottom of this doc is add-never-rename, and adding two columns for a
//     shape parsercommon.Fill no longer produces would put them on every future row.
//     The right fix is upstream, where the legacy pair is normalised into the split.
//
// On disk it holds hosts, model names, counts, dollars and timestamps. No prompt
// content, no completions, no tool arguments — ever. That is a user-facing promise
// and TestWriter_HoldsNoPromptContent asserts it against the serialized bytes.
//
// SCHEMA STABILITY: Row embeds usage.Counts, so a field added there changes what
// lands on disk with no edit here. That is the point — one vocabulary — but it
// means the ledger's JSON is only as stable as Counts'. Additive changes keep old
// files readable, because an absent field decodes to its zero; a RENAME would
// silently read every historical row as zero for that column. Add, never rename.
package costledger

import (
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Row is one minute's totals for one (endpoint, model, agent, provenance) key.
//
// usage.Counts is EMBEDDED rather than restated so the ledger, /v1/usage and any
// future collector share one vocabulary: a field added there appears here, and a
// divergence is a compile error instead of a review question. It also means
// Counts.Add is the summation for both — including IncompleteRequests, which is
// how the "this total is a floor, not an exact figure" caveat survives a restart
// instead of being the one thing a persisted total silently loses.
type Row struct {
	At time.Time `json:"at"`
	// Endpoint is the target host, taken from SessionEvent.Host.
	//
	// Named endpoint rather than host because that is what it MEANS to a reader of
	// a cost row: half of the rate key, since the same model bills differently on a
	// discounted gateway than on the vendor endpoint. The rename from the event's
	// field name is deliberate and matches usage.GroupEndpoint, which reads the
	// same field under the same name.
	Endpoint string `json:"endpoint,omitempty"`
	Model    string `json:"model,omitempty"`
	// Agent identifies the calling coding agent, as "name/version" —
	// "claude-code/2.1.14" — or as its raw User-Agent when the parser did not
	// recognise it. Populated from pipeline.EventClient.Label; see Writer.Record.
	//
	// CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE, because it is derived from a request
	// header: a cost-attribution and display key, never an authorization subject.
	// Nothing may branch on it.
	//
	// EMPTY means the request carried no User-Agent. Stored as "" rather than as the
	// display string "unknown" deliberately, and this is the one representation choice
	// in this file a reader joining the ledger to /v1/usage has to know about:
	//
	//   - This is a DURABLE file. Writing "unknown" into it would permanently destroy
	//     the difference between "no agent was recorded" and "an agent that reported
	//     itself as unknown", for every future reader of every retained day. "" plus
	//     omitempty is lossless and costs no bytes.
	//   - The live aggregator's series uses "unknown" instead, because its keys are
	//     display strings a client renders directly, where a "" key is a blank row that
	//     reads as a rendering bug.
	//   - labelFor maps "" to "unknown" at the query boundary, so group=agent answers
	//     IDENTICALLY whether it was served from the ring or from this file. The two
	//     sources differ in representation and agree in meaning; nothing a client sees
	//     differs.
	Agent string `json:"agent,omitempty"`
	// Provenance is part of the KEY, not a summary of the row: one minute can mix a
	// gateway's own figures with modelled ones, and a single provenance per row
	// would have to pick a winner. Keying on it keeps /v1/usage's pricedBy
	// reconstructible from the ledger.
	Provenance string `json:"provenance,omitempty"`

	usage.Counts
}

// key is the in-memory accumulation key. Mirrors the JSON identity fields exactly.
type key struct {
	endpoint, model, agent, provenance string
}

func (r Row) key() key {
	return key{r.Endpoint, r.Model, r.Agent, r.Provenance}
}
