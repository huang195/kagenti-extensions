package costledger

import (
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Window returns everything the ledger knows about [from, to]: the closed minutes
// on disk plus the open minute still in memory.
//
// THIS is what a reader calls. Query alone answers only for what has been flushed,
// which systematically omits the minute currently accumulating — and omits it
// indefinitely once traffic stops, since the flush is driven by the next event. A
// "today" figure that silently excluded live spend was the defect this exists to
// close; worse, a session whose whole conversation fit inside one minute produced no
// disk rows at all and rendered as "cost unavailable" over real money.
//
// NON-OVERLAP is by construction, in three steps, because a minute counted twice is
// a worse answer than a minute counted late:
//
//  1. The open minute is read FIRST. Its rows cannot then be missed by a flush
//     landing between the two reads — the failure mode of reading disk first.
//  2. Disk rows at or after that minute are DROPPED. The writer's ownership rule
//     (see the Writer doc) says there are none; if a concurrent flush produced some
//     between step 1 and step 3, they are the same rows step 1 already holds, and
//     this is what stops them being added twice.
//  3. Only then are the day files read.
//
// So the arithmetic is: every minute below the open one comes from disk, the open
// one comes from memory, and neither can supply the other's. TestWindow_* covers
// each step, including a hand-seeded overlap that step 2 has to absorb.
func (w *Writer) Window(from, to time.Time) ([]Row, error) {
	fromMin, toMin := span(from, to)
	pending, open := w.pending()

	rows, err := w.Query(from, to)
	if err != nil {
		return nil, err
	}
	if !open.IsZero() {
		kept := rows[:0]
		for _, r := range rows {
			if !r.At.Truncate(time.Minute).Before(open) {
				continue
			}
			kept = append(kept, r)
		}
		rows = kept
	}
	for _, r := range pending {
		if m := r.At.Truncate(time.Minute); m.Before(fromMin) || m.After(toMin) {
			// The held minute can fall outside the window asked for — an idle proxy still
			// holds yesterday's last minute at 00:05 today, and that spend belongs to
			// yesterday. Filtered with the same bounds Query applies so the two halves of
			// one answer cannot disagree about which minutes are in it.
			continue
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// Query returns every row whose minute falls in [from, to], inclusive at minute
// granularity.
//
// Reads only what the Writer has flushed — closed minutes. Prefer Window, which
// adds the open minute; this is the disk half on its own, kept separate so
// "only closed minutes reach disk" stays directly testable.
func (w *Writer) Query(from, to time.Time) ([]Row, error) {
	fromMin, toMin := span(from, to)

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

// span normalises a caller's range to inclusive minute bounds.
//
// A reversed range is a caller mistake, not a reason to return nothing: swapping
// answers the question that was meant instead of an empty result a client would
// render as "no spend". Shared by Query and Window so one answer cannot be
// assembled from two different readings of the same range.
func span(from, to time.Time) (time.Time, time.Time) {
	if to.Before(from) {
		from, to = to, from
	}
	return from.Truncate(time.Minute), to.Truncate(time.Minute)
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
