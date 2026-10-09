package table_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// searchStyles gives every search style a colour no other style uses, so a line's escape
// bytes say which drew it. The profile is forced because lipgloss emits no escapes without
// a terminal.
func searchStyles(t *testing.T) table.Styles {
	t.Helper()
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
	s := table.DefaultStyles()
	s.Selected = lipgloss.NewStyle().Background(lipgloss.Color("#101010"))
	s.Marked = lipgloss.NewStyle().Foreground(lipgloss.Color("#123456"))
	s.Match = lipgloss.NewStyle().Background(lipgloss.Color("#00ff00"))
	s.CurrentMatch = lipgloss.NewStyle().Background(lipgloss.Color("#ff00ff"))
	return s
}

func esc(s lipgloss.Style) string { return strings.SplitN(s.Render("x"), "x", 2)[0] }

// lineFor is the screen line that shows the row labelled tok.
func lineFor(t *testing.T, tb table.Model, tok string) string {
	t.Helper()
	for _, l := range strings.Split(tb.View(), "\n") {
		if strings.Contains(stripEsc(l), tok) {
			return l
		}
	}
	t.Fatalf("no screen line shows %s:\n%s", tok, tb.View())
	return ""
}

// stripEsc removes SGR sequences, enough to find a row's label in a styled line.
func stripEsc(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func twoColTable(rows []table.Row, widths ...int) table.Model {
	cols := []table.Column{{Title: "a", Width: widths[0]}, {Title: "b", Width: widths[1]}}
	tb := table.New(table.WithColumns(cols), table.WithRows(rows), table.WithFocused(true))
	tb.SetHeight(len(rows) + 1)
	return tb
}

func TestSetSearch_MatchesRowsOnTheirCells(t *testing.T) {
	tb := newTable(5, 10)
	tb.SetSearch("r00", nil)
	if got := tb.Matches(); !reflect.DeepEqual(got, []int{0, 1, 2, 3, 4}) {
		t.Errorf("r00: Matches = %v", got)
	}
	tb.SetSearch("3", nil)
	if got := tb.Matches(); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("3: Matches = %v", got)
	}
	tb.SetSearch("", nil)
	if got := tb.Matches(); got != nil {
		t.Errorf("empty query: Matches = %v, want none", got)
	}
}

// TestSetSearch_HighlightsTheMatchedCharacters: only the matched characters are drawn in
// Match, and a row with no match is drawn in no search style at all.
func TestSetSearch_HighlightsTheMatchedCharacters(t *testing.T) {
	s := searchStyles(t)
	tb := newTable(5, 10)
	tb.SetStyles(s)
	tb.SetSearch("003", nil)
	if l := lineFor(t, tb, "r003"); !strings.Contains(l, s.Match.Render("003")) {
		t.Errorf("r003 does not draw 003 in Match: %q", l)
	}
	if l := lineFor(t, tb, "r002"); strings.Contains(l, esc(s.Match)) || strings.Contains(l, esc(s.Marked)) {
		t.Errorf("r002 does not match but is styled: %q", l)
	}
}

// TestSetSearch_TheCursorRowKeepsItsStyleAfterAHighlight: the row style used to wrap the
// joined row, so a highlight's reset ended it partway along. Each piece carries it now.
func TestSetSearch_TheCursorRowKeepsItsStyleAfterAHighlight(t *testing.T) {
	s := searchStyles(t)
	tb := twoColTable([]table.Row{{"x000-tail", "after"}, {"x001", "b"}}, 10, 6)
	tb.SetStyles(s)
	tb.SetSearch("000", nil) // the cursor is on row 0
	l := lineFor(t, tb, "x000")
	if !strings.Contains(l, s.CurrentMatch.Inherit(s.Selected).Render("000")) {
		t.Errorf("the cursor row's match is not in CurrentMatch: %q", l)
	}
	// Each piece runs on through its cell's padding: "x000-tail" fills a 10-column cell, "after"
	// a 6-column one.
	for _, piece := range []string{"-tail ", "after "} {
		if !strings.Contains(l, s.Selected.Render(piece)) {
			t.Errorf("%q after the highlight lost the Selected style: %q", piece, l)
		}
	}
}

// TestSetSearch_AMatchPastTheCutHighlightsTheEllipsis: the table truncates a cell to its
// column, and a match in what it cut off is shown by its "…".
func TestSetSearch_AMatchPastTheCutHighlightsTheEllipsis(t *testing.T) {
	s := searchStyles(t)
	tb := twoColTable([]table.Row{{"abcdefghij", "1"}, {"zz", "2"}}, 6, 3)
	tb.SetStyles(s)
	tb.SetCursor(1)
	tb.SetSearch("hij", nil)
	if got := tb.Matches(); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("Matches = %v, want [0]", got)
	}
	if l := lineFor(t, tb, "abcde"); !strings.Contains(l, s.Match.Render("…")) {
		t.Errorf("the ellipsis is not highlighted: %q", l)
	}
}

