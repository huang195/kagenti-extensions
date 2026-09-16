package sessionapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costledger"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// handleUsage serves GET /v1/usage — time-bucketed volume, error, latency and cost
// aggregates for charting.
//
// Query parameters:
//
//	window      10m (default), or any multiple of the bucket width up to the ring's
//	            maximum, or the SYMBOLIC windows "today" (LOCAL midnight to now, so a
//	            laptop crossing a timezone does not reset its day mid-afternoon) and
//	            "7d" (a rolling 7x24h). Symbolic windows are served from the durable
//	            cost ledger; where it is off, the ring's maximum is served instead and
//	            the response's own "window" field names what was served.
//	resolution  bucket width to return; defaults to the 1m storage resolution. Folded
//	            here rather than in the client so every consumer gets the same
//	            arithmetic — see usage.fold for why latency cannot be folded naively.
//	session     session ID; omit for all sessions. REFUSED alongside a symbolic window:
//	            the ledger holds no session ids, and serving all-sessions data under a
//	            session label would be worse than refusing.
//	group       none (default), model, endpoint, session, agent, status, plugin.
//	            "method" is an alias for "model".
//
// THREE THINGS A CLIENT MUST NOT GET WRONG:
//
//  1. The two window kinds disagree about the same traffic, by design. A duration
//     window comes from the ring, which prices an unpriced request from the process
//     rate table and counts non-inference traffic; the ledger does neither. Never
//     subtract a ledger figure from a ring figure and present the result as spend.
//  2. priceableRequests is the coverage denominator, never requests — which counts
//     tool calls, health checks and tunnels that can never carry a price, so a client
//     using it would mark every total "partial" forever and train readers to ignore
//     the one caveat that matters.
//  3. priced:false means nothing was priced. Render "cost unavailable", never $0.00,
//     which reads as "this traffic was free".
//
// UNAUTHENTICATED, like every endpoint on this listener; bind it in-cluster only,
// never behind ingress. It carries no message content, but it is not free of
// information: group=endpoint discloses every upstream host including internal ones,
// group=agent discloses which coding agents at which versions run on a workstation,
// group=session pairs client-chosen ids with spend, and cost figures disclose spend.
//
// THE SYMBOLIC WINDOWS RAISE THAT MATERIALLY and are the first thing here that would
// need a credential if this port were ever exposed. Everything else is bounded by the
// six-hour ring — the worst an unauthenticated reader takes is an afternoon from a
// process that happened to be up. window=7d answers "what has this developer's agent
// cost over a week", which is a fact about a person. Two things bound it and neither
// is authentication: the ledger needs a durable location (so in Kubernetes it is
// usually off, and the --local config pins every listener to loopback), and session=
// is refused for a symbolic window, so a week of spend cannot be pinned to one named
// session through this path.
//
// COST IS NOT SINGLE-SOURCED, and this comment deliberately does not claim it is.
// authlib/costing settles the figure most requests arrive with, but the aggregator
// still prices independently when none is present, so the two can answer differently
// about the same request. Do not write here that cost is computed in one place.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		// Aggregation not wired up (session store disabled, or an older binary
		// composition). 404 rather than an empty snapshot: "no such channel" and
		// "channel with nothing in it" are different answers, and a client that
		// gets zeros would render a flat chart implying idle traffic.
		http.Error(w, `{"error":"usage aggregation not enabled"}`, http.StatusNotFound)
		return
	}

	spec, err := usage.ParseWindowSpec(r.URL.Query().Get("window"), time.Now())
	if err != nil {
		writeUsageError(w, err)
		return
	}
	// The span resolution is validated against is the one that can actually be
	// SERVED, not the one requested. A symbolic window is served either as one
	// ledger-backed bucket, where the requested resolution is not read at all, or as
	// the ring's maximum; validating "7d" against seven days would accept a 24-hour
	// bucket and then return a response whose own BucketSeconds contradicted it.
	resSpan := spec.Dur
	if spec.Symbolic() {
		resSpan = usage.MaxWindow
	}
	resolution, err := usage.ParseResolution(r.URL.Query().Get("resolution"), resSpan)
	if err != nil {
		writeUsageError(w, err)
		return
	}
	group, err := usage.ParseGroup(r.URL.Query().Get("group"))
	if err != nil {
		writeUsageError(w, err)
		return
	}
	sessionID := r.URL.Query().Get("session")
	if len(sessionID) > session.MaxSessionIDLen {
		writeUsageError(w, errSessionIDTooLong)
		return
	}
	// REJECTED rather than ignored, and rejected whether or not a ledger exists.
	//
	// A ledger row carries no session id: a session is a laptop-lifetime concept
	// while the ledger is a day-lifetime one, and persisting a client-supplied id per
	// minute would both grow the row key without bound and put an identifier of the
	// client's choosing on disk. So the filter cannot be applied — and serving
	// all-sessions data under a session label would be a wrong number wearing a right
	// label, which is the single failure this whole branch keeps refusing.
	//
	// Unconditional, even where the ring COULD answer for one session over six hours,
	// because the alternative is an API whose behaviour depends on the deployment: the
	// same request would 400 on a laptop and degrade in Kubernetes, and a client
	// cannot code against that. Asking for a duration window instead is one edit and
	// the error says so.
	if sessionID != "" && spec.Symbolic() {
		writeUsageError(w, errSessionWithSymbolicWindow)
		return
	}

	var snap usage.Snapshot
	switch {
	case !spec.Symbolic():
		snap = s.usage.Snapshot(spec.Dur, resolution, sessionID, group)
	case s.ledger == nil:
		// No ledger: Kubernetes by design, where files in a pod are the wrong sink
		// and the central collector is the right one. Serve what the ring HAS rather
		// than 400 — an abctl cost view must degrade to a shorter window, not fail —
		// and Snapshot reports the window actually served, so the client never
		// mislabels a 6-hour figure as a day's.
		snap = s.usage.Snapshot(usage.MaxWindow, resolution, sessionID, group)
	default:
		// r.Context(), so a client that hangs up stops the read. The ledger walks one day
		// file per day in the window — up to eight for window=7d, against a path an
		// operator configured and possibly a slow mount — and without this every abandoned
		// request kept reading to the end for nobody. See costledger.Query.
		snap, err = s.ledgerSnapshot(r.Context(), spec, group)
		if err != nil {
			// A read failure is not a client error and must not look like one.
			//
			// CANCELLATION IS NOT A FAULT, and it is separated out because it would otherwise
			// be the loudest line in the log on the surface most likely to produce it: a chart
			// that re-requests on every keystroke cancels its own in-flight reads, and a Warn
			// per cancelled request would teach an operator to filter out the message that also
			// reports a genuinely unreadable ledger. The 503 is still written — net/http would
			// otherwise send an empty 200 body, which is not decodable JSON for the rare
			// cancellation that is not a vanished client.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				slog.Debug("sessionapi: cost ledger read abandoned; the request went away",
					"window", spec.Label, "error", err)
			} else {
				slog.Warn("sessionapi: cost ledger read failed", "window", spec.Label, "error", err)
			}
			http.Error(w, `{"error":"cost history unavailable"}`, http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		slog.Debug("sessionapi: usage encode failed", "error", err)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

var errSessionIDTooLong = usageError{"session id too long"}

// errSessionWithSymbolicWindow refuses session= alongside window=today|7d. A fixed
// string, like every other message this endpoint returns — see writeUsageError.
var errSessionWithSymbolicWindow = usageError{
	"session= cannot be combined with a symbolic window (today, 7d); " +
		"the durable cost ledger holds no session ids — ask for a duration window such as 1h or 6h"}

// ledgerSnapshot builds a Snapshot from persisted rows.
//
// ONE bucket spanning the whole window, not a series at the requested resolution.
// The ledger exists to answer "what did today cost", and a client wanting a shaped
// chart asks for a ring window instead — synthesising per-minute buckets from disk
// for a 7-day span would read millions of rows to draw a chart nothing requests.
// BucketSeconds reports the real span so a client cannot mistake it for a fine
// series.
//
// Reads through costledger.Window, never Query: the ledger's day files hold only
// CLOSED minutes, so the minute currently accumulating is in the writer's memory
// and Query alone would omit it. That omission is not small in the case this
// endpoint exists for — a session whose whole conversation fit inside one minute has
// nothing on disk at all, and the response would have said priced:false over real
// spend. Window composes the two halves and guarantees no minute is in both; see its
// doc.
//
// Takes no session id: handleUsage rejects that combination before reaching here,
// for the reason recorded at the guard.
//
// Takes the REQUEST's context, so the day-file walk stops when the caller does. Threaded
// rather than context.Background() because this is the only work this endpoint does that
// is neither bounded nor in memory.
//
// UnpricedBy and PricedBy are deliberately absent. Provenance IS in the row key, so
// PricedBy is reconstructible and a later change can add it; UnpricedBy needs the
// endpoint-and-model pair of the requests that could NOT be priced, which a row
// carrying only its own labels cannot distinguish from a priced one of the same
// pair. Emitting one map and not the other would read as "no pricing gaps here",
// which is a claim the rows do not support — the gap is still visible, in
// Totals.PricedRequests against Totals.PriceableRequests.
func (s *Server) ledgerSnapshot(ctx context.Context, spec usage.Spec, group usage.Group) (usage.Snapshot, error) {
	rows, caveats, err := s.ledger.Window(ctx, spec.From, spec.To)
	if err != nil {
		return usage.Snapshot{}, err
	}
	// THE GROUPING THIS SOURCE CAN APPLY, which is not always the one that was asked for,
	// and the response says which it was.
	//
	// A ledger row is (endpoint, model, agent, provenance) per minute, so group=session,
	// group=status and group=plugin have no column to key on here — while the ring, which
	// serves the same axes over a duration window, answers all three. Echoing the
	// requested group over an empty series made those two states indistinguishable from
	// the response: a client asking for status got group:"status" with series:null and no
	// way to tell "this source cannot break down by status" from "there was no traffic".
	// Worse, Fold used to publish a residual for them, so the response also claimed 100%
	// of its own total was unaccounted for. See costledger.Groupable.
	//
	// Reported as the grouping IN EFFECT rather than refused with a 400, because a
	// rejection would have to be conditional on a ledger being wired up at all — the same
	// request is served from the ring in Kubernetes, where it is answerable — and an API
	// whose validity depends on the deployment is one a client cannot code against. That
	// is the argument the session= guard above makes in the other direction, and it is
	// load-bearing in both.
	applied := group
	if !costledger.Groupable(group) {
		applied = usage.GroupNone
	}
	totals, series, ungrouped := costledger.Fold(rows, applied)
	// Carried onto the totals BEFORE the snapshot is built, not left to
	// SetUngroupedCost's own assignment below. This response's single bucket is a copy of
	// totals, so setting the flag afterwards would mark Totals as a bound while the bucket
	// holding the same figure claimed to be exact — and a client charting the bucket would
	// never see it. The ring's Snapshot has the opposite shape (many buckets, one
	// cross-bucket residual), which is why the setter flags Totals there and this flags
	// both here.
	if ungrouped.Saturated {
		totals.Saturated = true
	}
	// FROM THE READ THAT PRODUCED THEM, which is why they come back from Window rather
	// than off the ledger. They used to be two atomics on the Writer, sampled here right
	// after Window returned — so any other /v1/usage request landing between those two
	// calls handed this response its caveats. Measured: a reader of a day file holding one
	// undecodable line reported SkippedLines 0, while a reader of a CLEAN day reported 1.
	// A chart polling this endpoint is exactly the traffic that produces it.
	//
	// Surfaced here because the ledger having the numbers is not the same as a client
	// being able to see them. Until this, a day that lost lines produced a response
	// byte-identical to a clean one — a short total under priced:true — so the skip that
	// saved the rest of the day was invisible to everyone downstream of it.
	var degraded *usage.Degraded
	if !caveats.Clean() {
		degraded = &usage.Degraded{SkippedLines: caveats.SkippedLines, TruncatedDays: caveats.TruncatedDays}
		// At Warn, and unconditionally: a client may not render the field, and an operator
		// with a corrupt day file wants to hear about it once per read rather than never.
		slog.Warn("sessionapi: cost ledger read was incomplete — the total is short",
			"window", spec.Label, "skippedLines", caveats.SkippedLines,
			"truncatedDays", caveats.TruncatedDays)
	}
	snap := usage.Snapshot{
		Window:        spec.Label,
		BucketSeconds: int(spec.To.Sub(spec.From).Seconds()),
		// The grouping SERVED, on the same rule as Window above: a response says what it
		// actually did, and a client that asked for something else learns so by comparing.
		Group:    applied,
		Buckets:  []usage.Bucket{{At: spec.From, Counts: totals, Series: series}},
		Totals:   totals,
		Degraded: degraded,
		// Same rule as Aggregator.Snapshot: an inexact figure is still a figure, so a
		// window whose every request was a truncated stream reports priced:true over a
		// real total and discloses the caveat in Totals.IncompleteRequests. Withholding
		// the figure would render "cost unavailable" over dollars that are known.
		Priced: totals.PricedRequests > 0,
	}
	// The dollars the breakdown above does not account for — a gateway-priced response the
	// inference parser could not read is stored with no model, so it counts toward Totals
	// and cannot be a group=model key. Set through the setter so a window with nothing to
	// disclose serialises no field at all, exactly like Degraded; see
	// usage.Snapshot.UngroupedCostMicros for what a client does with it.
	snap.SetUngroupedCost(ungrouped)
	return snap, nil
}

// writeUsageError returns 400 with the validation message. Every message it can
// carry is a fixed string authored in this package or in authlib/usage: none
// interpolates query input. That is a requirement, not an accident — this
// endpoint is unauthenticated, so reflecting caller-supplied bytes into a
// response body would hand an attacker a reflection primitive. Keep it that way
// when adding validation.
func writeUsageError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	if encErr := json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: err.Error()}); encErr != nil {
		slog.Debug("sessionapi: usage error encode failed", "error", encErr)
	}
}
