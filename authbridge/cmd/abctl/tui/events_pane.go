package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// newEventsTable builds an empty events table. Uses the shared tableStyles
// (including the Reverse-based Selected highlight) like the other panes —
// now safe because per-cell ANSI coloring was removed from this table.
func newEventsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "TIME", Width: 12},
			{Title: "DIR", Width: 4},
			{Title: "PHASE", Width: 6},
			{Title: "PROTO", Width: 5},
			{Title: "METHOD", Width: 22},
			{Title: "STATUS", Width: 7},
			{Title: "DURATION", Width: 10},
			{Title: "TOKENS", Width: 8},
			{Title: "HOST", Width: 20},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildEventsTable populates the events table from the cache for the
// currently selected session, applying filter + preserving cursor. Also
// resizes the table height to account for the IDENTITY banner — when
// the session has inbound identity, subtract the banner's rendered
// height so it doesn't push rows off-screen; otherwise claim the full
// body height.
func (m *model) rebuildEventsTable() {
	events := m.events[m.selectedSess]

	if m.bodyHeight > 0 {
		h := m.bodyHeight
		if len(distinctInboundIdentities(events)) > 0 {
			h -= identityBannerHeight
		}
		if h < 3 {
			h = 3
		}
		m.eventsTbl.SetHeight(h)
	}

	prevRow := m.eventsTbl.Cursor()
	wasAtEnd := prevRow >= len(m.eventsTbl.Rows())-1

	// Compute request↔response pairs up-front so response rows can render
	// a visual connector back to their request.
	pairs := pairRequestsAndResponses(events)

	rows := make([]table.Row, 0, len(events))
	for i, e := range events {
		if m.filter != "" && !matchEvent(e, m.filter) {
			continue
		}
		phase := shortPhase(e.Phase)
		if e.Phase == pipeline.SessionResponse {
			if _, paired := pairs[i]; paired {
				// └ prefix visually connects the response to its request
				// in the row above (or earlier, if filtered).
				phase = "└" + phase
			}
		}
		rows = append(rows, table.Row{
			e.At.Format("15:04:05.00"),
			shortDirection(e.Direction),
			phase,
			shortProto(e),
			eventMethod(e),
			statusCell(e),
			durationCell(e),
			tokensCell(e),
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

// tokensCell shows the total token count for inference response rows so
// operators can spot expensive calls while scrolling. Blank for every
// other event type (a2a, mcp, inference *request*). Uses the same
// thousands-separator formatter as the sessions-pane totals.
func tokensCell(e pipeline.SessionEvent) string {
	if e.Phase != pipeline.SessionResponse || e.Inference == nil || e.Inference.TotalTokens == 0 {
		return ""
	}
	return formatCount(e.Inference.TotalTokens)
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

// pairRequestsAndResponses returns a map whose keys are the indexes of
// events that participate in a request↔response pair. It walks events in
// order: each SessionRequest is paired with the NEXT SessionResponse that
// matches on direction + protocol + method, within the same session.
//
// Sequential pairing is sufficient for AuthBridge's current traffic
// patterns (no overlapping same-method outbound calls per turn). Future
// work: key pairs by MCP.RPCID / A2A.RPCID when available for stricter
// correlation.
func pairRequestsAndResponses(events []pipeline.SessionEvent) map[int]int {
	pairs := make(map[int]int)
	for i := range events {
		req := events[i]
		if req.Phase != pipeline.SessionRequest {
			continue
		}
		if _, already := pairs[i]; already {
			continue
		}
		for j := i + 1; j < len(events); j++ {
			resp := events[j]
			if resp.Phase != pipeline.SessionResponse {
				continue
			}
			if _, taken := pairs[j]; taken {
				continue
			}
			if resp.Direction != req.Direction {
				continue
			}
			if shortProto(resp) != shortProto(req) {
				continue
			}
			if eventMethod(resp) != eventMethod(req) {
				continue
			}
			pairs[i] = j
			pairs[j] = i
			break
		}
	}
	return pairs
}

// identityBannerStyle renders the small bordered box above the events
// table. Rounded border matches the outer frame; muted color keeps the
// banner as context rather than competing with the event rows.
var identityBannerStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.AdaptiveColor{Light: "#94A3B8", Dark: "#475569"}).
	Padding(0, 1)

// identityBannerHeight is the rendered height of the banner — four lines
// of content plus two border lines. layout() subtracts this from the
// events-table height so the banner doesn't push rows off-screen.
const identityBannerHeight = 6

// identityBanner renders a compact "IDENTITY" box summarizing the caller
// of this session's inbound events. If callers diverge across the
// session, it reports the count so the operator knows to check detail
// rows. Returns an empty string when no inbound identity is present
// (e.g. outbound-only buckets).
func identityBanner(events []pipeline.SessionEvent) string {
	idents := distinctInboundIdentities(events)
	if len(idents) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("IDENTITY"))
	b.WriteByte('\n')

	if len(idents) == 1 {
		id := idents[0]
		b.WriteString(fmt.Sprintf("subject  %s\n", nonEmpty(id.Subject, "—")))
		b.WriteString(fmt.Sprintf("client   %s\n", nonEmpty(id.ClientID, "—")))
		b.WriteString(fmt.Sprintf("scopes   %s", nonEmpty(truncateScopes(id.Scopes, 3), "—")))
	} else {
		// Multiple distinct callers — surface the count; detail rows
		// carry the full identity for drill-down.
		subjects := make([]string, 0, len(idents))
		for _, id := range idents {
			subjects = append(subjects, nonEmpty(id.Subject, "—"))
		}
		b.WriteString(fmt.Sprintf("subjects  %d distinct: %s\n", len(idents), strings.Join(subjects, ", ")))
		b.WriteString("client    (see individual events)\n")
		b.WriteString("scopes    (see individual events)")
	}
	return identityBannerStyle.Render(b.String())
}

// identityKey is the comparable shape used to dedupe identities in the
// banner. Using a struct avoids string concatenation (and the theoretical
// "|" collision) — subject+clientID are the two fields that define a
// unique caller; scopes can legitimately vary turn-to-turn.
type identityKey struct {
	subject  string
	clientID string
}

// distinctInboundIdentities returns the unique EventIdentity values seen on
// inbound events, in first-seen order.
func distinctInboundIdentities(events []pipeline.SessionEvent) []*pipeline.EventIdentity {
	var out []*pipeline.EventIdentity
	seen := map[identityKey]bool{}
	for i := range events {
		e := &events[i]
		if e.Direction != pipeline.Inbound || e.Identity == nil {
			continue
		}
		k := identityKey{subject: e.Identity.Subject, clientID: e.Identity.ClientID}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e.Identity)
	}
	return out
}

// truncateScopes joins the first n scopes with commas and appends a
// "+N more" suffix if the list was longer. Keeps the identity banner
// from overflowing the terminal when a caller has many scopes.
func truncateScopes(scopes []string, n int) string {
	if len(scopes) == 0 {
		return ""
	}
	if len(scopes) <= n {
		return strings.Join(scopes, ", ")
	}
	return strings.Join(scopes[:n], ", ") + fmt.Sprintf(" +%d more", len(scopes)-n)
}

