package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSearch_InTheHelpOverlay(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	m.Update(keyRune('?'))
	typeKeys(m, "/quit")
	if !m.searching || m.searchPane != targetHelp {
		t.Fatalf("searching=%v target=%v, want the prompt open on help", m.searching, m.searchPane)
	}
	if len(m.helpDoc.matches) == 0 {
		t.Fatal("help: no match for quit")
	}
	if !strings.Contains(stripANSI(m.View()), "/ quit") {
		t.Errorf("the prompt is not on the overlay:\n%s", stripANSI(m.View()))
	}
	press(m, tea.KeyEsc)
	if m.searching || !m.helpVisible {
		t.Fatalf("Esc with the prompt open: searching=%v help=%v, want the search cancelled and help still up", m.searching, m.helpVisible)
	}
	typeKeys(m, "/quit")
	press(m, tea.KeyEnter)
	if !strings.Contains(stripANSI(m.View()), "[/quit") {
		t.Errorf("no search status on the overlay:\n%s", stripANSI(m.View()))
	}
	m.Update(keyRune('n'))
	press(m, tea.KeyEsc)
	if m.helpVisible {
		t.Fatal("Esc with the prompt closed did not close help")
	}
	if got := m.search[paneSessions]; got != "" {
		t.Errorf("help's search leaked onto the sessions pane: %q", got)
	}
	m.Update(keyRune('?'))
	if !strings.Contains(stripANSI(m.View()), "[/quit") {
		t.Error("help's search did not last until agentop exits")
	}
}

// TestSearch_InUsage: usage is drawn to fit, so its drawn lines are its text, and n moves
// the current highlight without scrolling anything.
func TestSearch_InUsage(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	m.pane = paneUsage
	m.usage.err = errUsageUnsupported
	typeKeys(m, "/aggregation")
	press(m, tea.KeyEnter)
	if len(m.usageDoc.matches) == 0 {
		t.Fatalf("usage: no match for aggregation in:\n%s", stripANSI(m.usageBody()))
	}
	m.Update(keyRune('n'))
	if r, ok := m.usageDoc.currentRow(); !ok || r != m.usageDoc.matches[0] {
		t.Errorf("n: current = %d,%v, want %d", r, ok, m.usageDoc.matches[0])
	}
}
