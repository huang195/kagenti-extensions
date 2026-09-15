package costledger

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// opsBuffer is how many batches the writer goroutine can be behind by.
//
// A batch is one closed minute, so in steady state this is one send per minute and
// the buffer is never more than one deep. It is sized for the failure case instead: a
// filesystem that stops responding, where the queue absorbs the backlog rather than
// pushing back onto the request path. Past this depth rows are dropped and counted —
// see enqueue.
const opsBuffer = 1024

// dropNotifyDepth is the depth of the channel that wakes the writer goroutine to log
// a drop. One is enough: the NUMBER lives in w.dropped, so a wake that coalesces with
// another loses nothing and the goroutine reports the running total.
const dropNotifyDepth = 1

// defaultSettleInterval is how often the writer checks whether the held minute has
// ended. See settleClosedMinute.
//
// 30s rather than a minute so a minute settles within half a minute of ending, and
// rather than a second because nothing about cost history is worth a per-second
// wakeup. It bounds two things: how long an idle ledger holds its last minute in
// memory, and therefore how much an unclean kill loses.
const defaultSettleInterval = 30 * time.Second

// batch is one unit of work for the writer goroutine: rows to append, retention to
// enforce, or both.
type batch struct {
	rows []Row
	// pruneAt is non-zero when retention should be enforced back from that instant.
	pruneAt time.Time
	// done, when non-nil, receives the append error. Set by submit for the callers
	// that need to know the write landed — shutdown and tests.
	done chan error
}

