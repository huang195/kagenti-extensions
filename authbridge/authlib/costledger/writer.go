package costledger

import (
	"log/slog"
	"sync"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Writer accumulates the open minute in memory and appends closed minutes.
//
// Only CLOSED minutes reach disk. The open one belongs to the in-memory ring, and
// a reader stitches the two — so a minute never exists in both places and cannot
// be double-counted. The cost of that boundary is that a restart mid-minute loses
// up to 60 seconds of cost, which is documented rather than hidden: Flush on
// shutdown closes the gap for an orderly stop, and nothing can close it for a kill.
type Writer struct {
	store *store
	now   func() time.Time
	// retainDays is how many day files survive prune. Zero means the package
	// default; newStore owns that so the number is defined once.
	retainDays int

	mu sync.Mutex
	// open is the minute currently accumulating, truncated to the minute.
	open time.Time
	rows map[key]*Row
	// prunedDay is the local day retention was last enforced for. Pruning once per
	// day rather than per flush keeps a directory listing off the per-minute path,
	// and pruning on the DAY ROLL rather than only at startup matters for the case
	// this is built for: a laptop proxy that runs for weeks without a restart would
	// otherwise never enforce retention at all.
	prunedDay time.Time
}

// Option configures a Writer.
type Option func(*Writer)

// WithClock replaces the time source, for tests.
func WithClock(fn func() time.Time) Option { return func(w *Writer) { w.now = fn } }

// WithRetentionDays sets how many day files survive. Zero or negative means the
// package default.
func WithRetentionDays(days int) Option {
	return func(w *Writer) { w.retainDays = days }
}

// New opens a ledger under dir, creating it if needed.
//
// Enforces retention once here, before serving anything: a restart is the moment a
// long-idle ledger is most likely to be holding files past their window, and doing
// it at construction keeps the check off every later write path but one.
func New(dir string, opts ...Option) (*Writer, error) {
	w := &Writer{now: time.Now, rows: map[key]*Row{}}
	for _, o := range opts {
		o(w)
	}
	s, err := newStore(dir, w.retainDays)
	if err != nil {
		return nil, err
	}
	w.store = s
	now := w.now()
	w.prunedDay = dayOf(now)
	if perr := s.prune(now); perr != nil {
		// Not fatal, and not returned. A ledger that cannot delete an old file is
		// still a ledger that can record today's spend, and refusing to start the
		// proxy over it would trade an observability nicety for an outage.
		slog.Warn("costledger: retention prune failed; old day files remain", "dir", dir, "error", perr)
	}
	return w, nil
}

// Record implements session.Recorder.
//
// Never returns an error and never blocks on IO beyond one append per minute
// roll: this runs on the synchronous session-append path, under the store's write
// lock, and the ledger is observability. A failure here must not become a failed
// request.
func (w *Writer) Record(_ string, e *pipeline.SessionEvent) {
	if e == nil || e.Inference == nil {
		// Non-inference traffic the proxy handled — MCP, health checks, tunnels.
		// Recording it would put every proxied response in the cost denominator,
		// the mistake that made a correct deployment read "1/10 priced" forever.
		return
	}
	// Terminal events only. A request event carries no token counts and no cost, so
	// folding it would double the request count for every turn.
	//
	// Denials are included alongside responses, matching usage.Aggregator.Record's
	// own guard: a denial after the parser ran reports a model, so excluding it here
	// would give the ledger a smaller priceable denominator than /v1/usage has for
	// the same traffic, and the two coverage ratios would disagree for no stated
	// reason.
	if e.Phase != pipeline.SessionResponse && e.Phase != pipeline.SessionDenied {
		return
	}

	minute := e.At.Truncate(time.Minute)
	if e.At.IsZero() {
		// A zero At would file the row under year 1 and make it invisible to every
		// query. The aggregator substitutes its own clock for the same reason.
		minute = w.now().Truncate(time.Minute)
	}
	r := Row{
		At: minute,
		// Host is the event's field name; endpoint is what it means here. See Row.Endpoint.
		Endpoint: e.Host,
		Model:    e.Inference.Model,
		Counts: usage.Counts{
			Requests:         1,
			InputTokens:      int64(e.Inference.InputTokens),
			CacheReadTokens:  int64(e.Inference.CacheReadTokens),
			CacheWriteTokens: int64(e.Inference.CacheWriteTokens),
			OutputTokens:     int64(e.Inference.OutputTokens),
			ReasoningTokens:  int64(e.Inference.ReasoningTokens),
			Tokens:           int64(e.Inference.TotalTokens),
			PresentKinds:     e.Inference.PresentKinds,
		},
	}
	if e.StatusCode >= 400 || e.Phase == pipeline.SessionDenied {
		r.Errors = 1
	}
	// Priceable: carried a model and a non-zero token count. The denominator for
	// coverage — Requests is the wrong one, since only inference can be priced.
	if r.Model != "" && r.Tokens > 0 {
		r.PriceableRequests = 1
	}
	// The figure authlib/costing settled. Record, not Decode: an unpriced record
	// still exists and carries provenance, and later it carries savings.
	if ev, ok := costevent.Record(e); ok {
		r.Provenance = ev.Provenance
		if ev.Priced() {
			r.CostMicros = ev.Micros()
			r.PricedRequests = 1
			if ev.Incomplete {
				// The one caveat a persisted total cannot afford to lose. CostMicros here is
				// a floor (a stream that died before its output count) or an approximation (a
				// gateway reporting only a total), and without this counter a restart would
				// leave the dollars on disk with nothing left saying they are inexact — a
				// figure that quietly gains a precision it never had.
				//
				// Disclosed, not deducted: the dollars stay in CostMicros and the request
				// stays in PricedRequests, exactly as usage.Counts.IncompleteRequests
				// specifies. Set only inside the priced branch, because it is a subset of
				// PricedRequests and never a sibling of it.
				r.IncompleteRequests = 1
			}
		}
	}

	w.add(minute, r)
}

// add folds one row into the open minute, flushing first if the minute rolled.
func (w *Writer) add(minute time.Time, r Row) {
	w.mu.Lock()
	defer w.mu.Unlock()

	switch {
	case w.open.IsZero():
		w.open = minute
	case minute.After(w.open):
		w.flushLocked()
		w.open = minute
		w.pruneOnDayRollLocked(minute)
	case minute.Before(w.open):
		// A late event for an already-closed minute. Folding it into the open minute
		// would misdate it; appending a second row for a closed minute is correct,
		// because a reader sums every row for a timestamp rather than assuming one.
		w.appendRows([]Row{r})
		return
	}

	if cur, ok := w.rows[r.key()]; ok {
		cur.Add(r.Counts)
		return
	}
	row := r
	w.rows[r.key()] = &row
}

// pruneOnDayRollLocked enforces retention the first time a minute in a new local
// day is opened. Caller holds mu.
func (w *Writer) pruneOnDayRollLocked(minute time.Time) {
	day := dayOf(minute)
	if !day.After(w.prunedDay) {
		return
	}
	w.prunedDay = day
	if err := w.store.prune(minute); err != nil {
		slog.Warn("costledger: retention prune failed; old day files remain", "error", err)
	}
}

// Flush forces the open minute to disk. For shutdown and for tests.
//
// Returns the store's error so a shutdown path can log it, unlike Record, which
// has a request to serve and must not.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked()
}

func (w *Writer) flushLocked() error {
	if len(w.rows) == 0 {
		return nil
	}
	out := make([]Row, 0, len(w.rows))
	for _, r := range w.rows {
		out = append(out, *r)
	}
	// Cleared whether or not the append succeeds. Retrying next minute would append
	// the same minute twice on a partial failure — a double-counted minute is worse
	// than a missing one, because nothing downstream can detect it.
	w.rows = map[key]*Row{}
	return w.appendRows(out)
}

// appendRows writes and swallows the error into a log. Callers on the hot path
// must not learn about disk problems.
func (w *Writer) appendRows(rows []Row) error {
	err := w.store.append(rows)
	if err != nil {
		slog.Warn("costledger: append failed; cost history for this minute is lost",
			"error", err, "rows", len(rows))
	}
	return err
}

// Close flushes the open minute.
//
// There is no handle to release: append opens and closes the day file per call, so
// that a flush straddling local midnight cannot keep writing yesterday's file. So
// Close is the shutdown flush and nothing else — which is also why calling it is
// what turns "a restart loses up to 60 seconds" into "an orderly stop loses
// nothing".
func (w *Writer) Close() error { return w.Flush() }
