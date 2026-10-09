// Package tui implements the agentop Bubble Tea interactive terminal UI.
package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// Palette keeps all colors in one place so recoloring the TUI is a single
// file edit. Colors are chosen to render legibly on both light and dark
// terminals (Bubble Tea's ANSI adaptive palette) — avoid 24-bit colors here.
var (
	colorAccent   = lipgloss.AdaptiveColor{Light: "#4F46E5", Dark: "#A5B4FC"}
	colorOK       = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#6EE7B7"}
	colorWarn     = lipgloss.AdaptiveColor{Light: "#92400E", Dark: "#FCD34D"}
	colorError    = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"}
	colorMuted    = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	colorInbound  = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#93C5FD"}
	colorOutbound = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"}

	// colorOnSeries is the foreground for a letter drawn on a series background
	// (the usage pane's stacked bars). Inverted relative to the palette: those
	// backgrounds are mid-tone in both themes, so the mark needs the opposite end
	// of the ramp to stay legible — white on the darker light-theme grounds, near
	// black on the lighter dark-theme ones.
	colorOnSeries = lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#111827"}

	// colorSelectedBg is the row a table's cursor is on — see tableStyles for why selection is
	// a tint rather than a reverse.
	//
	// A BACKGROUND, so the cells' own ink is left alone: the sessions table's context gauge
	// encodes its value as ink, and anything that swaps ink for background reads it backwards.
	// Mid-tone in both themes for the same reason colorOnSeries is inverted relative to the
	// palette — it has to sit under default-foreground text without hiding it.
	colorSelectedBg = lipgloss.AdaptiveColor{Light: "#D7D7FF", Dark: "#303050"}
)

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleHint   = lipgloss.NewStyle().Foreground(colorMuted)
	styleOK     = lipgloss.NewStyle().Foreground(colorOK)
	styleWarn   = lipgloss.NewStyle().Foreground(colorWarn)
	styleError  = lipgloss.NewStyle().Foreground(colorError)
	styleMuted  = lipgloss.NewStyle().Foreground(colorMuted)
	styleBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorMuted)

	// styleMatch and styleCurrentMatch draw the characters a search matched: a background,
	// so a match reads the same whatever ink the text under it has (the JSON colorizer's,
	// a table cell's), and colorOnSeries on it for the reason that colour exists — the warn
	// and accent grounds are mid-tone in both themes. The current one, the row or line `n`
	// moved to, is the accent, so it stands out from the rest.
	styleMatch        = lipgloss.NewStyle().Foreground(colorOnSeries).Background(colorWarn)
	styleCurrentMatch = lipgloss.NewStyle().Foreground(colorOnSeries).Background(colorAccent).Bold(true)

	// PER-PROTOCOL CELL COLOURING USED TO LIVE HERE — styleProtoA2A / MCP / Inference / Blocked
	// and a protoStyle() dispatcher — and is deleted rather than kept for later. It had no
	// callers anywhere outside this file (events_pane.go records the change that removed the
	// last one), and while it sat here unused it was cited as the reason the selected row had
	// to be reverse video, which is the bug below. A palette nothing renders cannot justify a
	// constraint on what everything renders; restore it from git with its caller if the events
	// pane wants colour again.
)

// tableStyles returns the standard agentop table palette — layered on top of
// bubbles' DefaultStyles so cell padding, borders, and other layout rules
// come through unchanged.
//
// SELECTION IS A BACKGROUND TINT, NOT A REVERSE, and the reason is the sessions table's
// CTX(1M) gauge. bubbles wraps the whole row in Selected.Render, so reverse video swapped
// ink and background for every cell in it — and a gauge encodes its value AS ink. The filled
// blocks rendered in the background colour (reading as empty) while the blank track rendered in
// the foreground (reading as filled), so the highlighted row showed 44% as ~56%, from the wrong
// side. Any density-encoded figure has the same problem; only position and pattern survive an
// inversion.
//
// The previous comment here said Reverse was deliberate, to keep per-cell protocol colouring from
// being clobbered by the row style. That colouring did not exist — the palette it named had no
// callers — so nothing was being protected, and the palette is now deleted rather than left to make
// the same claim again.
//
// Colouring the GAUGE instead was the first thing tried and is not possible in this table: a
// styled cell carries escape bytes, runewidth.StringWidth counts them (an 11-column gauge
// measures 24), and renderRow truncates with runewidth BEFORE styling — so the cell comes back
// as a lone "…". Same mechanism as the mangled marker that
// TestSessionsPicker_CachedMarkerRendersIntact pins. bubbles v1.0.0 has no per-column style, so
// the row style is the only lever there is.
//
// What this costs: a terminal with no colour (the Ascii profile) drops the background and the
// selected row is left bold only, where reverse video would have survived. Accepted knowingly —
// a bold row is still marked, and a gauge that lies on the selected row is worse than a
// selection that is merely quieter in monochrome.
//
// ONE STYLE FOR EVERY TABLE. Scoping this to the sessions table would have left one pane
// tinting its selection and five inverting theirs.
func tableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		Foreground(colorAccent).
		BorderForeground(colorMuted).
		Bold(true)
	s.Selected = lipgloss.NewStyle().Bold(true).Background(colorSelectedBg)
	// A search's matched characters, and a row whose only match is somewhere no cell shows.
	// The mark is ink rather than a background, so a marked row never reads as the cursor,
	// in the warn colour the footer's search status is drawn in. Recolouring ink is safe for
	// the context gauge, unlike inverting it: the gauge's value is which cells are filled.
	s.Match = styleMatch
	s.CurrentMatch = styleCurrentMatch
	s.Marked = lipgloss.NewStyle().Foreground(colorWarn)
	return s
}
