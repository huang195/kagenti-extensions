package main

import (
	"sync/atomic"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/archive"
)

// archiveHistory is the session archive as plugins read it (session.History). The first
// pipeline build runs before the archive opens, so every build is handed this instead, and it
// reads the archive once open sets it; until then, and with no archive, it reads nothing.
type archiveHistory struct {
	archive atomic.Pointer[archive.Archive]
}

func (h *archiveHistory) open(a *archive.Archive) { h.archive.Store(a) }

func (h *archiveHistory) Earlier(id string, before uint64, fn func(*pipeline.SessionEvent) bool) error {
	a := h.archive.Load()
	if a == nil {
		return nil
	}
	return a.Earlier(id, before, fn)
}
