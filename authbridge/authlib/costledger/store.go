package costledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// dayLayout names a day file. Sortable, and the same layout Query parses back, so
// a human listing the directory reads the same dates the API serves.
const dayLayout = "2006-01-02"

// defaultRetentionDays is how many day files are kept.
//
// An active 8h day writes roughly 480 minutes x a few label combinations, about
// 350 KB, so 30 days is on the order of 10 MB — small enough that nobody has to
// think about it, long enough to answer "what did last month cost".
const defaultRetentionDays = 30

// fileMode is 0o600 because these files record spend. 0o644 would make one
// account's bill readable by every other account on a shared machine, and there is
// no reader that needs it.
const fileMode = 0o600

// dirMode is 0o700 for the same reason: a world-listable directory of day files
// discloses which days someone worked even before a file is read.
const dirMode = 0o700

// store is the on-disk half: one append-only JSON-lines file per local day,
// retention by deletion.
//
// One file per day rather than one per process or one growing forever, because
// retention then costs a directory listing and an unlink instead of a rewrite, and
// because Query walks the dates it was asked for rather than scanning everything
// the ledger has ever held.
type store struct {
	dir string
	// retainDays is how many day files survive prune. Zero means the default.
	retainDays int
	// loc is the zone the ledger's DAY BOUNDARY is defined in, and the single answer
	// to "which day does this instant belong to" for every part of this file.
	//
	// It is the Writer's clock zone, which is time.Local in production because every
	// producer and every reader derives its timestamps from time.Now(). It exists
	// because path() used to name the file from the ROW's own zone while readDay is
	// called with a day derived from the CALLER's, and the two agreed only by the
	// coincidence that nothing in the pipeline calls .UTC(). One that did would file a
	// row near midnight under a date no query for that local day ever visits: written,
	// retained for 30 days, and invisible to every read. Deciding it in one place makes
	// the agreement structural instead of a convention nobody wrote down.
	loc *time.Location
}

// newStore prepares dir, creating it if needed. A nil loc means time.Local.
func newStore(dir string, retainDays int, loc *time.Location) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("costledger: no directory configured")
	}
	if retainDays <= 0 {
		retainDays = defaultRetentionDays
	}
	if loc == nil {
		loc = time.Local
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("costledger: cannot create %s: %w", dir, err)
	}
	return &store{dir: dir, retainDays: retainDays, loc: loc}, nil
}

// dayOf is the package dayOf resolved in the STORE's zone. The one place the day
// boundary is decided, so path, readDay, prune and Query's day walk cannot disagree.
func (s *store) dayOf(t time.Time) time.Time {
	return dayOf(t.In(s.loc))
}

// path is the day file a timestamp belongs to.
//
// dayOf-derived rather than formatting t directly, so the name is explicitly the
// LEDGER DAY of t rather than whatever date t's own zone happens to print.
func (s *store) path(t time.Time) string {
	return filepath.Join(s.dir, s.dayOf(t).Format(dayLayout)+".jsonl")
}

