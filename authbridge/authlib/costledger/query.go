package costledger

import (
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Query returns every row whose minute falls in [from, to], inclusive at minute
// granularity.
//
// Reads only what the Writer has flushed — closed minutes. The caller stitches the
// open minute from the in-memory ring; see the package doc for why the two must
// never both own a minute.
func (w *Writer) Query(from, to time.Time) ([]Row, error) {
	if to.Before(from) {
		from, to = to, from
	}
	fromMin, toMin := from.Truncate(time.Minute), to.Truncate(time.Minute)

	var out []Row
	// Walk dates rather than globbing the directory: the read stays bounded by the
	// span the caller asked for instead of by how long the ledger has been running.
	for d := dayOf(fromMin); !d.After(dayOf(toMin)); d = d.AddDate(0, 0, 1) {
		rows, err := w.store.readDay(d)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if m := r.At.Truncate(time.Minute); m.Before(fromMin) || m.After(toMin) {
				continue
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// dayOf truncates to LOCAL midnight. Local, not UTC: "today" means the operator's
// day, and a laptop that crosses a timezone must not have its day reset
// mid-afternoon.
//
// AddDate on the result is how the day walk advances, and it is DST-correct where a
// 24h addition is not: on a spring-forward day local midnight plus 24h is 01:00 the
// next day, which would skip the first hour of that day's file.
func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// Fold sums rows into one total plus an optional per-label series, in the shape
// /v1/usage already serves.
//
// Returns usage.Counts and a Group-keyed map so an HTTP handler need not branch on
// whether the data came from the ring or from disk. Summation is Counts.Add, so a
// field added there is carried here with no edit — the same property that keeps
// fold() and abctl's (other)-band collapse correct.
func Fold(rows []Row, group usage.Group) (usage.Counts, map[string]usage.Counts) {
	var totals usage.Counts
	var series map[string]usage.Counts
	for _, r := range rows {
		totals.Add(r.Counts)
		label, ok := labelFor(r, group)
		if !ok {
			continue
		}
		if series == nil {
			series = map[string]usage.Counts{}
		}
		cur := series[label]
		cur.Add(r.Counts)
		series[label] = cur
	}
	return totals, series
}

// labelFor picks the grouping key, returning ok=false when the row carries no
// value for that axis — so an empty key never becomes a blank row, the same guard
// the live foldInto applies. The row still counts toward totals either way.
//
// usage.GroupSession, GroupStatus and GroupPlugin fall to the default and produce
// no series, because a ledger row carries none of the three: a session is a
// laptop-lifetime concept, and status and plugin composition are per-request facts
// a per-minute row cannot represent without one entry per combination. Returning
// no series is the honest answer; an empty map that a client renders as "no
// breakdown" would be the same answer said less clearly.
func labelFor(r Row, group usage.Group) (string, bool) {
	var v string
	switch group {
	// Both spellings read Model: group=method shipped as the model series under a
	// wrong name, and the alias must behave identically here too.
	case usage.GroupModel, usage.GroupMethod:
		v = r.Model
	case usage.GroupEndpoint:
		v = r.Endpoint
	default:
		return "", false
	}
	return v, v != ""
}
