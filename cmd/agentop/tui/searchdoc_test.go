package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestSearchDoc_FindsAMatchTheWrapSplits: matching is on the line before it is wrapped.
func TestSearchDoc_FindsAMatchTheWrapSplits(t *testing.T) {
	var d searchDoc
	d.setLines([]string{"the quick brown fox jumps"}, 10)
	out := d.render("quick brown")
	if !slices.Equal(d.matches, []int{0}) {
		t.Fatalf("matches = %v, want [0], the row the match starts on", d.matches)
	}
	if got, want := ansi.Strip(out), ansi.Wrap("the quick brown fox jumps", 10, " -"); got != want {
		t.Errorf("highlighting changed the wrap:\n%q\nwant\n%q", got, want)
	}
}

// TestSearchDoc_PutsAMatchOnTheRowItStartsOn: one long JSON string is one line and many
// rows; the match is on its own row, not the line's first.
func TestSearchDoc_PutsAMatchOnTheRowItStartsOn(t *testing.T) {
	long := `  "completion": "` + strings.Repeat("filler words here ", 30) + `needle at the end",`
	var d searchDoc
	d.setLines([]string{"{", long, "}"}, 24)
	d.render("needle")
	rows := strings.Split(ansi.Wrap(long, 24, " -"), "\n")
	want := -1
	for k, r := range rows {
		if strings.Contains(r, "needle") {
			want = 1 + k // "{" is row 0
		}
	}
	if !slices.Equal(d.matches, []int{want}) {
		t.Errorf("matches = %v, want [%d]", d.matches, want)
	}
}

// TestSearchDoc_PlacesAWideMatchOnItsRow: wide runes are two columns, and the row a match
// starts on is counted in columns.
func TestSearchDoc_PlacesAWideMatchOnItsRow(t *testing.T) {
	line := strings.Repeat("日本語 ", 10) + "テキスト"
	var d searchDoc
	d.setLines([]string{line}, 12)
	d.render("テキ")
	rows := strings.Split(ansi.Wrap(line, 12, " -"), "\n")
	want := -1
	for k, r := range rows {
		if strings.Contains(r, "テキ") {
			want = k
		}
	}
	if !slices.Equal(d.matches, []int{want}) {
		t.Errorf("matches = %v, want [%d]", d.matches, want)
	}
}

// TestSearchDoc_KeepsTheJSONColorsAroundAHighlight, on the colorizer's own output.
func TestSearchDoc_KeepsTheJSONColorsAroundAHighlight(t *testing.T) {
	forceColour(t)
	lines := strings.Split(ColorizeJSONBytes([]byte(`{"model":"claude-opus-5-5","stream":true}`)), "\n")
	var d searchDoc
	d.setLines(lines, 0)
	out := d.render("opus")
	if !strings.Contains(out, styleMatch.Render("opus")) {
		t.Errorf("opus is not highlighted: %q", out)
	}
	if !strings.Contains(out, styleJSONBool.Render("true")) {
		t.Errorf("the value after the match lost its color: %q", out)
	}
}

// TestSearchDoc_TheCurrentRowSurvivesARedrawOfTheSameText: layout() redraws the detail on
// every prompt open and close; a new message or a new wrap width is a new document.
func TestSearchDoc_TheCurrentRowSurvivesARedrawOfTheSameText(t *testing.T) {
	forceColour(t)
	lines := []string{"alpha needle", "beta", "gamma needle"}
	var d searchDoc
	d.setLines(lines, 0)
	d.render("needle")
	d.setCurrent(2)
	if !strings.Contains(d.render("needle"), styleCurrentMatch.Render("needle")) {
		t.Fatal("the current row is not drawn in styleCurrentMatch")
	}
	d.setLines(slices.Clone(lines), 0)
	if r, ok := d.currentRow(); !ok || r != 2 {
		t.Errorf("same text: current = %d,%v, want 2", r, ok)
	}
	d.setLines(lines, 40)
	if _, ok := d.currentRow(); ok {
		t.Error("a new wrap width kept the current row")
	}
	d.setLines([]string{"other"}, 40)
	if _, ok := d.currentRow(); ok {
		t.Error("new text kept the current row")
	}
}