// TestSetSearch_FullValuesOverrideACellThePaneCut: a pane that truncates a cell itself
// hands the table the whole value, and that is what matches.
func TestSetSearch_FullValuesOverrideACellThePaneCut(t *testing.T) {
	s := searchStyles(t)
	tb := twoColTable([]table.Row{{"abc…", "1"}, {"zz", "2"}}, 8, 3)
	tb.SetStyles(s)
	tb.SetCursor(1)
	tb.SetSearch("def", []table.SearchRow{{Cells: []string{"abcdef", ""}}, {}})
	if got := tb.Matches(); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("Matches = %v, want [0]", got)
	}
	if l := lineFor(t, tb, "abc"); !strings.Contains(l, s.Match.Render("…")) {
		t.Errorf("the ellipsis is not highlighted: %q", l)
	}
}

// TestSetSearch_AMatchNothingOnScreenShowsMarksTheRow: a match in a column the terminal
// dropped, or one fitted to no width, has nothing visible to highlight, so the row is marked.
func TestSetSearch_AMatchNothingOnScreenShowsMarksTheRow(t *testing.T) {
	s := searchStyles(t)
	tb := twoColTable([]table.Row{{"r0", "zeta"}, {"r1", "b"}, {"r2", "c"}}, 4, 0)
	tb.SetStyles(s)
	tb.SetCursor(2)
	tb.SetSearch("zeta", nil)
	if l := lineFor(t, tb, "r0"); !strings.Contains(l, esc(s.Marked)) {
		t.Errorf("zero-width match: r0 is not marked: %q", l)
	}
	tb.SetSearch("omega", []table.SearchRow{{}, {Dropped: []string{"omega"}}, {}})
	if got := tb.Matches(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("Dropped: Matches = %v, want [1]", got)
	}
	if l := lineFor(t, tb, "r1"); !strings.Contains(l, esc(s.Marked)) || strings.Contains(l, esc(s.Match)) {
		t.Errorf("Dropped match: r1 is not marked only: %q", l)
	}
}

// TestSetRows_KeepsTheQueryAndDropsTheOverrides: a refresh that sets rows without
// SetSearch still shows the search, matched against the new rows, and overrides for the
// old rows cannot land on the new ones.
func TestSetRows_KeepsTheQueryAndDropsTheOverrides(t *testing.T) {
	tb := newTable(3, 10)
	tb.SetSearch("hidden", []table.SearchRow{{Cells: []string{"hidden"}}, {}, {}})
	if got := tb.Matches(); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("setup: Matches = %v", got)
	}
	tb.SetRows([]table.Row{{"r000"}, {"hidden"}})
	if got := tb.Matches(); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("after SetRows: Matches = %v, want [1]", got)
	}
}

// TestSetSearch_HighlightsWideText: columns, not bytes — the highlight lands on テキ.
func TestSetSearch_HighlightsWideText(t *testing.T) {
	s := searchStyles(t)
	tb := twoColTable([]table.Row{{"日本語テキスト", "1"}, {"zz", "2"}}, 16, 3)
	tb.SetStyles(s)
	tb.SetCursor(1)
	tb.SetSearch("テキ", nil)
	if l := lineFor(t, tb, "日本語"); !strings.Contains(l, s.Match.Render("テキ")) {
		t.Errorf("テキ is not the highlighted text: %q", l)
	}
}

func TestSetSearch_OnAnEmptyTable(t *testing.T) {
	tb := newTable(0, 10)
	tb.SetSearch("x", nil)
	if got := tb.Matches(); got != nil {
		t.Errorf("Matches = %v, want none", got)
	}
	_ = tb.View()
}

// TestRenderRow_TheCursorRowDrawsACellAndItsPaddingAsOneRun: a styled row's cell text and the
// padding that fills its column are one styled run, as they were when the row style wrapped the
// whole row. The README demo's generator relies on it: it canonicalises an UPDATED age together
// with the padding after it, so "9s ago" and "45s ago" leave the columns after them in place —
// and an age split from its padding escaped that, which made the committed asset depend on the
// second the capture ran.
func TestRenderRow_TheCursorRowDrawsACellAndItsPaddingAsOneRun(t *testing.T) {
	s := searchStyles(t)
	tb := twoColTable([]table.Row{{"43s ago", "x"}, {"zz", "y"}}, 12, 3)
	tb.SetStyles(s)
	if l := lineFor(t, tb, "43s ago"); !strings.Contains(l, s.Selected.Render("43s ago     ")) {
		t.Errorf("the cell's text and its padding are separate runs: %q", l)
	}
	tb.SetSearch("43", nil)
	if l := lineFor(t, tb, "43s ago"); !strings.Contains(l, s.Selected.Render("s ago     ")) {
		t.Errorf("after a highlight, the rest of the cell and its padding are separate runs: %q", l)
	}
}
