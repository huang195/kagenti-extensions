package tui

import (
	"encoding/json"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// showDetail loads e into the detail viewport as colorized JSON and
// remembers the focused event so yank (y) can find it.
//
// Marshal with SessionEvent.MarshalJSON first (readable wire form — string
// enums, durationMs), then hand the bytes to the colorizer which re-parses
// and re-emits with lipgloss styling. The two-step round-trip keeps the
// enum-to-string logic in one place (authlib) and lets the colorizer stay
// a pure pretty-printer.
func (m *model) showDetail(e *pipeline.SessionEvent) {
	m.detailEvent = e
	data, err := json.Marshal(e)
	if err != nil {
		m.detailVp.SetContent("error marshaling event: " + err.Error())
		return
	}
	m.detailVp.SetContent(ColorizeJSONBytes(data))
	m.detailVp.GotoTop()
}
