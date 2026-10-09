package find_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
)

func TestContains(t *testing.T) {
	for _, tc := range []struct {
		text, q string
		want    bool
	}{
		{"api.Anthropic.com", "anthropic", true},
		{"req", "REQ", true},
		{"resp", "req", false},
		{"anything", "", false},
		{"", "x", false},
		{"İstanbul", "istanbul", true},
	} {
		if got := find.Contains(tc.text, tc.q); got != tc.want {
			t.Errorf("Contains(%q, %q) = %v, want %v", tc.text, tc.q, got, tc.want)
		}
	}
}

// TestRanges pins columns, not bytes: a wide rune is two columns, and "İ" lowers to two
// runes under strings.ToLower — an offset taken from that string would be one column off
// for every İ before the match.
func TestRanges(t *testing.T) {
	for _, tc := range []struct {
		text, q string
		want    []find.Range
	}{
		{"abcABC", "b", []find.Range{{1, 2}, {4, 5}}},
		{"aaaa", "aa", []find.Range{{0, 2}, {2, 4}}},
		{"日本語テキスト", "テキ", []find.Range{{6, 10}}},
		{"xİy", "y", []find.Range{{2, 3}}},
		{"İstanbul", "istanbul", []find.Range{{0, 8}}},
		{"abc", "", nil},
		{"ab", "abc", nil},
		{"abc", "z", nil},
	} {
		if got := find.Ranges(tc.text, tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Ranges(%q, %q) = %v, want %v", tc.text, tc.q, got, tc.want)
		}
	}
}

// TestHighlight_KeepsTheColorsEitherSide: a highlight inside colored text restyles the
// range and leaves the text before and after it in the colors it had.
func TestHighlight_KeepsTheColorsEitherSide(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000"))
	blue := lipgloss.NewStyle().Foreground(lipgloss.Color("#0000ff"))
	hl := lipgloss.NewStyle().Background(lipgloss.Color("#00ff00"))
	colored := red.Render("abc defghi") + " " + blue.Render("jkl")

	out := find.Highlight(colored, []find.Styled{{Range: find.Range{Start: 5, End: 8}, Style: hl}})
	if got, want := ansi.Strip(out), ansi.Strip(colored); got != want {
		t.Fatalf("highlight changed the text: %q, want %q", got, want)
	}
	for _, want := range []string{hl.Render("efg"), red.Render("hi"), blue.Render("jkl")} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	if got := find.Highlight(colored, nil); got != colored {
		t.Errorf("no spans changed the string: %q", got)
	}
}

// TestHighlight_IsLinearInTheLine: one JSON string value is one line, and a common word can
// occur in it thousands of times. lipgloss.StyleRanges re-cut the string from its start for
// every range, which took 1.5s on this line — per keystroke, in message detail. The bound is
// loose on purpose: linear is milliseconds, quadratic is seconds.
func TestHighlight_IsLinearInTheLine(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	plain := strings.Repeat("filler words here ", 2000)
	line := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000")).Render(plain)
	hl := lipgloss.NewStyle().Background(lipgloss.Color("#00ff00"))
	var spans []find.Styled
	for _, r := range find.Ranges(plain, "filler") {
		spans = append(spans, find.Styled{Range: r, Style: hl})
	}
	start := time.Now()
	out := find.Highlight(line, spans)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("%d spans over %d bytes took %v", len(spans), len(line), d)
	}
	if got := ansi.Strip(out); got != plain {
		t.Errorf("the text changed")
	}
}

// TestHighlight_SuppressesColorsInsideARangeAndRestoresThemAfter: a color that starts inside
// a highlight is held back until the highlight ends, then put in force, so the highlight is
// drawn whole and what follows it keeps the color it would have had.
func TestHighlight_SuppressesColorsInsideARangeAndRestoresThemAfter(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000"))
	hl := lipgloss.NewStyle().Background(lipgloss.Color("#00ff00"))
	styled := "ab" + red.Render("cdef")
	out := find.Highlight(styled, []find.Styled{{Range: find.Range{Start: 1, End: 3}, Style: hl}})
	if !strings.Contains(out, hl.Render("bc")) {
		t.Errorf("the highlight is not drawn whole: %q", out)
	}
	if !strings.Contains(out, red.Render("def")) {
		t.Errorf("the color that began inside the highlight was not restored after it: %q", out)
	}
	if got := ansi.Strip(out); got != "abcdef" {
		t.Errorf("text = %q", got)
	}
}

// TestHighlight_AtTheEndAndOnWideRunes: a range that runs to the end of the string is
// closed, and columns count a wide rune as two.
func TestHighlight_AtTheEndAndOnWideRunes(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	hl := lipgloss.NewStyle().Background(lipgloss.Color("#00ff00"))
	out := find.Highlight("日本語テキスト", []find.Styled{{Range: find.Range{Start: 6, End: 14}, Style: hl}})
	if want := "日本語" + hl.Render("テキスト"); out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}
