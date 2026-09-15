package costledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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
}

// newStore prepares dir, creating it if needed.
func newStore(dir string, retainDays int) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("costledger: no directory configured")
	}
	if retainDays <= 0 {
		retainDays = defaultRetentionDays
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("costledger: cannot create %s: %w", dir, err)
	}
	return &store{dir: dir, retainDays: retainDays}, nil
}

// path is the day file a timestamp belongs to.
func (s *store) path(t time.Time) string {
	return filepath.Join(s.dir, t.Format(dayLayout)+".jsonl")
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

// appendTarget is what appendBytes needs of a day file: append the bytes, and undo
// them if the append was partial.
//
// An interface rather than *os.File so the recovery path is testable — ENOSPC is
// not something a unit test can arrange, and the recovery is the whole point of
// this being one Write instead of N. See TestAppendBytes_ShortWriteIsRolledBack.
type appendTarget interface {
	Write([]byte) (int, error)
	Truncate(int64) error
}

// writeLines appends one day's rows as JSON lines.
//
// Marshalled in full FIRST and written ONCE, which is what keeps a day file
// syntactically intact under a failure. Encoding straight to the file, a row at a
// time, meant a short write — ENOSPC, EIO — left a fragment with no trailing
// newline, and the next successful append concatenated onto it: a guaranteed syntax
// error at that offset. readDay resyncs past one now, but not producing the damage
// beats tolerating it, and a laptop filling its disk is exactly when someone asks
// what things cost.
func writeLines(path string, rows []Row) error {
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

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	// The size to roll back TO. Taken from the handle rather than from a Stat on the
	// path so a concurrent rename cannot make it describe a different file.
	var size int64
	if info, serr := f.Stat(); serr == nil {
		size = info.Size()
	}
	werr := appendBytes(f, size, buf.Bytes())
	if cerr := f.Close(); cerr != nil && werr == nil {
		werr = cerr
	}
	return werr
}

// appendBytes writes b in one call and rolls the file back to size if that write
// was partial.
//
// os.File.Write reports an error whenever it wrote fewer bytes than asked, and on a
// full disk the bytes it DID write are in the file. Truncating back to where the
// file started leaves it exactly as it was — this minute's cost is lost either way,
// and the choice is only whether the loss is one minute or the rest of the day.
func appendBytes(f appendTarget, size int64, b []byte) error {
	n, err := f.Write(b)
	if err == nil {
		return nil
	}
	if n > 0 {
		if terr := f.Truncate(size); terr != nil {
			// Nothing further to do: the caller logs, and readDay will resync past the
			// fragment. Reported as the primary error because a file left mid-row is worse
			// news than the write that failed.
			return fmt.Errorf("costledger: partial write of %d bytes could not be rolled back: %w", n, terr)
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

// prune deletes day files older than the retention window, measured back from
// now's LOCAL day.
//
// Never touches a file it cannot date: an unrecognised name in the directory is
// left alone rather than deleted, because this runs against a path an operator
// configured and deleting something we do not understand is the one unrecoverable
// mistake available here.
//
// Returns the first error but keeps going, for the reason append does: one
// undeletable file must not leave the rest of the backlog in place.
func (s *store) prune(now time.Time) error {
	cutoff := dayOf(now).AddDate(0, 0, -s.retainDays)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".jsonl" {
			continue
		}
		day, perr := time.ParseInLocation(dayLayout, name[:len(name)-len(".jsonl")], now.Location())
		if perr != nil {
			continue
		}
		if !day.Before(cutoff) {
			continue
		}
		if rerr := os.Remove(filepath.Join(s.dir, name)); rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}
	return firstErr
}
