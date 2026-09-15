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

// inexactMarker precedes a dollar figure that is NOT EXACT: at least one of the priced
// requests behind it carries a figure the aggregator could not settle exactly
// (usage.Counts.IncompleteRequests) — a stream that died before its output count, so the
// amount is a LOWER BOUND, or a gateway that reported only a total. It reads
// "approximately", and the real number is at least this much.
//
// ONE SPELLING, EVERYWHERE. This is the marker the strip puts on the today and window
// figures, the sessions table puts on its COST cell (see fitCostCell) and the Usage
// pane puts on its cost cell (see renderCostSummary). A branch that carries a commit
// titled "Stop publishing a truncated stream's floor as an exact total" had three money
// surfaces republishing that floor with no annotation at all, and three different
// annotations would have been barely better: a marker a reader has to learn twice is a
// marker they learn once and misread thereafter.
//
// One display column, so it survives every width the strip's fitter can produce and
// every width fitTableColumns can leave the COST column at. The burn rate has always
// worn the same "~" for the same reason — a derived rate is not an exact figure either.
const inexactMarker = "~"

// partialMarker follows a dollar figure that covers only PART of the traffic it
// appears to be about: something in the window was priceable and carries no figure, so
// the real total is LARGER than the number shown.
//
// One display column, and it rides on the figure itself rather than in the words beside
// it. That is the whole point: fitStripFigures may shorten a caveat to nothing and may
// drop a whole figure, but while a figure is on screen its marker is on screen with it,
// so a partial total can never be published as a complete one. It reads as "and more",
// which is what a coverage gap means for a total.
const partialMarker = "+"

// stripFigure is one reading in the strip, in the two forms it can take.
//
// full spells the caveat out; compact keeps the figure and its marker and drops only
// the words. Both forms of a qualified figure carry the marker — see partialMarker —
// so degradation costs an explanation and never the fact.
//
// Two forms rather than a general shortener because the strip has exactly one thing to
// give up under width pressure, and the fitter's job is to choose between whole
// readings, not to edit them.
type stripFigure struct {
	full    string
	compact string
}

// plainFigure is a reading with nothing to qualify: both forms are the same string.
func plainFigure(s string) stripFigure { return stripFigure{full: s, compact: s} }

// plainFigures lifts the fixed strings the non-figure branches render ("cost
// unavailable", "[u] usage") into the fitter's type. None of them can be partial, so
// none of them degrades.
func plainFigures(ss ...string) []stripFigure {
	out := make([]stripFigure, 0, len(ss))
	for _, s := range ss {
		out = append(out, plainFigure(s))
	}
	return out
}

// coverageNote is the one spelling of a coverage gap, shared by every branch so the
// wording cannot drift between them.
func coverageNote(unpriced, priceable int64) string {
	return fmt.Sprintf("%d of %d unpriced", unpriced, priceable)
}