// Writer accumulates the open minute in memory and appends closed minutes.
//
// Only CLOSED minutes reach disk. The open one stays in this writer's own
// accumulator, and Window stitches the two — so a minute never exists in both
// places and cannot be double-counted. The cost of that boundary is that a restart
// mid-minute loses up to 60 seconds of cost, which is documented rather than
// hidden: Flush on shutdown closes the gap for an orderly stop, and nothing can
// close it for a kill.
//
// NOTHING HERE TOUCHES DISK ON THE REQUEST PATH. Record is called inside
// session.Store.Append, under the store's write lock, with every other request in the
// proxy waiting behind it; it takes a mutex, folds into a map, and hands any IO to a
// single background goroutine over a buffered channel that drops rather than blocks.
// Constraint 5 — a ledger failure must never break the proxy — was previously true
// only for ledger FAILURE and only by discipline, since the per-minute append and the
// once-a-day prune both ran inline. It is now true for ledger LATENCY too, and by
// construction.
//
// It is NOT, however, constant work. An earlier version of this paragraph said "all
// it does is take a mutex, fold into a map", and that was wrong for the one call in
// sixty that closes a minute: that call also runs takeLocked, which walks the whole
// accumulator and allocates a slice of every row in it, under mu, inside the session
// store's write lock. The honest statement is that the work is BOUNDED — the
// accumulator holds at most maxLabelsPerMinute rows, so the walk is at most 64
// entries and the allocation at most 64 rows, once a minute. It is bounded because
// foldLocked caps it; before that cap the same walk was O(distinct labels), and the
// labels are request-chosen, so a caller could make one request in sixty pay for an
// arbitrarily large map (50,000 keys was measured). Anything added to this path has
// to keep that bound.
//
// The cost of the hand-off is a brief window in which a just-closed minute is in
// neither half: it has left the map and its batch has not yet been written. That is
// microseconds, and it self-heals on the next read. The alternative — keeping the
// in-flight batch visible to readers — cannot work, because a minute can be split
// across two batches (a whole minute from the roll, plus a late row afterwards) and
// disk rows carry nothing that says which batch they came from, so "memory wins for
// this minute" would drop the batch already written. Under-reporting for microseconds
// beats a rule that can double-count.
//
// OWNERSHIP, stated precisely because Window's correctness rests on it: while
// pending() reports a minute, NOTHING on disk carries that minute or a later one.
// Three write paths have to keep that true, and each is guarded below —
// flushLocked advances flushedThrough as it writes, and the two direct-append
// paths in add only ever write a minute strictly below the one being held.
// TestPendingMinute_IsNeverAlsoOnDisk drives all three and asserts it.
type Writer struct {
	store *store
	now   func() time.Time
	// retainDays is how many day files survive prune. Zero means the package
	// default; newStore owns that so the number is defined once.
	retainDays int
	// settle is the settleClosedMinute cadence. Zero disables it.
	settle time.Duration

	// ops carries work to the writer goroutine. Buffered; see opsBuffer.
	ops  chan batch
	quit chan struct{}
	wg   sync.WaitGroup
	// closed reports that the writer goroutine has STOPPED, so submit writes inline
	// and enqueue counts instead of queueing work nobody will collect.
	//
	// Set after wg.Wait, never before. It used to be set before close(quit), which
	// armed submit's inline-write branch while the goroutine was still draining: two
	// writeLines on one day file, each holding a size from its own Stat, so a partial
	// write in one rolled the file back over rows the other had just appended.
	closed atomic.Bool
	// closeOnce guards the shutdown, and closeErr carries its result to every later
	// caller. A shutdown path that retries Close must not be told nil the second time
	// after a real failure the first.
	closeOnce sync.Once
	closeErr  error
	// dropped counts rows that will never reach disk: a full queue, a failed append, a
	// send that raced Close. Read by Dropped.
	dropped atomic.Int64
	// dropNotify wakes the writer goroutine to LOG a drop. See drop.
	dropNotify chan struct{}
	// loggedDrops is the cumulative drop count already reported. Atomic only because
	// submit's post-Close inline path can reach write from a caller's goroutine.
	loggedDrops atomic.Int64
	// skippedLines and truncatedDays report what the MOST RECENT Query could not read.
	// See SkippedLines.
	skippedLines  atomic.Int64
	truncatedDays atomic.Int64

	mu sync.Mutex
	// open is the minute currently accumulating, truncated to the minute.
	open time.Time
	rows map[key]*Row
	// flushedThrough is the newest minute any of whose rows have reached disk.
	//
	// It exists so a minute is never HELD after part of it has been written. Without
	// it, a Flush (shutdown, or the periodic settle) followed by another event in the
	// same minute would leave that minute both on disk and in memory, and Window —
	// which counts the held minute from memory and everything else from disk — would
	// count the flushed part twice. Spend counted twice is worse than spend counted
	// late, so the guard is on the write side where it can be absolute rather than on
	// the read side where it would be a heuristic.
	flushedThrough time.Time
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

// WithSettleInterval overrides how often the writer checks whether the held minute
// has ended. Zero disables the check entirely, which leaves the next event and
// shutdown as the only triggers — for a test that wants no background writes at all.
func WithSettleInterval(d time.Duration) Option {
	return func(w *Writer) { w.settle = d }
}

// New opens a ledger under dir, creating it if needed, and starts its writer
// goroutine. Close stops it.
//
// Enforces retention once here, SYNCHRONOUSLY, before serving anything: a restart is
// the moment a long-idle ledger is most likely to be holding files past their window,
// and construction is a startup path where a directory listing costs nothing. Every
// later prune goes through the writer goroutine instead.
func New(dir string, opts ...Option) (*Writer, error) {
	w := &Writer{
		now:        time.Now,
		rows:       map[key]*Row{},
		settle:     defaultSettleInterval,
		ops:        make(chan batch, opsBuffer),
		quit:       make(chan struct{}),
		dropNotify: make(chan struct{}, dropNotifyDepth),
	}
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
	w.wg.Add(1)
	go w.run()
	return w, nil
}

// agentLabel is the ledger's STORAGE-side spelling of pipeline.EventClient.Label:
// the same label for a client that exists, and "" instead of "unknown" for one that
// does not.
//
// It exists so the durable file stays lossless. Label() answers "unknown" because it
// serves a display surface where a blank row reads as a bug; a file retained for days
// must not bake that display string into a field, where it becomes permanently
// indistinguishable from an agent that really did call itself that. labelFor puts the
// display string back at the query boundary, so nothing a client sees differs. See
// Row.Agent.
func agentLabel(c *pipeline.EventClient) string {
	if c == nil {
		return ""
	}
	// Label() can answer "unknown" for a NON-nil client too: an event built by hand
	// with an empty EventClient, and — reachable from off-host — a caller that sends
	// literally "User-Agent: unknown", which parses to Raw="unknown" with no name.
	// Both normalise back to "" so there is exactly ONE representation of absence on
	// disk, rather than a file that has to be read two ways.
	//
	// That folds a caller claiming to be "unknown" in with genuinely unattributed
	// traffic, which is the collision EventClient.Label documents and deliberately
	// does not defend against: the axis is spoofable by construction, so the same
	// caller could instead claim "claude-cli/2.1.14" and land in a real agent's row.
	// Normalising is the safe direction — it cannot fabricate a row for an agent that
	// does not exist — and it keeps the ledger's answer identical to the ring's, which
	// buckets that same request under "unknown" as well.
	if l := c.Label(); l != unknownAgentLabel {
		return l
	}
	return ""
}

// Record implements session.Recorder.
//
// Never returns an error and never touches disk: this runs on the synchronous
// session-append path, under the store's write lock, and the ledger is
// observability. Neither a failure nor a delay here may become a failed or a slow
// request. See the type doc for how the hand-off works.
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
		// TRUNCATED, both of them, and this is not cosmetic: Model is the model name
		// straight off the parsed request body and Host is set by whatever the workload
		// asked for, so their lengths are chosen off-host and both are written to an
		// append-only file. One over-long line permanently ends every future read of that
		// day at its offset. See maxLabelLen for the measurement and the arithmetic.
		//
		// Host is the event's field name; endpoint is what it means here. See Row.Endpoint.
		Endpoint: truncateLabel(e.Host),
		Model:    truncateLabel(e.Inference.Model),
		// The calling coding agent, and part of the row KEY — two agents hitting the
		// same endpoint and model in the same minute are two rows, not one, or a
		// per-agent breakdown could not be reconstructed from the file at all.
		//
		// agentLabel rather than Label() directly, because the ledger stores absence as
		// "" where the live aggregator displays it as "unknown". See Row.Agent for why
		// the two representations differ and why they still mean the same thing.
		//
		// Truncated as well. pipeline.maxClientLen already caps the retained User-Agent
		// at 128, so this is not the unbounded case Model is — but the ring truncates the
		// same label to maxLabelLen before it becomes a byAgent key, so cutting at 96 here
		// is what keeps group=agent spelled identically whichever half answers.
		Agent: truncateLabel(agentLabel(e.Client)),
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
		// Truncated for completeness rather than against a known threat: provenance is
		// authored by this process's own pricing code, not by a caller. It is the fourth
		// field of the row key, so leaving one of the four uncapped would leave the
		// line-length bound maxLabelLen states resting on a promise about a sibling
		// package instead of on the arithmetic.
		r.Provenance = truncateLabel(ev.Provenance)
		if ev.Priced() {
			r.CostMicros = ev.Micros()
			r.PricedRequests = 1
			// PRICED IMPLIES PRICEABLE, and it has to be said here because the test above
			// does not cover this case. A response that carries the gateway's own cost
			// header but no parsed token counts — a body-less response, which the proxy
			// now charges — has Tokens == 0, so the model-and-tokens test leaves
			// PriceableRequests at 0 while this branch sets PricedRequests to 1.
			//
			// That inverts the subset: every consumer computes coverage as
			// priceable-minus-priced (see abctl's `cost` command and its spend strip),
			// and a NEGATIVE gap fails their `> 0` test, so a day mixing these responses
			// with genuinely unpriced ones silently prints no coverage warning at all.
			// The caveat disappears exactly when there is something to caveat.
			//
			// The live aggregator never had the bug: usage.Aggregator.costOf sets priced
			// and priceable together in one literal. This keeps the ledger's arithmetic
			// identical to the ring's, which matters because /v1/usage answers from
			// whichever one the window selects.
			r.PriceableRequests = 1
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

// add folds one row into the open minute and hands off any work that touches disk.
//
// Everything here is memory: one mutex, one map operation, at most one bounded walk
// of the accumulator (only on the call that closes a minute — see closeMinuteLocked)
// and at most one non-blocking channel send. No open, no write, no directory listing,
// and no logging. That is the property that matters, because this runs inside
// session.Store.Append under the store's WRITE LOCK — every other request in the
// proxy is waiting behind it — and the store's own comment says a Recorder must be
// cheap. "Bounded" is doing real work in that sentence: it holds only because
// foldLocked caps the accumulator at maxLabelsPerMinute.
func (w *Writer) add(minute time.Time, r Row) {
	w.mu.Lock()
	var out batch
	switch {
	case !minute.After(w.flushedThrough):
		// This minute is already on disk, in whole or in part: a late event, or one
		// arriving after a Flush for the minute it is still in. Straight to the writer
		// rather than back into memory — holding a minute that is partly written is the
		// one state that would let Window count the written part twice. A second row for
		// the same minute is correct, because a reader sums every row for a timestamp
		// rather than assuming there is only one.
		out.rows = []Row{r}
	case w.open.IsZero():
		w.open = minute
		w.foldLocked(r)
	case minute.After(w.open):
		out = w.closeMinuteLocked(minute)
		w.foldLocked(r)
	case minute.Before(w.open):
		// A late event for a closed-but-never-flushed minute. Folding it into the open
		// minute would misdate it, and it is strictly below the held minute, so writing
		// it keeps the ownership rule intact.
		out.rows = []Row{r}
	default:
		w.foldLocked(r)
	}
	w.mu.Unlock()

	// Outside the lock, and never blocking. See enqueue.
	w.enqueue(out)
}

// foldLocked accumulates one row into the open minute, capping distinct rows at
// maxLabelsPerMinute. Caller holds mu.
//
// CARDINALITY IS BOUNDED HERE and nowhere else, so this is the only thing standing
// between a request-chosen model string and unbounded growth of both the map and the
// batch takeLocked copies out of it under the same lock. Past the cap a row folds
// into overflowKey rather than being dropped: coarsely-attributed spend is a worse
// answer than exact spend and a better one than missing spend, and a total that
// still adds up is what lets a client's "(other)" band reconcile against it.
func (w *Writer) foldLocked(r Row) {
	k := r.key()
	if cur, ok := w.rows[k]; ok {
		cur.Add(r.Counts)
		return
	}
	// Reserve the LAST slot for overflowKey, exactly as usage.addLabel does: switching
	// to it only once the map is already full would make the overflow row itself the
	// (cap+1)th entry, so the map would settle one over the bound it claims.
	if len(w.rows) >= maxLabelsPerMinute-1 {
		k = overflowKey
		if cur, ok := w.rows[k]; ok {
			cur.Add(r.Counts)
			return
		}
		r = overflow(r)
	}
	row := r
	w.rows[k] = &row
}

// closeMinuteLocked takes the open minute's rows, opens the next one, and returns
// the work that has to reach disk. Caller holds mu.
func (w *Writer) closeMinuteLocked(next time.Time) batch {
	b := w.takeLocked()
	w.open = next
	b.pruneAt = w.markPrunedLocked(next)
	return b
}

// takeLocked removes every held row and marks its minute as no longer wholly in
// memory. Caller holds mu.
//
// flushedThrough advances here rather than when the write lands, and the rows are
// dropped whether or not that write succeeds. Both are deliberate: a minute that has
// been handed to the writer must never be re-held, because a re-held minute could be
// written twice and nothing downstream can detect a double-counted minute. A minute
// lost to a failed write is visible as a gap; a minute counted twice is not.
func (w *Writer) takeLocked() batch {
	if len(w.rows) == 0 {
		return batch{}
	}
	out := make([]Row, 0, len(w.rows))
	for _, r := range w.rows {
		out = append(out, *r)
	}
	if w.open.After(w.flushedThrough) {
		w.flushedThrough = w.open
	}
	w.rows = map[key]*Row{}
	return batch{rows: out}
}

// markPrunedLocked returns the instant retention should be enforced for, or the zero
// time when it already has been for that local day. Caller holds mu.
//
// The bookkeeping is here, under the lock, and the ReadDir and the unlinks are not:
// this used to run store.prune inline, so on the first minute of each new local day
// one request paid a directory listing plus up to N os.Remove calls against an
// operator-configurable path while every other request in the proxy waited on the
// session store's write lock. On a slow or hung mount — NFS, FUSE, an encrypted
// volume spinning up — that is a stall in request handling caused by observability.
func (w *Writer) markPrunedLocked(at time.Time) time.Time {
	day := dayOf(at)
	if !day.After(w.prunedDay) {
		return time.Time{}
	}
	w.prunedDay = day
	return at
}

// pending returns a copy of the open minute's rows and the minute they belong to.
//
// This is the half of the ledger that is NOT on disk, and it exists because
// something has to supply it: for a window whose spend all happened in the current
// minute, the day files hold nothing at all, and a reader that saw only them would
// answer "cost unavailable" for a day with real spend sitting right here.
//
// COPIED, not aliased: the caller is an HTTP handler that will encode these while
// the next Record mutates the live map.
//
// The returned minute is the ownership boundary Window relies on — see the type
// doc. Zero, with no rows, when nothing is held.
func (w *Writer) pending() ([]Row, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.rows) == 0 {
		return nil, time.Time{}
	}
	out := make([]Row, 0, len(w.rows))
	for _, r := range w.rows {
		out = append(out, *r)
	}
	return out, w.open
}

// enqueue hands a batch to the writer goroutine, or DROPS it.
//
// Never blocks, and that is the whole point: this is reached from
// session.Store.Append under the store's write lock, so a blocking send would put a
// hung filesystem directly in the path of every proxied request. Constraint 5 — a
// ledger failure must never break the proxy — is then true by construction rather
// than by every future caller remembering to keep this path cheap.
//
// A DROP LOSES COST HISTORY, which the store's own comment about recorders warns
// against ("a dropped event silently skews a cumulative counter"). That warning is
// right for the usage aggregator, which is pure memory and cannot be slow, and it is
// the wrong trade here: the only way to fill a buffer this deep is a filesystem that
// has stopped keeping up, and the alternative to dropping a minute of cost history is
// stalling every request in the proxy until the disk comes back. Not silent, though —
// every drop is counted into Dropped(), warned about from the writer goroutine (see
// drop and logDrops, and note that the warning is NOT emitted here, on the request
// path) and totalled again at Close, because an undisclosed drop is how a total
// quietly becomes wrong.
func (w *Writer) enqueue(b batch) {
	if len(b.rows) == 0 && b.pruneAt.IsZero() {
		return
	}
	if w.closed.Load() {
		// The writer goroutine has GONE. A send here would land in a buffer nobody will
		// ever read again — measured at 1,024 rows parked in w.ops for the life of the
		// process while Dropped() answered 49, a 21x undercount that also made the
		// shutdown warning quote the wrong number. Counted as the loss it is.
		//
		// A send can still be lost in the window between this load and Close's final
		// drain-and-count: that is inherent to a non-blocking hand-off, and Record is on
		// the request path so it cannot take the lock that would close it. Close narrows
		// the window to the few instructions between its drain and its store, and a
		// caller recording after Close has returned lands here instead.
		w.drop(len(b.rows))
		return
	}
	select {
	case w.ops <- b:
	default:
		w.drop(len(b.rows))
	}
}

// drop counts rows that will never reach disk and asks the writer goroutine to say so.
//
// NO FORMATTING AND NO IO, because this is reached from Record — inside
// session.Store.Append's write lock, with every other request in the proxy waiting.
// The slog.Warn that used to be here ran exactly there: slog's default handler
// serializes on its own mutex and writes to stderr, so the drop path put a lock and a
// possibly-slow fd in front of every proxied request. That is the property this
// package claims to have removed, so the request path now stores a number and rings a
// bell, and logDrops does the rest on the writer goroutine.
func (w *Writer) drop(rows int) {
	if rows == 0 {
		return
	}
	w.dropped.Add(int64(rows))
	select {
	case w.dropNotify <- struct{}{}:
	default:
		// A wake is already pending. It will report the total including this drop.
	}
}

// logDrops reports the cumulative drop count, on the writer goroutine.
//
// Rate limiting is free here rather than counted: wakes coalesce into the depth-1
// dropNotify channel, so a hung mount shedding thousands of rows produces a handful of
// lines quoting the running total instead of one line per inference response. That
// replaces an explicit every-Nth test, which had to be evaluated on the request path
// to work at all.
func (w *Writer) logDrops() {
	n := w.dropped.Load()
	if n <= w.loggedDrops.Load() {
		return
	}
	w.loggedDrops.Store(n)
	slog.Warn("costledger: dropping cost rows",
		"droppedRows", n, "queueDepth", cap(w.ops),
		"cause", "the ledger directory is not keeping up, or the writer has stopped",
		"effect", "cost history is missing those minutes; the proxy is unaffected")
}

// Dropped is how many rows will never reach disk.
//
// THE ONLY EXPORTED "is my cost history complete" SIGNAL, which is why it counts every
// path that loses a row and not just the one it was named for: a full queue, an append
// that failed (ENOSPC, EROFS, EACCES — takeLocked has already emptied the map by then,
// so the minute exists nowhere else), a send that raced Close, and rows still held when
// Close returns. Each of those used to be logged and not counted, so the number
// answered 0 while a whole minute's spend was gone.
//
// Exported so a caller can surface it rather than leaving it in a log line nobody
// greps: a cost total assembled from a ledger that dropped rows is short by an unknown
// amount, and that is worth saying out loud.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// SkippedLines is how many lines the MOST RECENT Query or Window could not decode.
//
// A GAUGE, NOT A COUNTER, and deliberately: a day file with one corrupt line is
// re-read on every /v1/usage request, so a cumulative count would climb forever over
// one piece of damage and read as an escalating fault. The last read's figure answers
// the question a caller actually has — "is the number you just gave me complete".
//
// Skips were previously counted into a local and logged at slog.Debug, below the
// default level, so a day quietly losing lines was indistinguishable in production
// from a clean one.
func (w *Writer) SkippedLines() int64 { return w.skippedLines.Load() }

// TruncatedDays is how many day files the MOST RECENT Query or Window ABANDONED
// part-way through — an IO error, or a line past maxLineBytes that a scanner cannot
// step over.
//
// Worse than a skipped line by an unknown amount: everything after that offset is
// missing from the answer and the file gives no way to say how much. A non-zero value
// means the total just returned is short, and a caller that reports a figure without
// checking it is serving a truncated day as a complete one.
//
// Same gauge semantics as SkippedLines. NOT yet reflected in the /v1/usage response
// shape — that is authlib/sessionapi's to add, and until it does, a client reading
// only the JSON still cannot see this. Exported here so it is available rather than
// discarded.
func (w *Writer) TruncatedDays() int64 { return w.truncatedDays.Load() }

// run is the ONLY goroutine that touches the filesystem after construction.
//
// One writer means no lock is needed around the day files, and no request ever waits
// on one. It also means the settle tick below can write directly instead of queueing
// work to itself.
func (w *Writer) run() {
	defer w.wg.Done()

	var tick <-chan time.Time
	if w.settle > 0 {
		t := time.NewTicker(w.settle)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case b := <-w.ops:
			w.write(b)
		case <-w.dropNotify:
			// The drop itself was detected on the request path, which only counted it. The
			// log line belongs here, off that path entirely. See drop.
			w.logDrops()
		case <-tick:
			w.settleClosedMinute()
		case <-w.quit:
			// Drain before exiting so a batch enqueued during shutdown is not lost and no
			// submit is left waiting on its done channel. Close drains again afterwards,
			// which covers a send that lands after this loop sees an empty queue.
			for {
				select {
				case b := <-w.ops:
					w.write(b)
				default:
					return
				}
			}
		}
	}
}

