package tui

import (
	"fmt"
	"slices"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// `/` SEARCHES, AS IN A TEXT EDITOR (#1339): every row stays on screen, the matching rows are
// marked, and the cursor jumps between them. It replaced a filter that hid every row it did not
// match. That filter was saved to the config file, so one forgotten in an earlier run made
// sessions look missing in the next; and the sessions and events panes shared it, so a filter
// typed on one matched nothing on the other it followed the operator into. A search is neither
// saved nor shared: the sessions and events panes each keep their own, and nothing else
// searches. The events pane's is for one session's events, so opening another session's drops
// it (see the drill-in in keys.go); backing out and re-entering the same session keeps it.
//
// What a row matches is unchanged from the filter: sessionMatchesSearch on the sessions pane,
// matchEventRow on the events pane — including its `deny` and `plugin:<name>` forms.

// newSearchInput is the `/` prompt both constructors build.
func newSearchInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "/ "
	return ti
}

// searchable reports whether pane p has a search: the two panes whose rows the match functions
// know how to read.
func searchable(p paneID) bool { return p == paneSessions || p == paneEvents }

// searchPlaceholder names what `/` matches on pane p (#867).
func searchPlaceholder(p paneID) string {
	switch p {
	case paneSessions:
		return "search SESSION, TITLE, or AGENT…"
	case paneEvents:
		return "search PLUGIN, METHOD, HOST, etc.…"
	}
	return "search…"
}

// searchSpot is a row by identity — a session id, or an event's key — with the row number it
// had, for when that identity is gone. Rows move under a held position: the sessions list
// re-sorts every second and the events pane evicts its oldest rows, so a bare row number would
// put Esc somewhere other than where `/` was pressed.
//
// tail is an events cursor that was following new events: on the newest row of a chronological
// table, the predicate rebuildEventsTable tail-follows on. That cursor's place is "the newest
// row" rather than the event that was newest at `/` — events keep streaming while the prompt
// is open, and putting it back on that event would stop it following them.
type searchSpot struct {
	row   int
	sess  string
	event eventKey
	tail  bool
}

// searchQuery is the query pane p's rows are matched against: what is in the prompt while it is
// open on p, which is what makes the search incremental, and the committed one otherwise.
func (m *model) searchQuery(p paneID) string {
	if m.searching && p == m.searchPane {
		return m.searchInput.Value()
	}
	return m.search[p]
}

// searchMatches is pane p's matching rows, ascending, as the last rebuild found them.
func (m *model) searchMatches(p paneID) []int {
	switch p {
	case paneSessions:
		return m.sessionMatches
	case paneEvents:
		return m.eventMatches
	}
	return nil
}

// searchCursor is where pane p's cursor is.
func (m *model) searchCursor(p paneID) searchSpot {
	switch p {
	case paneSessions:
		c := m.sessionsTbl.Cursor()
		s := searchSpot{row: c}
		if c >= 0 && c < len(m.sessionRowIDs) {
			s.sess = m.sessionRowIDs[c]
		}
		return s
	case paneEvents:
		c := m.eventsTbl.Cursor()
		return searchSpot{
			row: c, event: keyOf(m.selectedEvent()),
			tail: m.sortCol == "" && c >= len(m.eventsTbl.Rows())-1,
		}
	}
	return searchSpot{}
}

// searchRow is the row s is on now: its identity's row if that is still listed, and the row
// number it had otherwise. setCursorVisible clamps a row number the rows have shrunk past.
func (m *model) searchRow(p paneID, s searchSpot) int {
	switch p {
	case paneSessions:
		if s.sess != "" {
			if i := slices.Index(m.sessionRowIDs, s.sess); i >= 0 {
				return i
			}
		}
	case paneEvents:
		if s.event != (eventKey{}) {
			if i := findByKey(m.visibleRows, s.event); i >= 0 {
				return i
			}
		}
	}
	return s.row
}

// searchHome is where the cursor goes when the search does not place it — Esc, an empty box, a
// query that matches nothing: the row s is on, or the newest row when s was following new
// events, so that it goes on following them.
func (m *model) searchHome(p paneID, s searchSpot) int {
	if s.tail {
		return len(m.visibleRows) - 1
	}
	return m.searchRow(p, s)
}

// moveSearchCursor puts pane p's cursor on row i. On the events pane the event is pinned as well,
// as every other cursor move there does, so the next rebuild keeps the cursor on it — through an
// eviction or a re-sort — instead of on its old row number.
func (m *model) moveSearchCursor(p paneID, i int) {
	switch p {
	case paneSessions:
		setCursorVisible(&m.sessionsTbl, i)
	case paneEvents:
		setCursorVisible(&m.eventsTbl, i)
		m.selectedEventKey = keyOf(m.selectedEvent())
	}
}

