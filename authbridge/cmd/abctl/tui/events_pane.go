package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// newEventsTable builds an empty events table.
func newEventsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "TIME", Width: 12},
			{Title: "DIR", Width: 4},
			{Title: "PHASE", Width: 5},
			{Title: "PROTO", Width: 5},
			{Title: "METHOD", Width: 22},
			{Title: "STATUS", Width: 7},
			{Title: "DURATION", Width: 10},
			{Title: "HOST", Width: 20},
		}),
		table.WithFocused(true),
	)
	s := table.DefaultStyles()
	s.Header = styleTableHeader
	s.Selected = styleTableSelected
	t.SetStyles(s)
	return t
}

// rebuildEventsTable populates the events table from the cache for the
// currently selected session, applying filter + preserving cursor.
func (m *model) rebuildEventsTable() {
	events := m.events[m.selectedSess]
	prevRow := m.eventsTbl.Cursor()
	wasAtEnd := prevRow >= len(m.eventsTbl.Rows())-1

	rows := make([]table.Row, 0, len(events))
	for _, e := range events {
		if m.filter != "" && !matchEvent(e, m.filter) {
			continue
		}
		rows = append(rows, table.Row{
			e.At.Format("15:04:05.00"),
			shortDirection(e.Direction),
			shortPhase(e.Phase),
			shortProto(e),
			eventMethod(e),
			statusCell(e),
			durationCell(e),
			truncStr(e.Host, 20),
		})
	}
	m.eventsTbl.SetRows(rows)

	// Auto-follow: if user was at the bottom, stay at the bottom. Otherwise
	// preserve position so reading isn't disturbed by new events.
	if wasAtEnd && len(rows) > 0 {
		m.eventsTbl.SetCursor(len(rows) - 1)
	} else if prevRow < len(rows) {
		m.eventsTbl.SetCursor(prevRow)
	}
}

// selectedEvent returns the event at the cursor row, or nil.
func (m *model) selectedEvent() *pipeline.SessionEvent {
	rows := m.eventsTbl.Rows()
	if len(rows) == 0 {
		return nil
	}
	cur := m.eventsTbl.Cursor()
	// Re-walk the cache to find the cur'th filtered event.
	events := m.events[m.selectedSess]
	idx := 0
	for i := range events {
		if m.filter != "" && !matchEvent(events[i], m.filter) {
			continue
		}
		if idx == cur {
			return &events[i]
		}
		idx++
	}
	return nil
}

func shortDirection(d pipeline.Direction) string {
	if d == pipeline.Inbound {
		return "in"
	}
	return "out"
}

func shortPhase(p pipeline.SessionPhase) string {
	if p == pipeline.SessionRequest {
		return "req"
	}
	return "resp"
}

// shortProto classifies an event by which extension carries meaningful
// metadata. Inference wins over MCP when both are present: mcp-parser
// greedily accepts any JSON as JSON-RPC (often with an empty method on
// LLM request bodies) and sets MCPExtension, so an LLM call shows up
// with both MCP{method:""} and Inference{model:...}. Picking inference
// first surfaces the more specific truth.
func shortProto(e pipeline.SessionEvent) string {
	switch {
	case e.A2A != nil:
		return "a2a"
	case e.Inference != nil:
		return "inf"
	case e.MCP != nil && e.MCP.Method != "":
		return "mcp"
	case e.MCP != nil:
		return "—" // empty-method MCP = mcp-parser false-positive
	}
	return "—"
}

func eventMethod(e pipeline.SessionEvent) string {
	switch {
	case e.A2A != nil:
		return truncStr(e.A2A.Method, 22)
	case e.Inference != nil:
		return truncStr(e.Inference.Model, 22)
	case e.MCP != nil:
		return truncStr(e.MCP.Method, 22)
	}
	return ""
}

func statusCell(e pipeline.SessionEvent) string {
	if e.StatusCode == 0 {
		return ""
	}
	return fmt.Sprintf("%d", e.StatusCode)
}

func durationCell(e pipeline.SessionEvent) string {
	if e.Duration == 0 {
		return ""
	}
	ms := e.Duration.Milliseconds()
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000)
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 2 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// matchEvent does a case-insensitive substring match across every string
// field the operator might reasonably search for.
func matchEvent(e pipeline.SessionEvent, q string) bool {
	q = strings.ToLower(q)
	hay := []string{e.Host, e.TargetAudience, shortProto(e), eventMethod(e)}
	if e.Identity != nil {
		hay = append(hay, e.Identity.Subject, e.Identity.ClientID)
	}
	if e.A2A != nil {
		hay = append(hay, e.A2A.SessionID, e.A2A.MessageID, e.A2A.Role)
		for _, p := range e.A2A.Parts {
			hay = append(hay, p.Content)
		}
	}
	if e.MCP != nil && e.MCP.Err != nil {
		hay = append(hay, e.MCP.Err.Message)
	}
	if e.Inference != nil {
		hay = append(hay, e.Inference.Completion, e.Inference.FinishReason)
	}
	for _, s := range hay {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}
