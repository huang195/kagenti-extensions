package table_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// markedStyles gives Marked and Selected colours no other style uses, so a line's escape
// bytes say which of the two drew it. The profile is forced because lipgloss emits no
// escapes without a terminal, and then every line would read as unstyled.
func markedStyles(t *testing.T) (table.Styles, string, string) {
	t.Helper()
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	s := table.DefaultStyles()
	s.Marked = lipgloss.NewStyle().Foreground(lipgloss.Color("#123456"))
	s.Selected = lipgloss.NewStyle().Foreground(lipgloss.Color("#654321"))
	markedEsc := strings.SplitN(s.Marked.Render("x"), "x", 2)[0]
	selectedEsc := strings.SplitN(s.Selected.Render("x"), "x", 2)[0]
	if markedEsc == "" || selectedEsc == "" || markedEsc == selectedEsc {
		t.Fatalf("styles do not tell marked from selected: %q %q", markedEsc, selectedEsc)
	}
	return s, markedEsc, selectedEsc
}

// lineFor is the screen line that shows the row labelled tok.
func lineFor(t *testing.T, tb table.Model, tok string) string {
	t.Helper()
	for _, l := range strings.Split(tb.View(), "\n") {
		if strings.Contains(l, tok) {
			return l
		}
	}
	t.Fatalf("no screen line shows %s:\n%s", tok, tb.View())
	return ""
}

// TestSetMarked_DrawsMarkedRowsInTheMarkedStyle is the search highlight: every marked row is
// drawn in Marked, an unmarked row in neither style, and the cursor's row in Selected even
// when it is marked too, so the current match still reads as the cursor.
func TestSetMarked_DrawsMarkedRowsInTheMarkedStyle(t *testing.T) {
	s, markedEsc, selectedEsc := markedStyles(t)
	tb := newTable(5, 10)
	tb.SetStyles(s)
	tb.SetMarked([]int{1, 3, 0})

	for _, tok := range []string{"r001", "r003"} {
		if l := lineFor(t, tb, tok); !strings.Contains(l, markedEsc) {
			t.Errorf("marked row %s is not drawn in Marked: %q", tok, l)
		}
	}
	for _, tok := range []string{"r002", "r004"} {
		if l := lineFor(t, tb, tok); strings.Contains(l, markedEsc) || strings.Contains(l, selectedEsc) {
			t.Errorf("unmarked row %s is styled: %q", tok, l)
		}
	}
	if l := lineFor(t, tb, "r000"); !strings.Contains(l, selectedEsc) || strings.Contains(l, markedEsc) {
		t.Errorf("the cursor's row is marked too, and must be drawn in Selected only: %q", l)
	}
}

// TestSetMarked_NilClearsAndOutOfRangeIsIgnored: a search that stops matching clears its
// marks, and a mark past the last row (rows shrank before the caller re-marked) is not an error.
func TestSetMarked_NilClearsAndOutOfRangeIsIgnored(t *testing.T) {
	s, markedEsc, _ := markedStyles(t)
	tb := newTable(3, 10)
	tb.SetStyles(s)
	tb.SetMarked([]int{2, 7, -1})
	if l := lineFor(t, tb, "r002"); !strings.Contains(l, markedEsc) {
		t.Fatalf("row 2 is not marked: %q", l)
	}
	tb.SetMarked(nil)
	if l := lineFor(t, tb, "r002"); strings.Contains(l, markedEsc) {
		t.Errorf("SetMarked(nil) left row 2 marked: %q", l)
	}
}