// write performs one batch's IO. Runs on the writer goroutine, holding NO lock — the
// store touches only the filesystem, and holding mu across an append would put a
// hung mount back in front of every reader.
func (w *Writer) write(b batch) {
	var err error
	if len(b.rows) > 0 {
		if err = w.store.append(b.rows); err != nil {
			// COUNTED, not only logged. takeLocked advanced flushedThrough and emptied the
			// map before this ran — deliberately, because a re-held minute could be written
			// twice and nothing downstream can detect a double count — so these rows now
			// exist nowhere at all. An ENOSPC, an EROFS or an EACCES here is a permanent loss
			// of that minute, and Dropped() answered 0 over it.
			//
			// loggedDrops is advanced past this count because the line below already reports
			// it, with the error attached, which is more use than logDrops' bare total.
			n := w.dropped.Add(int64(len(b.rows)))
			w.loggedDrops.Store(n)
			slog.Warn("costledger: append failed; cost history for this minute is lost",
				"error", err, "rows", len(b.rows), "droppedRows", n)
		}
	}
	if !b.pruneAt.IsZero() {
		if perr := w.store.prune(b.pruneAt); perr != nil {
			slog.Warn("costledger: retention prune failed; old day files remain", "error", perr)
		}
	}
	if b.done != nil {
		b.done <- err
	}
}

