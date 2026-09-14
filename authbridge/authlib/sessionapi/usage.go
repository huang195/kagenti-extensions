package sessionapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// handleUsage serves GET /v1/usage — time-bucketed volume, error, latency and
// cost aggregates for charting.
//
// Query parameters:
//
//	window      10m (default), 1h, 6h — any multiple of the bucket width up to
//	            the retained maximum
//	resolution  bucket width to return, e.g. 5m for a 1h window rendered as 12
//	            bars. Defaults to the 1m storage resolution. Folding is done
//	            here, not in the client, so every consumer gets the same
//	            arithmetic — see usage.fold for why latency in particular cannot
//	            be folded naively.
//	session     session ID; omit for all sessions combined
//	group       none (default), model, endpoint, status, plugin. "method" is
//	            accepted as an alias for "model" — the series shipped under that
//	            name before it was clear the aggregator only ever populated it
//	            from the inference model.
//
// UNAUTHENTICATED, like every endpoint on this listener. Bind it on in-cluster
// addresses only, never behind ingress — the trust model is documented in
// authbridge/CLAUDE.md and applies here unchanged.
//
// This response is less sensitive than /v1/sessions, which serves raw prompts,
// completions and tool results. It carries no message content at all: only
// counts, timings and cost. But it is not free of information either, and three
// groupings leak deployment shape to anyone who can reach the port:
//
//   - group=model exposes the model names in use (claude-sonnet-5, and any
//     internal or preview model an operator is testing against). group=method is
//     an alias for it and exposes exactly the same thing.
//
//   - group=endpoint exposes every upstream host the proxy talked to, not only the
//     inference ones: the accumulator is populated for any response or denial
//     carrying a Host, with no inference guard, so MCP servers, A2A peers and tool
//     backends appear alongside model gateways — internal hostnames included. That
//     is deployment topology rather than model choice, which makes it the most
//     sensitive of the groupings.
//
//     It also means the two axes of one cost table have different denominators:
//     group=endpoint rows can carry requests with no tokens and no cost, while
//     group=model is inference-only because that accumulator requires a model name.
//     Their request totals will not reconcile, and that is correct rather than a
//     bug — but a client putting the two side by side has to say so.
//
//   - group=plugin exposes the active pipeline composition — though /v1/pipeline
//     already publishes that in full, so this adds no new exposure.
//
// Cost figures also disclose spend, which is business-sensitive in a way raw
// request counts are not. None of this changes the listener's existing posture;
// it is written down so the decision to expose it is a decision rather than an
// oversight.
//
// costMicros is populated from the figure inference-parser settles on the
// response pass. It prefers the gateway's own post-discount cost header — the
// authoritative figure — and falls back to pricing the parsed token counters when
// the header is absent or reports 0, which every streamed response does.
//
// The parser owns that decision because it is the only component that knows when
// the token counters are final. Cost used to be computed in four places, and
// which one answered depended on which plugins an operator had enabled: a request
// could carry a token count with no money in a live abctl view while showing
// dollars here. litellm-budget-track now consumes the settled figure to enforce a
// budget rather than computing one of its own.
//
// Requests the parser did not price contribute no cost and appear as the gap
// between totals.pricedRequests and totals.requests. Where those differ the
// dollar total covers only the priced subset, so a client rendering it must
// present it as partial rather than complete. priced:false means nothing at all
// was priced — render "cost unavailable", never $0.00, which would read as "this
// traffic was free".
//
// Traffic that carries no settled figure is still priced here, from the process
// rate table: the pricing resolver has landed, so modelled rates are no longer
// something that arrives later — usage.Aggregator.costOf resolves a rate for any
// request carrying a model and tokens but no cost record. See
// docs/superpowers/specs/2026-09-09-pricing-consolidation-design.md.
//
// Which means cost is NOT single-sourced, and this comment deliberately stops
// short of claiming that it is. inference-parser settles the figure most requests
// arrive with, but the aggregator still prices independently when none is present,
// so the two can answer differently about the same request. Collapsing them onto
// the parser's figure alone is a later change; until it lands, do not write here
// that cost is computed in exactly one place, because it is not.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		// Aggregation not wired up (session store disabled, or an older binary
		// composition). 404 rather than an empty snapshot: "no such channel" and
		// "channel with nothing in it" are different answers, and a client that
		// gets zeros would render a flat chart implying idle traffic.
		http.Error(w, `{"error":"usage aggregation not enabled"}`, http.StatusNotFound)
		return
	}

	window, err := usage.ParseWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeUsageError(w, err)
		return
	}
	resolution, err := usage.ParseResolution(r.URL.Query().Get("resolution"), window)
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

	snap := s.usage.Snapshot(window, resolution, sessionID, group)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		slog.Debug("sessionapi: usage encode failed", "error", err)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

var errSessionIDTooLong = usageError{"session id too long"}

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
