package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// stripLabel prefixes the line. Kept short: it is spent on every width.
const stripLabel = "SPEND"

// stripGap separates whole figures. Three spaces rather than a glyph separator so
// the figures read as independent readings rather than as one expression — the
// same spacing the footer's status line uses between its own readings.
const stripGap = "   "

// renderSpendStrip draws the always-on spend line.
//
// A pure function of its arguments so it can be table-tested at many widths,
// which is where its entire risk lives. Chrome that overflows wraps, and a
// wrapped chrome line costs a row of the table below it.
//
// Degradation drops WHOLE FIGURES from the right and never clips a number.
// #953 asks for "no truncated numbers", and a half-rendered dollar amount is
// worse than a missing one: "$1.1" reads as a real, smaller figure rather than as
// an incomplete one. This is the same discipline as fitHintLine, mirrored —
// helpView orders its hints so the essential ones come last and fitHintLine cuts
// the front; the strip's essential figure is first, so it cuts the back.
//
// Returns "" in exactly two cases, and they are deliberately the only two:
//
//  1. No poll has answered yet (!HasSnapshot). "We have not looked" is honest and
//     self-corrects within one poll interval.
//  2. The terminal is too narrow for even one WHOLE figure — under about 18
//     columns. Accepted rather than fixed: at that width there is no honest short
//     form, and clipping a number is forbidden. The row stays reserved, because
//     making the reservation depend on the rendered result would mean re-running
//     layout() outside WindowSizeMsg and resizing every table as data came and
//     went, which is worse than a blank line in a terminal too narrow to show a
//     dollar figure at all.
//
// Every OTHER state says something: a failed poll says "cost unavailable", an
// unpriced window says so with its coverage, and a window with no priceable
// traffic says that. An always-on strip that renders nothing has failed at its
// only job.
//
// Two of the four figures the spec wants do not exist yet ("today" needs the
// durable ledger, "saved" needs prune attribution), so the strip ships with two
// and reports the other two only once HasToday / HasSaved are set.
func renderSpendStrip(s spendSummary, width int) string {
	if width <= 0 {
		return ""
	}

	// The poll failed outright, so there are no counters to qualify the answer
	// with. Say "unavailable" and point at the Usage pane, which already
	// distinguishes an old proxy from a transport error and can explain WHY.
	//
	// Silence is the worst option available here: the row is reserved on height
	// alone, so returning "" for a persistently failing endpoint buys a permanent
	// blank line above the footer and no diagnostic anywhere on screen.
	if s.Failed {
		return fitStripFigures(stripLabel, []string{"cost unavailable", "[u] usage"}, width)
	}

	// Nothing was priced: say so rather than assert a zero. usage_render.go
	// establishes the rule — a zero cost and an unknown cost are different
	// answers, and only one of them means the traffic was free.
	if !s.Priced && !s.HasToday {
		if s.Priceable > 0 {
			return fitStripFigures(stripLabel, []string{
				"cost unavailable",
				fmt.Sprintf("%d of %d unpriced", s.Unpriced, s.Priceable),
				"[u] usage",
			}, width)
		}
		// Priceable == 0 WITH a snapshot in hand is a finding, not an absence: we
		// looked, and there was no inference traffic to price. Say so. An always-on
		// strip that renders nothing has failed at its only job, and since the row is
		// reserved on height alone the alternative is a blank line above the footer,
		// which reads as a broken UI rather than as an absence of data.
		if s.HasSnapshot {
			return fitStripFigures(stripLabel, []string{"no priceable traffic yet"}, width)
		}
		// No poll has answered yet. THIS silence is honest: it says "we have not
		// looked", which is true, brief, and self-correcting within one poll
		// interval. It is the only case where "" is the right answer.
		return ""
	}

	// Ordered most to least important; the tail is what a narrow terminal loses.
	var figures []string
	if s.HasToday {
		// Today outranks the rolling window when it exists: it is the figure an
		// operator is accountable for, and the window is context for it.
		figures = append(figures, formatUSDCell(s.TodayUSD)+" today")
	}
	// Guarded on Priced independently of the branch above, which lets !Priced
	// through whenever HasToday is set. Without this guard that combination — a
	// ledger-backed today figure over a rolling window that priced nothing —
	// rendered "$0.0000 /1h", stating a settled zero for a cost nobody knows. It is
	// unreachable while nothing sets HasToday, which is why it survived two review
	// rounds; the commit that adds the ledger arms it. Fixed here rather than left
	// as a note because an unreachable bug still has to be reasoned about by every
	// reader, and this is two lines.
	if s.Priced {
		figures = append(figures, formatUSDCell(s.WindowUSD)+" /"+s.WindowLabel)
	}
	if s.BurnPerMin > 0 {
		figures = append(figures, "~"+formatUSDCell(s.BurnPerMin)+"/min")
	}
	if s.HasSaved {
		figures = append(figures, "saved "+formatUSDCell(s.SavedUSD))
	}
	// The coverage gap rides at the end: it qualifies the total, so it is the
	// first thing a narrow terminal gives up — but a fully priced deployment must
	// never carry it, because a permanent warning with nothing to act on is what
	// teaches an operator to ignore the one signal that matters.
	if s.Unpriced > 0 && s.Priceable > 0 {
		figures = append(figures, fmt.Sprintf("%d of %d unpriced", s.Unpriced, s.Priceable))
	}
	return fitStripFigures(stripLabel, figures, width)
}

