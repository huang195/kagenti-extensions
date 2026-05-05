// Package tui implements the abctl Bubble Tea interactive terminal UI.
package tui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// Palette keeps all colors in one place so recoloring the TUI is a single
// file edit. Colors are chosen to render legibly on both light and dark
// terminals (Bubble Tea's ANSI adaptive palette) — avoid 24-bit colors here.
var (
	colorAccent    = lipgloss.AdaptiveColor{Light: "#4F46E5", Dark: "#A5B4FC"}
	colorOK        = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#6EE7B7"}
	colorWarn      = lipgloss.AdaptiveColor{Light: "#92400E", Dark: "#FCD34D"}
	colorError     = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"}
	colorMuted     = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	colorInbound   = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#93C5FD"}
	colorOutbound  = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"}
)

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleHint   = lipgloss.NewStyle().Foreground(colorMuted)
	styleOK     = lipgloss.NewStyle().Foreground(colorOK)
	styleWarn   = lipgloss.NewStyle().Foreground(colorWarn)
	styleError  = lipgloss.NewStyle().Foreground(colorError)
	styleMuted  = lipgloss.NewStyle().Foreground(colorMuted)
	styleBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorMuted)

)

// tableStyles returns the standard abctl table palette — layered on top of
// bubbles' DefaultStyles so cell padding, borders, and other layout rules
// come through unchanged. Replacing DefaultStyles().Header with a blank
// lipgloss.Style wiped out the horizontal padding, which caused header
// cells to butt up against each other while row cells stayed padded —
// hence the "PROTOMETHOD" run-together in the events pane.
func tableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		Foreground(colorAccent).
		BorderForeground(colorMuted).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(colorAccent).
		Bold(true)
	return s
}
