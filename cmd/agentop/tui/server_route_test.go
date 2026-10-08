package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/redact"
)

// routerYAML is a local config with the router's two servers and claude-code routed to ete.
const routerYAML = `mode: proxy-sidecar
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: inference-router
        config:
          servers:
            ete:
              url: https://ete.example.com
              key: sk-ete
            glm:
              url: https://glm.example.com:8443
              key: sk-glm
          agents:
            claude-code: ete
`

// routerRaw is routerYAML's router config as /v1/pipeline's source holds it, before redaction.
const routerRaw = `{"servers":{"ete":{"url":"https://ete.example.com","key":"sk-ete"},` +
	`"glm":{"url":"https://glm.example.com:8443","key":"sk-glm"}},"agents":{"claude-code":"ete"}}`

// routerPipeline is /v1/pipeline with the router configured as raw, its keys redacted as the
// proxy serves them.
func routerPipeline(raw string) *apiclient.PipelineView {
	return &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{
		{Name: "inference-parser", Direction: "outbound", Position: 1},
		{Name: "inference-router", Direction: "outbound", Position: 2, Config: redact.JSON(json.RawMessage(raw))},
	}}
}

// fakeProxy is this machine's Cortex as S sees it: a config file, and one server answering
// /reload/status and /v1/pipeline. A proxy that reloads serves the router from the file; one
// that refuses reports a failed reload and keeps serving what it started with.
//
// The refusal is the one edit.PollUntilReloaded reads as PollFailure, so edit.WritePluginConfig
// ends it as WriteReloadFailed and puts the file back: the first poll is the baseline
// (reloads_failed 0), and the second reports one failure past it. last_success stays an hour
// old throughout, so no poll can read the refused write as a reload. It never stops answering,
// so it never models WriteStatusUnreachable.
type fakeProxy struct {
	srv    *httptest.Server
	path   string
	refuse bool
	polls  atomic.Int32
}

func newFakeProxy(t *testing.T, refuse bool) *fakeProxy {
	t.Helper()
	f := &fakeProxy{path: filepath.Join(t.TempDir(), "config.yaml"), refuse: refuse}
	if err := os.WriteFile(f.path, []byte(routerYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/pipeline":
			raw := json.RawMessage(routerRaw)
			if !f.refuse {
				cfg, err := config.Load(f.path)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				raw = cfg.Pipeline.Outbound.Plugins[1].Config
			}
			_ = json.NewEncoder(w).Encode(routerPipeline(string(raw)))
		case "/reload/status":
			st := edit.ReloadStatus{LastSuccess: time.Now(), ReloadsOK: 1}
			if f.refuse {
				st.LastSuccess = time.Now().Add(-time.Hour)
				if f.polls.Add(1) >= 2 {
					st.ReloadsFailed, st.LastError = 1, `configure "inference-router": refused`
				}
			}
			_ = json.NewEncoder(w).Encode(st)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProxy) config(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// serverModel is a model attached to f as this machine's Cortex, on the AGENTS pane with
// claude-code (routed to ete) and opencode (not routed).
func serverModel(t *testing.T, f *fakeProxy) *model {
	t.Helper()
	m := &model{
		ctx:                context.Background(),
		pane:               paneAgents,
		previousPane:       paneNone,
		pipelineReturnPane: paneNone,
		agentsTbl:          newAgentsTable(),
		sessionsTbl:        newSessionsTable(),
		client:             apiclient.New(f.srv.URL),
		width:              120,
		height:             40,
		localEndpoint:      f.srv.URL,
		localConfigPath:    f.path,
		localStatsURL:      f.srv.URL,
		pipeline:           routerPipeline(routerRaw),
		agents: []agentRow{
			{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10, PricedRequests: 10}},
			{label: "opencode/1.0.3", Counts: usage.Counts{Requests: 4}},
		},
	}
	m.rebuildAgentsTable()
	return m
}

// onRow puts the AGENTS cursor on the row labelled label, All agents being "".
func onRow(t *testing.T, m *model, label string) {
	t.Helper()
	for i, l := range m.agentRowLabels {
		if l == label {
			m.agentsTbl.SetCursor(i)
			return
		}
	}
	t.Fatalf("no AGENTS row %q in %q", label, m.agentRowLabels)
}

// agentsCell is the cell under the column titled title in the AGENTS row labelled label, and
// whether the column is there at all.
func agentsCell(t *testing.T, m *model, label, title string) (string, bool) {
	t.Helper()
	col := -1
	for i, c := range m.agentsTbl.Columns() {
		if headerTitle(c) == title {
			col = i
		}
	}
	if col < 0 {
		return "", false
	}
	for i, l := range m.agentRowLabels {
		if l == label {
			return m.agentsTbl.Rows()[i][col], true
		}
	}
	t.Fatalf("no AGENTS row %q", label)
	return "", false
}

// The column is there only while the proxy runs the router, and says each agent's server — or
// own choice — with nothing on All agents.
func TestAgentsPane_ServerColumnFollowsTheRouter(t *testing.T) {
	m := serverModel(t, newFakeProxy(t, false))
	for label, want := range map[string]string{"": "", "claude-code/2.1.270": "ete", "opencode/1.0.3": agentOwnChoice} {
		if got, ok := agentsCell(t, m, label, "SERVER"); !ok || got != want {
			t.Errorf("row %q: SERVER = %q (present %v), want %q", label, got, ok, want)
		}
	}
	m.pipeline = &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{{Name: "inference-parser"}}}
	m.rebuildAgentsTable()
	if _, ok := agentsCell(t, m, "claude-code/2.1.270", "SERVER"); ok {
		t.Error("SERVER is shown with no inference-router in the pipeline")
	}
}

// The SERVER cells carry no ANSI: the table truncates an escape as text. "own choice" is the
// dimming, in words.
func TestServerCells_CarryNoANSI(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
	m := serverModel(t, newFakeProxy(t, false))
	for r, row := range m.agentsTbl.Rows() {
		for c, cell := range row {
			if strings.ContainsRune(cell, 0x1b) {
				t.Errorf("AGENTS row %d cell %d carries an escape: %q", r, c, cell)
			}
		}
	}
}

// The pane's rows bring the pipeline with them, so a switch made in a shell — `agentop server
// use` — shows on the next refresh, and S opens on the value the proxy runs.
func TestAgentsPane_ARowsRefreshAlsoRefetchesThePipeline(t *testing.T) {
	f := newFakeProxy(t, false)
	m := serverModel(t, f)
	routed := strings.Replace(routerYAML, "            claude-code: ete\n", "            claude-code: ete\n            opencode: glm\n", 1)
	if err := os.WriteFile(f.path, []byte(routed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, cmd := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenNever})
	if cmd == nil {
		t.Fatal("the rows' reply fetched no pipeline")
	}
	m.Update(cmd())
	if got, _ := agentsCell(t, m, "opencode/1.0.3", "SERVER"); got != "glm" {
		t.Errorf("SERVER = %q after the refresh, want glm", got)
	}
}