// moneyFigure builds one dollar reading together with the caveats that belong to IT.
//
// label is the figure's own suffix — "today", "/1h" — and it is why this takes one at
// all: the coverage note used to ride at the END of the line, describing the rolling
// window, while the today figure sat at the front as the headline. "SPEND $4.1700
// today  $1.1200 /1h  40 of 40 unpriced" reads as qualifying the day and describes the
// hour. A caveat attached to its own figure cannot be misread that way, and a figure
// with no caveat of its own now says so by carrying no marker.
//
// Exactness and coverage are separate claims about the same number — whether the figure
// is the real one, and how much of the traffic it covers — and both can be true at once.
// They are stated in that order, matching cmd_cost.go, whose comment records why: the
// exactness caveat qualifies the dollar figure itself, where coverage qualifies how much
// of the traffic the figure is about.
func moneyFigure(usd float64, label string, unpriced, priceable, incomplete int64) stripFigure {
	amount := formatUSDCell(usd)
	if incomplete > 0 {
		amount = inexactMarker + amount
	}
	// A gap is only readable with a denominator, and a denominator of zero is not a
	// gap at all — it is a window with nothing to price, which the caller handles.
	partial := unpriced > 0 && priceable > 0
	if partial {
		amount += partialMarker
	}
	fig := plainFigure(amount + " " + label)
	var caveats []string
	if incomplete > 0 {
		// No denominator: spendSummary carries the day's and the window's priced counts
		// nowhere, and the count alone is what a strip has room for. The Cost pane states
		// the full "N of M priced figures are lower bounds" for a reader who wants it.
		caveats = append(caveats, fmt.Sprintf("%d inexact", incomplete))
	}
	if partial {
		caveats = append(caveats, coverageNote(unpriced, priceable))
	}
	if len(caveats) > 0 {
		fig.full = fig.compact + " (" + strings.Join(caveats, ", ") + ")"
	}
	return fig
}

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
//  2. The terminal is too narrow for even one WHOLE figure. That threshold is the
//     width of the FIRST figure's most compact form and nothing more, because
//     fitStripFigures drops the LABEL, and then the caveat's words, before it drops a
//     number: "$1.1200 /1h" is 11 columns and renders bare from width 11 up, so ""
//     appears only at 10 or below. A figure carrying a marker is one column wider than
//     the same figure without one, which moves that threshold by exactly one — the
//     marker is never what gets dropped. (This note said "about 18" — label plus figure
//     — which was right before the label-drop fallback below existed and has been wrong
//     by 7 since.) Accepted rather than
//     fixed: at that width there is no honest short form, and clipping a number is
//     forbidden. The row stays reserved, because making the reservation depend on
//     the rendered result would mean re-running layout() outside WindowSizeMsg and
//     resizing every table as data came and went, which is worse than a blank line
//     in a terminal too narrow to show a dollar figure at all.
//
// Every OTHER state says something: a failed poll says "cost unavailable", an
// unpriced window says so with its coverage, and a window with no priceable
// traffic says that. An always-on strip that renders nothing has failed at its
// only job.
//
// "Today" now exists — the durable cost ledger supplies it and spend.go's
// applyTodayFigure sets HasToday only when the server really served that window and
// priced it — so the strip reports three of the four figures the spec wants. "Saved"
// still needs prune attribution aggregated across requests, and until HasSaved is set
// the strip says nothing for it rather than a zero.
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
		return fitStripFigures(stripLabel, plainFigures("cost unavailable", "[u] usage"), width)
	}

	// Nothing was priced: say so rather than assert a zero. usage_render.go
	// establishes the rule — a zero cost and an unknown cost are different
	// answers, and only one of them means the traffic was free.
	if !s.Priced && !s.HasToday {
		if s.Priceable > 0 {
			// No label on the coverage note here, unlike the figures path below: there is
			// exactly one reading on this line and it is the window's, so there is no second
			// figure the note could be read as qualifying.
			return fitStripFigures(stripLabel, plainFigures(
				"cost unavailable",
				coverageNote(s.Unpriced, s.Priceable),
				"[u] usage",
			), width)
		}
		// Priceable == 0 WITH a snapshot in hand is a finding, not an absence: we
		// looked, and there was no inference traffic to price. Say so. An always-on
		// strip that renders nothing has failed at its only job, and since the row is
		// reserved on height alone the alternative is a blank line above the footer,
		// which reads as a broken UI rather than as an absence of data.
		if s.HasSnapshot {
			return fitStripFigures(stripLabel, plainFigures("no priceable traffic yet"), width)
		}
		// No poll has answered yet. THIS silence is honest: it says "we have not
		// looked", which is true, brief, and self-correcting within one poll
		// interval. It is the only case where "" is the right answer.
		return ""
	}

	// Ordered most to least important; the tail is what a narrow terminal loses.
	//
	// EVERY FIGURE CARRIES ITS OWN CAVEAT. A caveat built from one window's counters
	// and rendered beside another window's figure is not a warning, it is a
	// misattribution — and the figure it silently vouched for was "today", the headline
	// of this branch and the one the ledger is most likely to leave partial.
	var figures []stripFigure
	if s.HasToday {
		// Today outranks the rolling window when it exists: it is the figure an
		// operator is accountable for, and the window is context for it.
		figures = append(figures, moneyFigure(s.TodayUSD, "today",
			s.TodayUnpriced, s.TodayPriceable, s.TodayIncomplete))
	}
	// Guarded on Priced independently of the branch above, which lets !Priced
	// through whenever HasToday is set. Without this guard that combination — a
	// ledger-backed today figure over a rolling window that priced nothing —
	// rendered "$0.0000 /1h", stating a settled zero for a cost nobody knows.
	//
	// It was unreachable while nothing set HasToday, which is why it survived two
	// review rounds. It is REACHABLE now: the today poll lands on its own 5-minute
	// chain, so a fresh session can hold a priced day total beside a rolling hour that
	// has priced nothing yet. The guard is what makes that state render honestly.
	if s.Priced {
		figures = append(figures, moneyFigure(s.WindowUSD, "/"+s.WindowLabel,
			s.Unpriced, s.Priceable, s.Incomplete))
	} else if s.Unpriced > 0 && s.Priceable > 0 {
		// The window figure is suppressed because nothing in the window was priced, so
		// its coverage gap has no figure to ride on. It still has to be stated — this is
		// the reachable state where a ledger-backed day sits beside a rolling hour that
		// priced nothing — and it is stated WEARING THE WINDOW'S LABEL, so it cannot be
		// read as qualifying the today figure to its left. That misreading is exactly
		// what the unlabelled tail note used to produce.
		figures = append(figures, plainFigure(coverageNote(s.Unpriced, s.Priceable)+" /"+s.WindowLabel))
	}
	if s.BurnPerMin > 0 {
		// inexactMarker, the same one-cell claim it makes on every other figure: this is
		// not an exact number. A rate is a quotient of a total that may itself be partial
		// or inexact, so it is never exact, and it wears the marker unconditionally rather
		// than taking a second one for each way it can be wrong.
		figures = append(figures, plainFigure(inexactMarker+formatUSDCell(s.BurnPerMin)+"/min"))
	}
	if s.HasSaved {
		figures = append(figures, plainFigure("saved "+formatUSDCell(s.SavedUSD)))
	}
	return fitStripFigures(stripLabel, figures, width)
}

