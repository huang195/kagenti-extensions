package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// sessionsColumns is the table's full-width column set, before any terminal-fitting.
//
// A function rather than a package var so layout() can re-fit from the originals on every
// resize: fitting the LIVE columns would be cumulative, and a terminal that got narrower once
// would keep its narrowed columns after being widened again.
//
// COST is INSERTED before ACTIVE rather than appended after it, because ACTIVE is a
// status marker and belongs at the end of a row where the eye stops. table.Row is a
// []string addressed positionally, so that insert shifted every reader of ACTIVE by
// one — selectedSessionID takes [0] and is unaffected, but a retention test pinned
// "cached" at [4] and now reads [5]. That shift COMPILES and keeps asserting; it just
// asserts about COST instead. Both were moved deliberately; if a third positional
// reader appears, move it in the same commit as any reordering.
//
// The declared widths sum to 90, so the table renders 102 display columns: six columns,
// each charged cellPadding for the space bubbles puts either side of a cell. That is past
// an 80-column terminal, which is why layout() runs this set through fitTableColumns.
//
// What fitTableColumns does is SHRINK the widest column one cell at a time until the total
// fits; it never DROPS a column. So the fitted set always has six entries in this order,
// and the positional trap above stays closed at every terminal width — but the burden of
// not clipping moves onto the CELL, because a column can arrive narrower than its content.
// Measured, fitTableColumns leaves COST 5 columns wide on a 40-column terminal, 7 at 50, 8
// at 60, and only the declared 10 from 70 up. bubbles renders each cell as
// runewidth.Truncate(value, col.Width, "…"), so a 5-wide COST column turns $1234.5678 into
// "$123…" — a figure short by an order of magnitude wearing an ellipsis narrow enough to
// miss. fitCostCell is what stops that; see its comment for what a too-narrow cell says
// instead. TestSessionsTable_CostCellIsNeverATruncatedNumber exercises the fitter's OUTPUT
// at those widths, which is the check that was missing while only the DECLARED widths were
// pinned.
//
// SPAN (UpdatedAt-CreatedAt) was added and then removed WITHIN this branch. It never
// existed on main, so a diff against upstream will not show it going, and nothing outside
// this branch ever depended on it. Two reasons it went, and neither of them was the fitter
// — the fitter drops nothing. It measured ELAPSED time, so a session idle for an hour
// reported an hour, which made it nearly redundant with UPDATED two columns to its left;
// and it was always blank on cached-only rows, which have no CreatedAt to subtract from. It
// cost width on every terminal and said something on few.
func sessionsColumns() []table.Column {
	return []table.Column{
		{Title: "ID", Width: 40},
		{Title: "UPDATED", Width: 14},
		{Title: "EVENTS", Width: 8},
		{Title: "TOKENS", Width: 10},
		// 10 columns is exactly "$9999.9999", the widest figure formatUSDCell's fixed 4dp
		// produces below five figures of dollars. Above that bubbles truncates with
		// runewidth.Truncate, putting an ellipsis inside a dollar amount — the one thing
		// #953 rules out. The ceiling means one session spending $10,000 inside the
		// strip's window.
		//
		// This is the DECLARED width, which the table only gets from a 70-column terminal
		// up; below that fitTableColumns shrinks it and fitCostCell elides rather than let
		// bubbles clip. Pinned at both levels — TestSessionsTable_CostFitsItsColumn on the
		// declaration, TestSessionsTable_CostCellIsNeverATruncatedNumber on the fitted
		// widths — so the trade is visible if the formatter's precision changes.
		{Title: "COST", Width: 10},
		{Title: "ACTIVE", Width: 8},
	}
}

// costElision is what a COST cell says when the column it landed in is too narrow to hold
// the whole figure.
//
// A lone ellipsis, and deliberately NOT blank. Blank already means "unpriced / unknown"
// everywhere in this table — sessionCost's contract, the cached-only rows, and three tests
// that pin it — so reusing it here would collapse "nobody knows what this cost" into "the
// terminal is too narrow to say it". They are different truths and a cell that conflates
// them is the same class of lie as "$0.00" for an unknown cost. The ellipsis asserts
// neither: it says a figure exists and did not fit, which no reader can mistake for an
// amount.
//
// One display column wide, so it fits any column fitTableColumns can produce
// (minColumnWidth is 4).
const costElision = "…"