// append writes rows to whichever day files they belong to.
//
// Grouped by day rather than assuming one, because a flush can straddle local
// midnight: the minute that closes at 00:00 belongs to yesterday's file while the
// one that opened belongs to today's. No long-lived handle is held for the same
// reason — a handle cached across a day boundary would keep writing yesterday's
// file forever, which is the bug that makes a day silently gain 24 hours of rows.
func (s *store) append(rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	byDay := map[string][]Row{}
	for _, r := range rows {
		p := s.path(r.At)
		byDay[p] = append(byDay[p], r)
	}
	// The first error is returned but every day is still attempted: a failure
	// writing one file is no reason to drop the rows destined for another.
	var firstErr error
	for p, dayRows := range byDay {
		if err := writeLines(p, dayRows); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// dayFile is what writeLinesTo needs of an open day file: append the bytes, get them
// onto the device, close.
//
// An interface rather than *os.File so the failure path is testable — ENOSPC is not
// something a unit test can arrange, and surviving one is the whole point of this being
// a single Write instead of N. See TestWriteLines_ATornAppendDoesNotRollBackAnotherWritersRows.
//
// DELIBERATELY NARROWER THAN *os.File: no Truncate, and no Stat to derive a size from.
// That absence IS the fix rather than an oversight — see appendBytes for why nothing on
// this path may ever shorten a day file — so keep both out of here and no future edit
// can quietly reintroduce the rollback.
type dayFile interface {
	Write(b []byte) (int, error)
	Sync() error
	Close() error
}

// writeLines appends one day's rows as JSON lines to the day file at path.
func writeLines(path string, rows []Row) error {
	return writeLinesTo(rows, func() (dayFile, error) {
		return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	})
}

// writeLinesTo is writeLines with the opening of the day file injected, so a test can
// put a file that fails part-way through a write where the real one goes.
//
// Marshalled in full FIRST and written ONCE, which is what keeps a day file
// syntactically intact under a failure. Encoding straight to the file, a row at a
// time, meant a short write — ENOSPC, EIO — left a fragment with no trailing
// newline, and the next successful append concatenated onto it: a guaranteed syntax
// error at that offset. readDay resyncs past one now, but not producing the damage
// beats tolerating it, and a laptop filling its disk is exactly when someone asks
// what things cost.
func writeLinesTo(rows []Row, open func() (dayFile, error)) error {
	var buf bytes.Buffer
	// json.Encoder writes one object per line and terminates each with a newline,
	// which is exactly the JSON-lines shape readDay decodes.
	enc := json.NewEncoder(&buf)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			// A Row cannot fail to marshal — no channels, no funcs, no NaN — so this is
			// unreachable in practice. Returned rather than skipped anyway: reaching it
			// would mean the schema gained a field JSON cannot express, and silently
			// dropping that day's rows is not how anyone should find out.
			return err
		}
	}

	f, err := open()
	if err != nil {
		return err
	}
	werr := appendBytes(f, buf.Bytes())
	if werr == nil {
		// FSYNCED, so a nil error means the bytes are on the device rather than in the
		// page cache. Without this, Writer.Close's whole reason for existing — "an orderly
		// stop loses nothing" — was false for a host that lost power seconds after the
		// stop, and the doc said otherwise. Softening the doc was the alternative; syncing
		// is better, because the promise is the thing callers use Flush and Close FOR.
		//
		// The cost is one fsync per closed minute, on the writer goroutine, never on a
		// request path. Reported rather than swallowed: a sync that fails is a durability
		// claim that cannot be made, and Flush and Close are the two callers that exist to
		// hear it.
		werr = f.Sync()
	}
	if cerr := f.Close(); cerr != nil && werr == nil {
		werr = cerr
	}
	return werr
}

// appendBytes writes b in ONE call and, if that write landed only partly, appends a
// single newline so the fragment cannot swallow whatever is appended next. IT NEVER
// SHORTENS THE FILE.
//
// It used to roll back. os.File.Write reports an error whenever it wrote fewer bytes
// than asked and the bytes it DID write are in the file, so this took the file's size
// before the write and Truncate'd back to it afterwards, on the reasoning that losing
// this minute beats leaving a corrupt line.
//
// THAT ROLLBACK COULD DESTROY ROWS THIS WRITER NEVER WROTE. The size was read before
// the write and used after it, and a day file is not private to one writer:
// ~/.cortex/cost is a fixed default that every proxy on the host opens, so a second
// proxy — a spare on another port, an overlapping restart — can append in that window,
// and Close's inline-write branch could do it from inside this process. Anything that
// landed in between sat inside the range being truncated away, so one bad minute took
// the rest of the day with it, and the file said nothing about it afterwards.
//
// A ROLLBACK THAT CAN BE WRONG IS WORSE THAN NO ROLLBACK, because of what the two
// failures cost. Not rolling back leaves one undecodable line: readDay steps over it and
// COUNTS it, and the count reaches a caller on Writer.SkippedLines, so the loss is
// bounded at that line and it is visible. Rolling back over another writer's rows
// deletes committed history with no error, no count and nothing left in the file to say
// it happened. Bounded and reported beats unbounded and silent, and that is the whole
// trade.
//
// Hence the newline. A torn write ends mid-row, and with no terminator the NEXT append
// concatenates onto that fragment and makes its first row unreadable too — so one byte
// fences the damage to the fragment alone. Best effort: the write that just tore will
// often refuse this too, and then the file is merely back to the bounded case above. It
// can only ever ADD a byte, which is what makes it safe to attempt on a file another
// writer has open.
//
// NO LOCK, and that is a decision rather than an omission. With nothing on this path
// that shortens a file, concurrent writers can only append: each flush is one write to
// an O_APPEND handle, so rows land whole and interleaved instead of over one another.
// See TestWriteLines_ConcurrentWritersDoNotLoseEachOthersRows. What two writers still
// cannot do is make each other's TOTALS right — two processes pricing the same traffic
// would double-count it — but that is a question about who may write a ledger, not
// about whether a write destroys what is already in it.
func appendBytes(f io.Writer, b []byte) error {
	n, err := f.Write(b)
	if err == nil {
		return nil
	}
	if n > 0 && n < len(b) && b[n-1] != '\n' {
		// Ended mid-row. A tear that happened to land on a line boundary needs nothing.
		if _, ferr := f.Write([]byte{'\n'}); ferr != nil {
			// Reported together with the write that tore, because the consequence outlives
			// this call: the next row appended to this file is unreadable too, and only the
			// skipped-line count will show it.
			return fmt.Errorf("costledger: torn append of %d bytes could not be fenced off with a "+
				"newline (%v), so the next row appended to this file will be unreadable too: %w", n, ferr, err)
		}
	}
	return err
}