// fitStripFigures joins as many LEADING figures as fit, dropping whole ones from
// the right. Returns "" when even the first figure cannot fit without clipping,
// because a clipped figure is a wrong figure.
//
// Two degradation steps per prefix length, in this order: every figure spelled out,
// then every figure compact. So the line gives up its EXPLANATIONS before it gives up a
// READING, and it gives up a reading before it clips anything. The compact form still
// carries each figure's marker, so no step of this can turn a partial total into one
// that looks complete — the worst outcome available on this line, and the one the old
// tail-mounted coverage note produced whenever it was the thing that got dropped.
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
func fitStripFigures(label string, figures []stripFigure, width int) string {
	if len(figures) == 0 {
		return ""
	}
	join := func(n int, compact bool) string {
		parts := make([]string, 0, n)
		for _, f := range figures[:n] {
			if compact {
				parts = append(parts, f.compact)
			} else {
				parts = append(parts, f.full)
			}
		}
		return label + "  " + strings.Join(parts, stripGap)
	}
	for n := len(figures); n >= 1; n-- {
		for _, compact := range []bool{false, true} {
			if candidate := join(n, compact); lipgloss.Width(candidate) <= width {
				return candidate
			}
		}
	}
	// The label does not fit alongside even one figure. Drop the LABEL before
	// dropping the number: the figure is the information, the label is decoration,
	// and a dollar amount on its own is still unambiguous in the chrome.
	for _, only := range []string{figures[0].full, figures[0].compact} {
		if lipgloss.Width(only) <= width {
			return only
		}
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