// fitCostCell renders a priced session's cost for a column exactly width cells wide,
// eliding rather than letting bubbles clip it.
//
// bubbles' renderRow is runewidth.Truncate(value, col.Width, "…"), which cuts a money
// figure from the RIGHT and keeps its LEADING digits: $1234.5678 in the 5-wide column a
// 40-column terminal leaves comes out "$123…", an amount ten times smaller that still reads
// as an amount. #953's rule is that no number is ever half-rendered, and a figure wrong by
// an order of magnitude is the worst case of it.
//
// Value-aware rather than a width threshold, because formatUSDCell is variable-width once
// the "$" and the "<" floor are counted: $0.1200 is 7 cells and states itself in full in
// the 7-wide column a 50-column terminal leaves, while $1234.5678 needs 10 and cannot. A
// blanket "elide below 10" would hide the common small figure on every narrow terminal for
// nothing.
//
// lipgloss.Width, never len() and never a rune count: it measures DISPLAY COLUMNS, which is
// what runewidth.Truncate compares against. footer.go:88-92 records what the other two cost.
//
// width <= 0 means the caller found no such column — a table whose columns have not been
// fitted yet. Render the figure: the declared width is 10 and holds everything below five
// figures of dollars.
func fitCostCell(usd float64, width int) string {
	cell := formatUSDCell(usd)
	if width > 0 && lipgloss.Width(cell) > width {
		return costElision
	}
	return cell
}

// fittedSessionsColumnWidth reports the width the named sessions column is CURRENTLY set
// to, which after layout() is fitTableColumns' output rather than sessionsColumns'
// declaration. 0 when there is no such column.
//
// Read from the live table at row-build time rather than computed here, so there is exactly
// one fitter and the cell measures itself against what the renderer will actually use.
//
// The two run on different messages, and that leaves a known seam: layout() re-fits the
// columns on WindowSizeMsg and does NOT rebuild the rows, so a resize leaves the existing
// rows formatted for the previous width until the next sessionsLoadedMsg or the next
// streamed event repaints them. For that interval a terminal just narrowed can show one
// clipped figure and one just widened can show one needless ellipsis. Closing it means
// rebuilding the rows from layout(), which is keys.go's call to make, not this file's.
func (m *model) fittedSessionsColumnWidth(title string) int {
	for _, c := range m.sessionsTbl.Columns() {
		if c.Title == title {
			return c.Width
		}
	}
	return 0
}

