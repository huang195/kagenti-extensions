package tui

import (
	"slices"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// surface is target t's search, or nil for a target with nothing to search.
func (m *model) surface(t paneID) searchSurface {
	switch t {
	case paneSessions:
		return sessionsSurface{m}
	case paneEvents:
		return eventsSurface{m}
	case panePipeline:
		return tableSurface{m, t, &m.pipelineTbl}
	case paneCatalog:
		return tableSurface{m, t, &m.catalogTbl}
	case paneAgents:
		return tableSurface{m, t, &m.agentsTbl}
	case paneNamespaces:
		return tableSurface{m, t, &m.namespacesTbl}
	case panePods:
		return tableSurface{m, t, &m.podsTbl}
	}
	return nil
}

// tableSurface is a table pane whose rows already hold their whole values: the table matches
// the rows as they are, and the cursor is the place.
type tableSurface struct {
	m   *model
	t   paneID
	tbl *table.Model
}

func (s tableSurface) matchLines() []int        { return s.tbl.Matches() }
func (s tableSurface) spot() searchSpot         { return searchSpot{row: s.tbl.Cursor()} }
func (s tableSurface) lineOf(sp searchSpot) int { return sp.row }
func (s tableSurface) reveal(i int)             { setCursorVisible(s.tbl, i) }
func (s tableSurface) restore(sp searchSpot)    { setCursorVisible(s.tbl, sp.row) }
func (s tableSurface) redraw()                  { s.tbl.SetSearch(s.m.searchQuery(s.t), nil) }

// sessionsSurface holds its place by session id: the list re-sorts under the cursor.
type sessionsSurface struct{ m *model }

func (s sessionsSurface) matchLines() []int { return s.m.sessionsTbl.Matches() }

func (s sessionsSurface) spot() searchSpot {
	c := s.m.sessionsTbl.Cursor()
	sp := searchSpot{row: c}
	if c >= 0 && c < len(s.m.sessionRowIDs) {
		sp.sess = s.m.sessionRowIDs[c]
	}
	return sp
}

func (s sessionsSurface) lineOf(sp searchSpot) int {
	if sp.sess != "" {
		if i := slices.Index(s.m.sessionRowIDs, sp.sess); i >= 0 {
			return i
		}
	}
	return sp.row
}

func (s sessionsSurface) reveal(i int)          { setCursorVisible(&s.m.sessionsTbl, i) }
func (s sessionsSurface) restore(sp searchSpot) { s.reveal(s.lineOf(sp)) }
func (s sessionsSurface) redraw()               { s.m.rebuildSessionsTable() }

// eventsSurface holds its place by event key, and by "the newest row" while following.
type eventsSurface struct{ m *model }

func (s eventsSurface) matchLines() []int { return s.m.eventsTbl.Matches() }

func (s eventsSurface) spot() searchSpot {
	c := s.m.eventsTbl.Cursor()
	return searchSpot{
		row: c, event: keyOf(s.m.selectedEvent()),
		tail: s.m.sortCol == "" && c >= len(s.m.eventsTbl.Rows())-1,
	}
}

func (s eventsSurface) lineOf(sp searchSpot) int {
	if sp.event != (eventKey{}) {
		if i := findByKey(s.m.visibleRows, sp.event); i >= 0 {
			return i
		}
	}
	return sp.row
}

// reveal pins the event as well, as every other cursor move here does, so the next rebuild
// keeps the cursor on it — through an eviction or a re-sort — instead of on its row number.
func (s eventsSurface) reveal(i int) {
	setCursorVisible(&s.m.eventsTbl, i)
	s.m.selectedEventKey = keyOf(s.m.selectedEvent())
}

// restore puts a following cursor back on the newest row, so it goes on following.
func (s eventsSurface) restore(sp searchSpot) {
	if sp.tail {
		s.reveal(len(s.m.visibleRows) - 1)
		return
	}
	s.reveal(s.lineOf(sp))
}

func (s eventsSurface) redraw() { s.m.rebuildEventsTable() }