// settleClosedMinute writes the held minute once it has ENDED, so an idle ledger
// settles instead of holding its last minute until the next request arrives.
//
// Without it the only triggers were the next event and shutdown, so a proxy that went
// quiet at 14:23 kept that minute in memory for as long as it stayed quiet — a kill
// would lose it, and every reader had to reach into memory to see it at all. It only
// ever writes a minute that is strictly in the past, which is what keeps it from
// creating the state Window forbids: the current minute is never written while it is
// still accumulating, so it cannot be in both halves.
func (w *Writer) settleClosedMinute() {
	now := w.now()
	w.mu.Lock()
	if len(w.rows) == 0 || w.open.IsZero() || !w.open.Before(now.Truncate(time.Minute)) {
		w.mu.Unlock()
		return
	}
	b := w.takeLocked()
	// A proxy idle across midnight rolls the day without recording anything, so
	// retention is enforced here too rather than waiting for traffic to resume.
	b.pruneAt = w.markPrunedLocked(now)
	w.mu.Unlock()
	w.write(b)
}

// Flush writes everything held in memory, including the minute still open.
//
// For shutdown and for tests, and synchronous in both: it hands the rows to the
// writer goroutine BEHIND everything already queued and waits for that batch to
// land, so "Flush returned" means "every row recorded before this call is on disk".
// Tests depend on that ordering, and so does the shutdown sequence in main.
//
// Returns the store's error so a shutdown path can log it, unlike Record, which has a
// request to serve and must not.
func (w *Writer) Flush() error {
	w.mu.Lock()
	b := w.takeLocked()
	w.mu.Unlock()
	return w.submit(b)
}