// newSessionsTable builds an empty sessions table. Columns are fitted to the terminal by
// layout(), which is called on every WindowSizeMsg.
func newSessionsTable() table.Model {
	t := table.New(
		table.WithColumns(sessionsColumns()),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildSessionsTable updates the rows from m.sessions, applies the current
// filter, and keeps the cursor on the previously-selected session if still
// present.
func (m *model) rebuildSessionsTable() {
	prev := ""
	if rows := m.sessionsTbl.Rows(); len(rows) > 0 {
		prev = rows[m.sessionsTbl.Cursor()][0]
	}
	now := time.Now()
	// The width COST was actually FITTED to, not the 10 sessionsColumns declares:
	// fitTableColumns shrinks it to 5 on a 40-column terminal, and fitCostCell needs the
	// real number to decide whether a figure fits. Hoisted out of the loop because it is
	// the same for every row.
	costWidth := m.fittedSessionsColumnWidth("COST")
	rows := make([]table.Row, 0, len(m.sessions))
	for _, s := range m.sessions {
		if m.filter != "" && !strings.Contains(s.ID, m.filter) {
			continue
		}
		active := ""
		if s.Active {
			active = "●"
		}
		// Blank, never "$0.00", when the cost is unknown. usage_render.go establishes
		// the rule and the strip repeats it: a zero cost and an unknown cost are
		// different answers, and only one of them means the traffic was free. A cell
		// has no room to say "unavailable", so it says nothing — see sessionCost for
		// the three states that collapse into priced=false.
		//
		// A KNOWN cost goes through fitCostCell, which renders costElision rather than a
		// figure the fitted column would clip. That is a third state and it is why the
		// elision is not blank: blank here would say the cost is unknown when it is known
		// and merely too wide for this terminal.
		cost := ""
		if usd, priced := m.sessionCost(s.ID); priced {
			cost = fitCostCell(usd, costWidth)
		}
		rows = append(rows, table.Row{
			s.ID,
			relTime(now, s.UpdatedAt),
			fmt.Sprintf("%d", s.EventCount),
			sessionTokens(s.TotalTokens, m.events[s.ID]),
			cost,
			active,
		})
	}
	// Sessions whose events abctl still holds but the server no longer lists.
	// Retaining the events (#870) is only half a fix if there is no row to
	// select them from: after a proxy restart the server lists nothing, so
	// without this the picker is empty and the retained history is unreachable.
	for _, id := range m.cachedOnlySessionIDs() {
		if m.filter != "" && !strings.Contains(id, m.filter) {
			continue
		}
		cached := m.events[id]
		// COST is blank for these rows on purpose, not because the lookup would be
		// awkward. It COULD be looked up — the id is all sessionCost needs — but these
		// are sessions the server has expired out of its store, while the strip's window
		// is a rolling hour: any figure found would cover whatever part of the last hour
		// happens to overlap, not the session the row is about. Blank says "unknown",
		// which is what it is.
		rows = append(rows, table.Row{
			id,
			"—",
			fmt.Sprintf("%d", len(cached)),
			sessionTokens(0, cached),
			"",
			"cached",
		})
	}
	m.sessionsTbl.SetRows(rows)

	// Restore cursor position if possible. Through setCursorVisible: a restored row
	// past the first screenful would otherwise land one line below the rendered
	// window, leaving the pane with no highlight — see setCursorVisible.
	if prev != "" {
		for i, r := range rows {
			if r[0] == prev {
				setCursorVisible(&m.sessionsTbl, i)
				return
			}
		}
	}
	setCursorVisible(&m.sessionsTbl, 0)
}

// cachedOnlySessionIDs lists sessions abctl has events for that the server's
// current list omits, sorted so the picker does not reshuffle under the cursor
// on each refresh. Empty caches are skipped: a row advertising zero events
// helps nobody, and snapshotLoadedMsg can create the key with an empty slice.
func (m *model) cachedOnlySessionIDs() []string {
	live := make(map[string]bool, len(m.sessions))
	for _, s := range m.sessions {
		live[s.ID] = true
	}
	out := make([]string, 0, len(m.events))
	for id, evs := range m.events {
		if live[id] || len(evs) == 0 {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// relTime renders "Ns", "Nm", "Nh" for small deltas; absolute time otherwise.
func relTime(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2 15:04")
	}
}

// sessionTokens reports the total tokens for a session. Prefers the
// server-computed count from SessionSummary (authoritative, covers the
// full event backlog even before we've streamed anything for this
// session). Falls back to a client-side sum over the cached events when
// the server returned zero (older authbridge server without token
// aggregation). Returns "—" when neither source has data.
func sessionTokens(serverTotal int, cached []pipeline.SessionEvent) string {
	if serverTotal > 0 {
		return formatCount(serverTotal)
	}
	var total int
	for i := range cached {
		if cached[i].Phase != pipeline.SessionResponse {
			continue
		}
		if cached[i].Inference == nil {
			continue
		}
		total += cached[i].Inference.TotalTokens
	}
	if total == 0 {
		return "—"
	}
	return formatCount(total)
}

// selectedSessionID returns the cursor row's session ID, or "".
func (m *model) selectedSessionID() string {
	rows := m.sessionsTbl.Rows()
	if len(rows) == 0 {
		return ""
	}
	return rows[m.sessionsTbl.Cursor()][0]
}
