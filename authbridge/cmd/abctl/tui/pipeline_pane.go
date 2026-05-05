package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/kagenti/kagenti-extensions/authbridge/cmd/abctl/apiclient"
)

// newPipelineTable builds the plugins table shown on the Pipeline top-level
// view. Columns are sized to match the sessions table's compact width so
// Tab-switching doesn't feel layout-jarring.
func newPipelineTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "#", Width: 3},
			{Title: "DIRECTION", Width: 10},
			{Title: "PLUGIN", Width: 22},
			{Title: "WRITES", Width: 18},
			{Title: "BODY", Width: 6},
			{Title: "EVENTS", Width: 8},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildPipelineTable renders the plugin list with a "(app)" divider row
// between inbound and outbound. eventsPerPlugin counts how many events in
// the cached data came from each plugin (by matching the event's written
// extension against the plugin's Writes).
func (m *model) rebuildPipelineTable() {
	if m.pipeline == nil {
		m.pipelineTbl.SetRows(nil)
		return
	}
	counts := m.countEventsPerPlugin()

	rows := make([]table.Row, 0, len(m.pipeline.Inbound)+len(m.pipeline.Outbound)+1)
	for _, p := range m.pipeline.Inbound {
		rows = append(rows, pipelineRow(p, counts[p.Name]))
	}
	// Divider between inbound and outbound.
	rows = append(rows, table.Row{"", "", "── (app) ──", "", "", ""})
	for _, p := range m.pipeline.Outbound {
		rows = append(rows, pipelineRow(p, counts[p.Name]))
	}
	m.pipelineTbl.SetRows(rows)
	// If cursor is on the divider row, nudge to the next plugin.
	if isDividerRow(rows, m.pipelineTbl.Cursor()) {
		m.pipelineTbl.SetCursor(m.pipelineTbl.Cursor() + 1)
	}
}

func pipelineRow(p apiclient.PipelinePlugin, events int) table.Row {
	body := "no"
	if p.BodyAccess {
		body = "yes"
	}
	eventsStr := ""
	if events > 0 {
		eventsStr = fmt.Sprintf("%d", events)
	}
	// Color plugin names by the protocol they parse (or leave unstyled for
	// non-parser plugins like token-exchange / jwt-validation) so Pipeline
	// and Events panes share a visual vocabulary.
	name := p.Name
	if style := pluginStyle(p); style != nil {
		name = style.Render(name)
	}
	return table.Row{
		fmt.Sprintf("%d", p.Position),
		p.Direction,
		name,
		strings.Join(p.Writes, ","),
		body,
		eventsStr,
	}
}

// pluginStyle returns the protocol color for a plugin based on its Writes
// slots. Plugins that don't write a protocol extension (token-exchange,
// jwt-validation) return nil so they render in the default color.
func pluginStyle(p apiclient.PipelinePlugin) *lipgloss.Style {
	for _, w := range p.Writes {
		switch w {
		case "a2a":
			s := styleProtoA2A
			return &s
		case "mcp":
			s := styleProtoMCP
			return &s
		case "inference":
			s := styleProtoInference
			return &s
		}
	}
	return nil
}

func isDividerRow(rows []table.Row, i int) bool {
	if i < 0 || i >= len(rows) {
		return false
	}
	return rows[i][2] == "── (app) ──"
}

// selectedPlugin returns the PipelinePlugin under the cursor, or nil when
// the cursor sits on the divider or the table is empty.
func (m *model) selectedPlugin() *apiclient.PipelinePlugin {
	if m.pipeline == nil {
		return nil
	}
	rows := m.pipelineTbl.Rows()
	i := m.pipelineTbl.Cursor()
	if i < 0 || i >= len(rows) {
		return nil
	}
	if isDividerRow(rows, i) {
		return nil
	}
	// Rows are inbound, divider, outbound. Map the table index back to the
	// source slices by name rather than arithmetic — safer against future
	// divider changes.
	name := rows[i][2]
	for j := range m.pipeline.Inbound {
		if m.pipeline.Inbound[j].Name == name {
			return &m.pipeline.Inbound[j]
		}
	}
	for j := range m.pipeline.Outbound {
		if m.pipeline.Outbound[j].Name == name {
			return &m.pipeline.Outbound[j]
		}
	}
	return nil
}

// countEventsPerPlugin attributes each cached event to the plugin that
// wrote its extension. An event maps to a plugin when that plugin's
// Capabilities.Writes contains the extension slot that the event's body
// represents (a2a, mcp, inference). Events without a recognised extension
// (e.g. request-phase events before parsing completes) are unattributed.
func (m *model) countEventsPerPlugin() map[string]int {
	counts := map[string]int{}
	if m.pipeline == nil {
		return counts
	}
	// Build slot → plugin name map for quick lookup.
	slotToPlugin := map[string]string{}
	for _, p := range m.pipeline.Inbound {
		for _, w := range p.Writes {
			slotToPlugin[w] = p.Name
		}
	}
	for _, p := range m.pipeline.Outbound {
		for _, w := range p.Writes {
			slotToPlugin[w] = p.Name
		}
	}
	for _, events := range m.events {
		for _, e := range events {
			switch {
			case e.A2A != nil:
				if name, ok := slotToPlugin["a2a"]; ok {
					counts[name]++
				}
			case e.Inference != nil:
				if name, ok := slotToPlugin["inference"]; ok {
					counts[name]++
				}
			case e.MCP != nil:
				if name, ok := slotToPlugin["mcp"]; ok {
					counts[name]++
				}
			}
		}
	}
	return counts
}