// sync blocks until every batch enqueued before this call has been written.
//
// A test barrier, and only that. It exists because Record now returns before its IO
// has happened, so a test that records and then reads the day files is asserting
// against a race rather than against behaviour. Production readers need no barrier:
// Window reads the in-memory half directly, and Flush already waits for its own batch.
//
// Deliberately NOT called from Window. A reader that waited for the writer's queue to
// drain would put a hung filesystem in front of /v1/usage, and the alternative it buys
// is closing a microsecond-wide gap that self-heals on the next read. See the type
// doc.
func (w *Writer) sync() error {
	if w.closed.Load() {
		return nil
	}
	done := make(chan error, 1)
	select {
	case w.ops <- batch{done: done}:
	case <-w.quit:
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-w.quit:
		return nil
	}
}

// submit sends a batch and waits for it. Blocking, unlike enqueue — every caller is
// a shutdown or a test, never a request.
func (w *Writer) submit(b batch) error {
	if len(b.rows) == 0 && b.pruneAt.IsZero() {
		return nil
	}
	done := make(chan error, 1)
	b.done = done
	if w.closed.Load() {
		// The goroutine has exited, so nothing else can be writing: do it here rather
		// than queue work nobody will pick up.
		w.write(b)
		return <-done
	}
	select {
	case w.ops <- b:
	case <-w.quit:
		w.write(b)
		return <-done
	}
	select {
	case err := <-done:
		return err
	case <-w.quit:
		// Only reachable when Flush runs concurrently with Close, which the shutdown
		// path does not do. Reported rather than retried: the batch may already be
		// mid-write, and writing it a second time would double-count that minute.
		return errClosedWhileFlushing
	}
}