// rebuildSearched rebuilds pane p's table, which is what re-matches and re-marks its rows.
func (m *model) rebuildSearched(p paneID) {
	switch p {
	case paneSessions:
		m.rebuildSessionsTable()
	case paneEvents:
		m.rebuildEventsTable()
	}
}

// openSearch is `/`. The prompt opens empty, and the row the cursor is on is remembered: it is
// the row typing searches from, and the row Esc returns to.
func (m *model) openSearch() {
	m.searching = true
	m.searchPane = m.pane
	m.searchOrigin = m.searchCursor(m.pane)
	m.searchInput.SetValue("")
	m.searchInput.Placeholder = searchPlaceholder(m.pane)
	m.searchInput.Focus()
	// The prompt takes a body line, so the height budget changes — see layout(). layout also
	// rebuilds both tables, which clears the committed search's marks while the box is empty.
	m.layout()
}

// closeSearch closes the prompt. layout gives the body back the prompt's line and rebuilds both
// tables, so the rows are marked for the committed search again.
func (m *model) closeSearch() {
	m.searching = false
	m.searchInput.Blur()
	m.layout()
}

// searchKey handles a key while the prompt is open. Enter keeps the cursor where the search put
// it and commits the query; an empty one clears the search. Esc puts back the cursor and the
// search as they were at `/`. Anything else edits the query, and the cursor jumps to the first
// match at or after the row `/` was pressed on, wrapping past the last row — or back to that
// row when nothing matches. "Back" is searchHome, so a cursor that was following new events
// is still following them.
func (m *model) searchKey(msg tea.KeyMsg) tea.Cmd {
	p := m.searchPane
	switch msg.String() {
	case "enter":
		if m.search == nil {
			m.search = make(map[paneID]string, 2)
		}
		m.search[p] = m.searchInput.Value()
		m.closeSearch()
		return nil
	case "esc":
		m.closeSearch()
		m.moveSearchCursor(p, m.searchHome(p, m.searchOrigin))
		return nil
	}
	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	m.rebuildSearched(p)
	target := m.searchHome(p, m.searchOrigin)
	if m.searchInput.Value() != "" {
		// Counted from the event `/` was pressed on even when following: what streamed in since
		// is after it, and is searched first.
		if i, ok := nextMatch(m.searchMatches(p), m.searchRow(p, m.searchOrigin), true, true); ok {
			target = i
		}
	}
	m.moveSearchCursor(p, target)
	return cmd
}

// searchStep is n (forward) and N: the next match past the cursor, wrapping. Nothing happens
// with no match, which includes having no search: an empty query matches no row.
func (m *model) searchStep(forward bool) {
	p := m.pane
	if i, ok := nextMatch(m.searchMatches(p), m.searchCursor(p).row, forward, false); ok {
		m.moveSearchCursor(p, i)
	}
}

// nextMatch is the first of matches (ascending rows) after from going forward, or before it going
// back, wrapping past either end. inclusive lets from itself count.
func nextMatch(matches []int, from int, forward, inclusive bool) (int, bool) {
	if len(matches) == 0 {
		return 0, false
	}
	if forward {
		for _, i := range matches {
			if i > from || (inclusive && i == from) {
				return i, true
			}
		}
		return matches[0], true
	}
	for k := len(matches) - 1; k >= 0; k-- {
		if i := matches[k]; i < from || (inclusive && i == from) {
			return i, true
		}
	}
	return matches[len(matches)-1], true
}

// searchStatus is the footer's account of the active pane's search: which match the cursor is
// on, how many there are when it is on none, or that there are none. Live while the prompt is
// open, so a query that matches nothing says so as it is typed rather than after Enter. Empty
// without a search.
func (m *model) searchStatus() string {
	p := m.pane
	if !searchable(p) {
		return ""
	}
	q := m.searchQuery(p)
	if q == "" {
		return ""
	}
	matches := m.searchMatches(p)
	if len(matches) == 0 {
		return fmt.Sprintf("[/%s: no match]", q)
	}
	if k, ok := slices.BinarySearch(matches, m.searchCursor(p).row); ok {
		return fmt.Sprintf("[/%s %d/%d]", q, k+1, len(matches))
	}
	if len(matches) == 1 {
		return fmt.Sprintf("[/%s 1 match]", q)
	}
	return fmt.Sprintf("[/%s %d matches]", q, len(matches))
}

// searchHints is the hint line's `/`, with n/N beside it only while the active pane has a
// committed search: they do nothing without one, and the line is short of room.
func (m *model) searchHints() string {
	if m.search[m.pane] != "" {
		return "[/] search  [n/N] next/prev"
	}
	return "[/] search"
}
