package main

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/archive"
)

func historySeqs(t *testing.T, h *archiveHistory, id string) []uint64 {
	t.Helper()
	var seqs []uint64
	if err := h.Earlier(id, 0, func(e *pipeline.SessionEvent) bool {
		seqs = append(seqs, e.Seq)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return seqs
}

// Plugins hold the history from the first build, before the archive opens: it reads nothing
// until then, and the archive's events once it has.
func TestArchiveHistory_ReadsTheArchiveOnceItOpens(t *testing.T) {
	root := t.TempDir()
	recordTraffic(t, root, nil, time.Now())
	h := &archiveHistory{}
	if seqs := historySeqs(t, h, "a"); len(seqs) != 0 {
		t.Fatalf("before open: %v, want nothing", seqs)
	}

	arch, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = arch.Close() })
	h.open(arch)
	if seqs := historySeqs(t, h, "a"); len(seqs) != 4 || seqs[0] != 4 {
		t.Errorf("after open: %v, want session a's 4 events newest first", seqs)
	}
}