// maxLineBytes bounds one line readDay will buffer.
//
// A row is a few hundred bytes, so 1 MiB is roughly three thousand times the real
// shape. It is deliberately not unbounded: this reads a path an operator configured,
// and a reader that will buffer a line of any length can be made to allocate
// arbitrarily by whatever else ends up in that directory.
//
// A line longer than this ENDS that day's read, with a warning, because a scanner
// cannot skip a token it refused to buffer. Every row appended after that offset is
// then unreadable, on this read and on every future one, and the file is append-only
// so the loss is permanent.
//
// AN EARLIER VERSION OF THIS COMMENT CLAIMED THAT WAS FINE, on the grounds that this
// is "damage of a kind no ledger write can produce — every row this package emits is
// one Encode of one struct". That was FALSE, and it was the premise the whole design
// rested on. One Encode of one struct is exactly how the damage was produced: Row
// carries Model, Model is the model name off the parsed request body, and it was
// written with no length cap — so a workload naming its model with a megabyte of
// bytes wrote a single valid line past this limit and destroyed the remainder of that
// day. Measured: 5 priced requests totalling $3.25 read back as $0.25.
//
// The write side now caps every label at maxLabelLen, which puts the longest line
// this package can emit under a kilobyte. So this guard is once again what the
// comment above wrongly assumed it already was — a last resort for a file corrupted
// by something other than this package — and NOT a live failure mode a request can
// reach. Keep it that way: any new Row field carrying caller-controlled bytes needs a
// cap on the write path, not a larger buffer here.
const maxLineBytes = 1 << 20

// readDay decodes one day file. A missing file is not an error: an idle day writes
// none, which is the normal case on a laptop.
//
// SKIPS an undecodable line and keeps going, rather than stopping at it. This used
// to drive one json.Decoder over the whole file and return what it had on the first
// error — which tolerates a truncated FINAL line, and only that, because a Decoder
// cannot resync. A bad line in the MIDDLE silently truncated the rest of the day,
// permanently, and the shortened figure was still labelled "today".
//
// That was reachable, not theoretical: before the write path became a single
// rolled-back Write, a short append left a fragment with no newline and the next
// append concatenated onto it, guaranteeing a syntax error mid-file. Both halves are
// fixed; this half is the one that keeps an already-damaged file readable.
//
// Skips are COUNTED, RETURNED and logged at Warn. An earlier version counted them
// into a local and logged that at slog.Debug — below the default level, so in
// production a day quietly losing lines was indistinguishable from a clean one, and
// the count reached no caller, no exported counter and no API response. The count is
// what lets an operator tell "my ledger is fine" from "my ledger is losing lines", so
// it has to leave this function. See dayIssues and Writer.SkippedLines.
func (s *store) readDay(day time.Time) ([]Row, dayIssues, error) {
	f, err := os.Open(s.path(day))
	if os.IsNotExist(err) {
		return nil, dayIssues{}, nil
	}
	if err != nil {
		return nil, dayIssues{}, err
	}
	defer func() { _ = f.Close() }()

	var out []Row
	var issues dayIssues
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		b := sc.Bytes()
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		var r Row
		if derr := json.Unmarshal(b, &r); derr != nil {
			// No offset in the message and no bytes from the line: a corrupt ledger line
			// could contain anything, and this text reaches a log an operator pastes.
			issues.skippedLines++
			continue
		}
		out = append(out, r)
	}
	if serr := sc.Err(); serr != nil {
		// Only ever an IO error or a line past maxLineBytes. Either ends the day's read,
		// so it is a warning rather than a debug line: the figure that follows is short
		// by however much came after this point, and by an amount the file cannot say.
		issues.truncated = true
		slog.Warn("costledger: stopped part-way through a day file; the total for it is short",
			"day", day.Format(dayLayout), "rowsRead", len(out),
			"linesSkipped", issues.skippedLines, "error", serr)
		return out, issues, nil
	}
	if issues.skippedLines > 0 {
		// WARN, not Debug. A number missing rows is exactly what an operator has to be
		// able to see, and the default level does not carry Debug.
		slog.Warn("costledger: skipped undecodable lines; the total for this day is short",
			"day", day.Format(dayLayout), "rowsRead", len(out), "linesSkipped", issues.skippedLines)
	}
	return out, issues, nil
}

