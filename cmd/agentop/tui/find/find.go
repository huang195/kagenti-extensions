// Package find is agentop's text matcher: the one place that decides what a search query
// matches and where, for the table copy and the TUI alike.
//
// Matching is a case-insensitive substring, folded one rune at a time with unicode.ToLower
// rather than with strings.ToLower. strings.ToLower is not length-preserving — "İ" lowers to
// two runes — so an offset found in the lowered string is not an offset into the original,
// and a highlight drawn from it lands on the wrong characters. Folding rune by rune keeps the
// two the same length, rune for rune.
//
// Positions are screen columns, not bytes: highlighting cuts styled text by column
// (lipgloss.StyleRanges, ansi.Cut), and a wide rune is two of them.
package find

import (
	"slices"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Range is the screen columns [Start, End) of one occurrence.
type Range struct{ Start, End int }

// Styled is a Range and the style it is drawn in.
type Styled struct {
	Range
	Style lipgloss.Style
}

func fold(s string) []rune {
	r := []rune(s)
	for i, c := range r {
		r[i] = unicode.ToLower(c)
	}
	return r
}

// Contains reports whether q occurs in text, folded as Ranges folds. An empty q occurs in
// nothing: no query marks no row.
func Contains(text, q string) bool {
	if q == "" {
		return false
	}
	return strings.Contains(string(fold(text)), string(fold(q)))
}

// Ranges is every occurrence of q in text, left to right and not overlapping, in screen
// columns. text must be plain; strip styled text first.
func Ranges(text, q string) []Range {
	if q == "" {
		return nil
	}
	t, p := fold(text), fold(q)
	if len(p) > len(t) {
		return nil
	}
	cols := make([]int, len(t)+1)
	for i, r := range []rune(text) {
		cols[i+1] = cols[i] + ansi.StringWidth(string(r))
	}
	var out []Range
	for i := 0; i+len(p) <= len(t); {
		if slices.Equal(t[i:i+len(p)], p) {
			out = append(out, Range{cols[i], cols[i+len(p)]})
			i += len(p)
			continue
		}
		i++
	}
	return out
}

// Highlight draws each span of styled in its own style and leaves the escape sequences
// around it alone, so a highlight inside colored text keeps the colors either side. Spans
// are in styled's visible columns, ascending and not overlapping — the order Ranges returns.
func Highlight(styled string, spans []Styled) string {
	if len(spans) == 0 {
		return styled
	}
	rs := make([]lipgloss.Range, len(spans))
	for i, s := range spans {
		rs[i] = lipgloss.NewRange(s.Start, s.End, s.Style)
	}
	return lipgloss.StyleRanges(styled, rs...)
}
