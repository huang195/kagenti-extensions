package costledger

import (
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

// writeLines appends one day's rows as JSON lines.
func writeLines(path string, rows []Row) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	// json.Encoder writes one object per line and terminates each with a newline,
	// which is exactly the JSON-lines shape readDay decodes.
	enc := json.NewEncoder(f)
	var encErr error
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			encErr = err
			break
		}
	}
	if err := f.Close(); err != nil && encErr == nil {
		encErr = err
	}
	return encErr
}

// readDay decodes one day file. A missing file is not an error: an idle day writes
// none, which is the normal case on a laptop.
//
// A decode failure stops reading THAT file and returns what was read so far rather
// than failing the query. A truncated final line is the expected outcome of a crash
// mid-append, and discarding a whole day because its last line is half-written
// would turn a 60-second gap into a 24-hour one.
func (s *store) readDay(day time.Time) ([]Row, error) {
	f, err := os.Open(s.path(day))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []Row
	dec := json.NewDecoder(f)
	for {
		var r Row
		if err := dec.Decode(&r); err != nil {
			if err != io.EOF {
				slog.Debug("costledger: stopping at an undecodable line",
					"day", day.Format(dayLayout), "rowsRead", len(out), "error", err)
			}
			return out, nil
		}
		out = append(out, r)
	}
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