// dayIssues is what one day file's read could not use.
//
// Returned rather than only logged, because the caller is what turns it into something
// an operator can see: Query publishes it on Writer.SkippedLines and
// Writer.TruncatedDays. Without that, a day file that lost half its lines produced the
// same API response as a clean one — window:"today", priced:true, no caveat.
type dayIssues struct {
	// skippedLines is undecodable lines stepped over. The rows around them survive, so
	// the loss is bounded by this count.
	skippedLines int
	// truncated reports that the read STOPPED before the end of the file. Everything
	// after that offset is missing from the answer and nothing says how much, which is
	// why it is tracked separately from a skip rather than added to it.
	truncated bool
}

// prune deletes day files older than the retention window, measured back from now's
// ledger day.
//
// retainDays FILES SURVIVE, counting today: the cutoff is today minus retainDays-1,
// so a 3-day retention keeps today and the two days before it. It used to be today
// minus retainDays, an inclusive range that kept retainDays+1 files — off by one
// against what the option and the config field both say the number means.
//
// Never touches a file it cannot date: an unrecognised name in the directory is
// left alone rather than deleted, because this runs against a path an operator
// configured and deleting something we do not understand is the one unrecoverable
// mistake available here.
//
// Returns the first error but keeps going, for the reason append does: one
// undeletable file must not leave the rest of the backlog in place.
//
// THE CUTOFF IS FLOORED AT THE NEWEST DAY FILE'S OWN DATE, which is what stops a
// wrong clock from deleting the ledger. It used to be today-minus-retention and
// nothing else, so a host whose clock STEPPED FORWARD past the window — NTP
// correcting after a resume, a restored VM image, a dead CMOS battery — put every
// existing file behind the cutoff and unlinked all of them, TODAY'S INCLUDED. This
// runs synchronously in New, i.e. at the one moment a laptop's clock is least
// trustworthy. See TestPrune_AForwardClockStepDoesNotDeleteTheLedger.
//
// Deleting nothing is always recoverable and deleting today is not, so where the two
// available readings of "how old is this file" disagree, the older reference wins and
// fewer files go. A file only ever goes when it is past the window under BOTH
// readings — the clock's day and the newest day the ledger itself has on disk.
//
// A FLOOR RATHER THAN A PLAUSIBILITY THRESHOLD because no threshold can separate the
// two cases. From the directory alone, "the clock jumped 40 days" and "this ledger was
// idle for 40 days" look identical, and one of them is a legitimate prune. The floor is
// sound either way: everything it still deletes is past the window relative to real
// recorded activity.
//
// The cost is that retention is measured from the ledger's newest DATA rather than
// from the clock, so an ARCHIVE nothing writes to any more keeps its last retainDays
// files instead of emptying out. The disk bound is unchanged — at most retainDays
// files survive, because they all have to sit within the window ending at the newest
// one — and the moment writing resumes, today becomes the newest day and the old era
// ages out normally.
func (s *store) prune(now time.Time) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	// Datable names only, and their days, so the newest is known BEFORE anything is
	// unlinked. Never touches a file it cannot date; see the doc above.
	type dayEntry struct {
		name string
		day  time.Time
	}
	var days []dayEntry
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".jsonl" {
			continue
		}
		// s.loc, not now's zone: the names were WRITTEN in s.loc, so that is the only
		// zone they can be read back in without the cutoff comparing two different days.
		day, perr := time.ParseInLocation(dayLayout, name[:len(name)-len(".jsonl")], s.loc)
		if perr != nil {
			continue
		}
		days = append(days, dayEntry{name: name, day: day})
		if day.After(newest) {
			newest = day
		}
	}
	if len(days) == 0 {
		return nil
	}

	// The day retention is counted back from: the OLDER of what the clock says today is
	// and the newest day the ledger has written. They are the same day on a healthy host,
	// so this changes nothing there.
	ref := s.dayOf(now)
	if newest.Before(ref) {
		ref = newest
		if gap := s.dayOf(now).Sub(newest); gap > time.Duration(s.retainDays-1)*24*time.Hour {
			// Worth a line: on a healthy host the newest day file IS today, so a gap wider
			// than the whole retention window means either the clock is wrong or the ledger
			// has not been written to in longer than it retains. Said at Warn because the
			// first of those is a host fault an operator wants to know about, and neither is
			// visible anywhere else.
			slog.Warn("costledger: the clock is further ahead of the newest day file than "+
				"retention could explain; keeping day files rather than deleting cost history",
				"clockDay", s.dayOf(now).Format(dayLayout), "newestDayFile", newest.Format(dayLayout),
				"retainDays", s.retainDays,
				"cause", "the host clock stepped forward, or this ledger has been idle longer than its retention",
				"effect", "retention is measured back from the newest day file instead of from the clock")
		}
	}
	cutoff := ref.AddDate(0, 0, -(s.retainDays - 1))

	var firstErr error
	for _, d := range days {
		if !d.day.Before(cutoff) {
			continue
		}
		if rerr := os.Remove(filepath.Join(s.dir, d.name)); rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}
	return firstErr
}
