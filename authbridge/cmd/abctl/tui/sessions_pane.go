package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// sessionsColumns is the table's full-width column set, before any terminal-fitting.
//
// A function rather than a package var so layout() can re-fit from the originals on every
// resize: fitting the LIVE columns would be cumulative, and a terminal that got narrower once
// would keep its narrowed columns after being widened again.
//
// COST and SPAN are appended rather than slotted next to TOKENS and UPDATED where they read
// best. table.Row is a []string addressed positionally — selectedSessionID takes [0], and a
// test pins ACTIVE at [4] — so an INSERT shifts every reader silently instead of failing to
// compile. Appending trades column order for a change that cannot misattribute a cell.
//
// The declared widths sum to 98, so the table renders 116 display columns with bubbles'
// two-per-cell padding. That is well past an 80-column terminal, which is exactly why
// fitTableColumns exists: it drops trailing columns rather than letting the renderer clip a
// figure mid-digits. Without it a 96-column terminal rendered "$12.5" for a $12.5000 session
// — a half-rendered dollar amount that reads as a real, smaller one.
func sessionsColumns() []table.Column {
	return []table.Column{
		{Title: "ID", Width: 40},
		{Title: "UPDATED", Width: 14},
		{Title: "EVENTS", Width: 8},
		{Title: "TOKENS", Width: 10},
		{Title: "ACTIVE", Width: 8},
		// 10 columns is exactly "$9999.9999", the widest figure formatUSDCell's fixed 4dp
		// produces below five figures of dollars. Above that bubbles truncates with
		// runewidth.Truncate, putting an ellipsis inside a dollar amount — the one thing
		// #953 rules out. The ceiling means one session spending $10,000 inside the
		// strip's window; pinned by TestSessionsTable_CostAndSpanFitTheirColumns so the
		// trade is visible if the formatter's precision changes.
		{Title: "COST", Width: 10},
		// 8 holds formatSpan's ordinary output. A corrupt CreatedAt can produce more
		// (up to "106751d23h"), which bubbles then ellipsizes — acceptable for a
		// duration in a way it is not for money.
		{Title: "SPAN", Width: 8},
	}
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
		cost := ""
		if usd, priced := m.sessionCost(s.ID); priced {
			cost = formatUSDCell(usd)
		}
		rows = append(rows, table.Row{
			s.ID,
			relTime(now, s.UpdatedAt),
			// The server's count, and only ever the server's: it is the complete one.
			// abctl's own cache holds what it snapshotted plus what it has streamed
			// since attaching, which for a session older than the connection is a
			// smaller number — and when handleStreamEvent also wrote this field, the
			// cell flipped between the two on live traffic. The cached-only rows below
			// use len(cached) because the server does not list those at all.
			fmt.Sprintf("%d", s.EventCount),
			sessionTokens(s.TotalTokens, m.events[s.ID]),
			active,
			cost,
			formatSpan(sessionSpan(s)),
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
		// COST and SPAN are blank for these rows on purpose, not because the lookups
		// would be awkward. There is no SessionSummary, so there are no
		// CreatedAt/UpdatedAt to span at all. The cost COULD be looked up — the id is
		// all sessionCost needs — but these are sessions the server has expired out of
		// its store, while the strip's window is a rolling hour: any figure found would
		// cover whatever part of the last hour happens to overlap, not the session the
		// row is about. Blank says "unknown", which is what it is.
		rows = append(rows, table.Row{
			id,
			"—",
			fmt.Sprintf("%d", len(cached)),
			sessionTokens(0, cached),
			"cached",
			"",
			"",
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

// formatSpan renders a duration for the SPAN cell, compactly.
//
// Not time.Duration.String(): that renders 1h23m45.6s for the case this column
// exists to show, which is 10 columns of which the last four are noise — and in an
// 8-column cell it is truncated mid-number, which is exactly the clipping the spend
// work exists to avoid. Two units at most, largest first, and no decimals: the
// question is "how long has this been going", not "to the millisecond".
//
// Sits beside relTime rather than in spend.go because the two are siblings — both
// turn a time into a cell for this table — and a reader comparing the UPDATED and
// SPAN columns needs to see both formatters at once. relTime is not reused: it
// renders "ago" suffixes and "just now", which are wrong for an elapsed span.
//
// Non-positive renders "" for the reason sessionSpan returns 0 there: a blank cell
// says "unknown", "0s" would assert an instantaneous session.
func formatSpan(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		if m := int((d % time.Hour) / time.Minute); m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		days := int(d / (24 * time.Hour))
		if h := int((d % (24 * time.Hour)) / time.Hour); h > 0 {
			return fmt.Sprintf("%dd%dh", days, h)
		}
		return fmt.Sprintf("%dd", days)
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