// errClosedWhileFlushing reports a Flush that could not be confirmed because Close
// took the writer away underneath it. A fixed string; it names the race rather than
// pretending the flush succeeded.
var errClosedWhileFlushing = errors.New("costledger: closed while flushing; the last batch may not have landed")

// Close flushes what is held and stops the writer goroutine.
//
// There is no handle to release: append opens and closes the day file per call, so
// that a flush straddling local midnight cannot keep writing yesterday's file. So
// Close is the shutdown flush plus the goroutine's stop — which is also why calling
// it is what turns "a restart loses up to 60 seconds" into "an orderly stop loses
// nothing".
//
// Idempotent, and safe to call on a Writer whose goroutine has already gone. Every
// call after the first returns the FIRST call's error rather than nil: a shutdown path
// that retries Close must not be told the flush succeeded because it already ran.
func (w *Writer) Close() error {
	w.closeOnce.Do(func() {
		// Before the goroutine stops, so this batch is ordered behind everything already
		// queued rather than racing the drain.
		w.closeErr = w.Flush()
		close(w.quit)
		w.wg.Wait()

		// The goroutine is gone and nothing else writes, so anything still queued —
		// a Record that raced this shutdown — is written here.
		w.drainAndWrite()

		// AFTER wg.Wait AND after the drain above. Setting it earlier armed submit's
		// inline-write branch while the goroutine was still draining, so two writeLines
		// could run on one day file, each with a size from its own Stat — and a partial
		// write in either rolled the file back over the other's rows.
		w.closed.Store(true)

		// From here nothing can reach disk, so whatever is left is LOST and has to be
		// counted: a batch that landed in ops between the drain and the store above, and
		// any row a racing Record folded back into the accumulator after Flush emptied it.
		// Both were previously invisible — the first was the 21x undercount, and this
		// warning quoted the wrong total because of it.
		if lost := w.abandon(); lost > 0 {
			w.dropped.Add(lost)
		}
		if n := w.dropped.Load(); n > 0 {
			slog.Warn("costledger: rows were dropped during this run; the cost history is short",
				"droppedRows", n)
		}
	})
	return w.closeErr
}

// drainAndWrite writes every batch currently queued and returns when the queue is
// empty.
//
// The writer goroutine must already have STOPPED. Two goroutines in writeLines on one
// day file each take their rollback size from their own Stat, so a partial write in
// one truncates away rows the other appended.
func (w *Writer) drainAndWrite() {
	for {
		select {
		case b := <-w.ops:
			w.write(b)
		default:
			return
		}
	}
}

// abandon counts every row that can no longer reach disk and empties both places one
// can be: the queue and the accumulator. For Close, after w.closed is set.
//
// The accumulator is cleared rather than left in place so the rows are counted exactly
// once — a later Flush on a closed Writer still works, and must write only what was
// recorded after this point.
func (w *Writer) abandon() int64 {
	var lost int64
	for {
		select {
		case b := <-w.ops:
			lost += int64(len(b.rows))
		default:
			w.mu.Lock()
			lost += int64(len(w.rows))
			w.rows = map[key]*Row{}
			w.mu.Unlock()
			return lost
		}
	}
}
