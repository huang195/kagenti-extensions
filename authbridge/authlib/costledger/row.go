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
// It prices NOTHING. Every figure here is the one authlib/costing settled and
// inference-parser published on the event; this package only decodes and adds.
// That is the invariant cortex #972 exists to protect: one component turns
// tokens into dollars.
//
// A CONSEQUENCE of pricing nothing, stated because it is a real difference a
// reader will otherwise discover by comparing two totals: usage.Aggregator.costOf
// falls back to the process rate table for a request that arrives with no settled
// record, and this package does not. Where that fallback fires, /v1/usage over a
// ring window reports dollars the ledger records as priceable-but-unpriced. On the
// live pipeline inference-parser settles every inference response, so the two
// agree; a composition without it would show the gap rather than a wrong number,
// which is the failure mode to prefer.
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
	// Agent identifies the calling coding agent, as "name/version".
	//
	// EMPTY until the change that captures the client from the User-Agent. The
	// field ships now, unpopulated, because a schema that gains a column later is
	// worse for every reader than one that has an empty column from the start.
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
