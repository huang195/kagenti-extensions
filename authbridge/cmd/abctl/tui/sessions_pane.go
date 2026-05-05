package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
)

// newSessionsTable builds an empty sessions table with the four columns.
// Widths are refined later by layout() based on terminal width.
func newSessionsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "ID", Width: 40},
			{Title: "UPDATED", Width: 14},
			{Title: "EVENTS", Width: 8},
			{Title: "ACTIVE", Width: 8},
		}),
		table.WithFocused(true),
	)
	s := table.DefaultStyles()
	s.Header = styleTableHeader
	s.Selected = styleTableSelected
	t.SetStyles(s)
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
		rows = append(rows, table.Row{
			s.ID,
			relTime(now, s.UpdatedAt),
			fmt.Sprintf("%d", s.EventCount),
			active,
		})
	}
	m.sessionsTbl.SetRows(rows)

	// Restore cursor position if possible.
	if prev != "" {
		for i, r := range rows {
			if r[0] == prev {
				m.sessionsTbl.SetCursor(i)
				return
			}
		}
	}
	if len(rows) > 0 {
		m.sessionsTbl.SetCursor(0)
	}
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

// selectedSessionID returns the cursor row's session ID, or "".
func (m *model) selectedSessionID() string {
	rows := m.sessionsTbl.Rows()
	if len(rows) == 0 {
		return ""
	}
	return rows[m.sessionsTbl.Cursor()][0]
}
