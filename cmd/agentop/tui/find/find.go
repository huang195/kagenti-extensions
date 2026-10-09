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
// (Highlight's own walk, the table copy's ansi.Cut), and a wide rune is two of them.
package find

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

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
//
// ONE PASS OVER THE STRING. lipgloss.StyleRanges does this job by re-cutting the string from
// its start for every range, which is quadratic: one JSON string value is one line, a common
// word occurs in it thousands of times, and on a 36KB line it took 1.5s — per keystroke.
// Here the string is walked once. The SGR sequences styled puts in force are tracked since
// its last reset; inside a span they are held back, so the span is drawn in its own style
// whole, and when the span ends they are put back in force for what follows.
func Highlight(styled string, spans []Styled) string {
	if len(spans) == 0 {
		return styled
	}
	var b, active strings.Builder
	b.Grow(len(styled) + len(spans)*32)
	col, k := 0, 0
	open, closer := false, ""
	for i := 0; i < len(styled); {
		if styled[i] == 0x1b {
			n := escapeLen(styled[i:])
			seq := styled[i : i+n]
			if isSGR(seq) {
				if seq == "\x1b[0m" || seq == "\x1b[m" {
					active.Reset()
				} else {
					active.WriteString(seq)
				}
				if !open {
					b.WriteString(seq)
				}
			} else {
				b.WriteString(seq)
			}
			i += n
			continue
		}
		for k < len(spans) && spans[k].End <= col {
			k++
		}
		if !open && k < len(spans) && col >= spans[k].Start {
			var opener string
			opener, closer = styleEdges(spans[k].Style)
			b.WriteString(opener)
			open = true
		}
		r, size := utf8.DecodeRuneInString(styled[i:])
		b.WriteString(styled[i : i+size])
		col += ansi.StringWidth(string(r))
		i += size
		if open && col >= spans[k].End {
			b.WriteString(closer)
			b.WriteString(active.String())
			open = false
			k++
		}
	}
	if open {
		b.WriteString(closer)
	}
	return b.String()
}

// styleEdges is what s writes before and after the text it renders.
func styleEdges(s lipgloss.Style) (opener, closer string) {
	const mark = "\x00"
	parts := strings.SplitN(s.Render(mark), mark, 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// escapeLen is the length of the escape sequence at the start of s: a CSI sequence up to
// its final byte, else ESC and the byte after it.
func escapeLen(s string) int {
	if len(s) >= 2 && s[1] == '[' {
		for j := 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
		}
		return len(s)
	}
	return min(2, len(s))
}

func isSGR(seq string) bool { return len(seq) >= 3 && seq[1] == '[' && seq[len(seq)-1] == 'm' }
