package tui

import (
	"encoding/json"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// showDetail loads e into the detail viewport as pretty-printed JSON and
// remembers the focused event so yank (y) can find it.
func (m *model) showDetail(e *pipeline.SessionEvent) {
	m.detailEvent = e
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		m.detailVp.SetContent("error marshaling event: " + err.Error())
		return
	}
	m.detailVp.SetContent(string(data))
	m.detailVp.GotoTop()
}