// fitStripFigures joins as many LEADING figures as fit, dropping whole ones from
// the right. Returns "" when even the first figure cannot fit without clipping,
// because a clipped figure is a wrong figure.
//
// Width arithmetic is lipgloss.Width throughout, never len() and never a rune
// count. footer.go:88-92 records the bug that costs: a budget computed in display
// columns and then sliced by rune index overflowed on any wide character,
// rendering 55 columns for a 40-column budget. The package's trunc and truncStr
// helpers are both display-cell-unaware for the same reason, so neither is usable
// here.
//
// Linear rather than a binary search over n: the list is at most five figures
// long, and the loop is the specification — "the widest prefix that fits".
func fitStripFigures(label string, figures []string, width int) string {
	if len(figures) == 0 {
		return ""
	}
	for n := len(figures); n >= 1; n-- {
		candidate := label + "  " + strings.Join(figures[:n], stripGap)
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	// The label does not fit alongside even one figure. Drop the LABEL before
	// dropping the number: the figure is the information, the label is decoration,
	// and a dollar amount on its own is still unambiguous in the chrome.
	if lipgloss.Width(figures[0]) <= width {
		return figures[0]
	}
	return ""
}

// spendStripMinHeight is the terminal height at which the strip earns its row.
//
// Below it the row is worth more to the table than to the chrome, so the strip
// yields it entirely rather than shrink the body further. 20 rows is roughly
// where an events table stops being able to show a turn's request and its
// response together, which is the smallest useful unit of that pane.
const spendStripMinHeight = 20

// spendStripVisible reports whether the strip DRAWS a row on the current pane.
//
// False on paneNamespaces and panePods: those run before a connection exists, so
// there is no spend to report, and they return early from paneView with their own
// JoinVertical anyway.
//
// Note the asymmetry with layout(), which reserves the row on HEIGHT alone and
// ignores the pane. That is deliberate and is explained where the reservation is
// made: layout() runs on WindowSizeMsg, so a pane-aware reservation would have to
// be re-run on every pane transition. The consequence is that the two picker
// panes render one row shorter than they strictly need — invisible, next to an
// events table that is wrong by a row and pushes the footer off-screen.
func (m *model) spendStripVisible() bool {
	if !m.spendStripReservesRow() {
		return false
	}
	switch m.pane {
	case paneNamespaces, panePods:
		return false
	}
	return true
}

// spendStripReservesRow reports whether layout() must hold a row back for the
// strip. Height ONLY — deliberately blind to the pane.
//
// layout() is called from exactly one place, the WindowSizeMsg handler; no pane
// transition re-runs it. So a reservation that read m.pane would go stale the
// moment the user moved between a picker and a data pane, and bodyHeight would
// stay wrong until the next terminal resize — which for a pane the strip DOES
// draw on means a body one row too tall and a footer pushed off the bottom.
//
// Being blind to the pane costs the two picker panes one row they could have
// used. That is the accepted trade: a picker one row short is invisible, an
// events table one row long is not.
func (m *model) spendStripReservesRow() bool { return m.height >= spendStripMinHeight }
