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
	"strings"
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
	//
	// EXPORTED BECAUSE encoding/json REQUIRES IT, like every other field here, and not
	// because a caller reads it: nothing outside this package does today. Unexporting it
	// would drop the column from every persisted row and from the accumulation key with
	// it, which is the opposite of tidying up. See Fold for what is served from it.
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

// maxLabelLen bounds one label ON DISK, and it is the same 96 bytes
// usage.maxLabelLen applies to the same two request-controlled fields — the value
// is matched deliberately rather than chosen again, so a model name is spelled
// identically in the ring and in the ledger and group=model answers the same from
// either source.
//
// Load-bearing here for a reason the ring does not have: these strings are WRITTEN
// TO AN APPEND-ONLY FILE that is retained for retentionDays. Model comes straight
// off the parsed request body, so a workload chooses its length. Uncapped, one
// request with a model name longer than maxLineBytes produced a line readDay cannot
// buffer and cannot step over, which ENDED that day's read at that offset — every
// row appended after it, for the rest of the day, unreadable from then on, on every
// future read, invisible in the API's totals and unfixable because the file is
// append-only. Measured: $3.00 of a $3.25 day gone to one 1 MiB model name.
//
// So the cap is on the WRITE side, where it is absolute. Four labels at 96 bytes
// plus a timestamp and the numeric counters put the longest line this package can
// emit at well under a kilobyte, three orders of magnitude below maxLineBytes —
// which is what turns readDay's over-long-line guard from a live failure mode into
// the last-resort guard for a file some other process corrupted.
// TestRecord_LabelsAreCappedSoALineCanNeverExceedTheReadLimit pins the arithmetic.
const maxLabelLen = 96

// truncateLabel caps one label. A byte cut, matching usage.truncateLabel and
// pipeline's maxClientLen exactly — including its documented willingness to split a
// multi-byte rune, because two truncation rules for one string that appears in both
// places would be a worse trade than the occasional replacement character.
func truncateLabel(s string) string {
	if len(s) <= maxLabelLen {
		return s
	}
	return s[:maxLabelLen]
}

// sanitizeLabel replaces control characters and DEL with U+FFFD.
//
// These strings are request- or upstream-controlled — Endpoint is the host the
// workload asked for and Model comes straight off the parsed request body — and they
// are written to a DURABLE file that an operator cats and that other tools parse. An
// escape sequence in a model name repositions the cursor, recolours the pane or erases
// the line that reports it, for every future read of a file that is retained for
// retentionDays and cannot be edited. CWE-150.
//
// REPLACED, NOT DROPPED, so tampering is visible instead of collapsing into a
// plausible-looking label: "m\x1b[31mx" reads as "m�[31mx" rather than as "m[31mx",
// which nobody would question.
//
// This is the same rule abctl's tui.sanitizeLabel applies when RENDERING these labels,
// and it is deliberately NOT that function: cmd/ is a main-module package this library
// must not import. Two copies of a five-line rule beats a dependency edge the wrong way
// round; the rule is stated in both doc comments so a future change to either is a
// visible divergence rather than a silent one.
//
// NOT a JSON-integrity guard — encoding/json escapes control bytes, so an unsanitised
// label could never split a line or break readDay. Every consumer downstream of the
// decode is what this protects.
//
// The one consequence worth stating: usage (the ring) does not sanitise, so a label
// carrying control bytes is now spelled differently in the two halves and group=model
// would show it as two series. That only happens for a label that is already hostile,
// the ring's copy dies with the process, and the ledger's is the one that survives —
// so the divergence is the right way round. The matching fix belongs in usage.
func sanitizeLabel(s string) string {
	if !hasControlBytes(s) {
		// The overwhelmingly common case, and no allocation for it: this runs on the
		// session-append path, under the store's write lock.
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0x7f || r < 0x20 {
			b.WriteRune('\uFFFD')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hasControlBytes reports whether s carries a C0 control byte or DEL.
//
// A BYTE scan, which is exact rather than approximate: no continuation byte of a
// multi-byte UTF-8 sequence is below 0x80, so a byte below 0x20 can only be that
// character itself.
func hasControlBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x7f || s[i] < 0x20 {
			return true
		}
	}
	return false
}

// rowLabel prepares one string for a durable row: sanitised, then capped.
//
// THE ORDER MATTERS. Sanitising can triple a string's length — every replaced byte
// becomes three — so capping first would let a label of 96 control bytes reach 288 on
// disk and break the line-length arithmetic maxLabelLen exists to guarantee. Capping
// last can split a U+FFFD, which truncateLabel already documents as an accepted cost.
func rowLabel(s string) string {
	return truncateLabel(sanitizeLabel(s))
}

// maxLabelsPerMinute caps how many DISTINCT rows one open minute accumulates, and
// it is usage.maxLabelsPerBucket's 64, again matched rather than re-chosen.
//
// TIGHTER THAN THE RING'S, and knowingly: usage applies 64 per axis, to independent
// marginals, while the key here is the (endpoint, model, agent, provenance) JOINT
// tuple, so 64 bounds the product rather than each factor. That is the bound this
// package needs, because the map is not the only cost — every entry is also copied
// into a batch by takeLocked on a minute roll, which happens INSIDE
// session.Store.Append's write lock, and it becomes a durable line. Unbounded, a
// caller varying the model string per request grew both: 50,000 keys held for one
// minute at 200 bytes each was measured, with an O(N) walk and allocation in front
// of the request that closed the minute.
//
// FOLDED, NOT DROPPED, past the cap — see overflowKey. A deployment busy enough to
// exceed 64 joint labels in one minute loses attribution DETAIL and no dollars,
// which is the right direction; raising this one constant is the fix if a real
// deployment ever does.
const maxLabelsPerMinute = 64

// overflowLabel collects everything past maxLabelsPerMinute. usage.overflowLabel's
// spelling, so a client that renders both sources shows one "(other)" band and not
// two.
//
// NOT A REAL LABEL, and no more spoofable-looking than the axes it replaces: a
// caller can send "(other)" as its model and land in this row, exactly as it can
// send another agent's name. The cap is a memory bound, not an authorization
// boundary.
const overflowLabel = "(other)"

// overflowKey is the one reserved accumulator slot every label past the cap folds
// into.
//
// ALL FOUR fields, not just the request-controlled two: the key is a tuple, so
// keeping any real field would let the overflow row multiply on that axis and defeat
// the bound it exists to enforce. The consequence is that a capped minute reports its
// excess as "(other)" on every axis at once, which reads as "cardinality was capped
// here" rather than as a plausible endpoint that spent money.
var overflowKey = key{overflowLabel, overflowLabel, overflowLabel, overflowLabel}

// overflow rewrites a row's identity onto overflowKey, keeping At and every counter.
// The dollars are unchanged; only the attribution is coarsened.
func overflow(r Row) Row {
	r.Endpoint, r.Model, r.Agent, r.Provenance = overflowLabel, overflowLabel, overflowLabel, overflowLabel
	return r
}
