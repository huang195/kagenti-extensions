package extproc

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// TestExtProcRecordersCarryTheClient is a POPULATION guard, the same shape and for
// the same reason as TestExtProcRecordersCarryMethodAndPath: the reflection guards
// in pipeline catch "the field never reaches the wire", but a recorder that simply
// forgets Client serializes a clean nil and every other test stays green. That is
// the defect this file exists to make impossible — an unwired call site has already
// shipped once on this branch.
//
// One subtest per recorder, so a failure names the site rather than the file.
func TestExtProcRecordersCarryTheClient(t *testing.T) {
	// Fixture mirrors newPctx in httppath_test.go — each recorder needs whatever
	// makes its own gate open, and the response recorders filter invocations on
	// InvocationPhaseResponse, so the fixture carries an entry in both phases.
	// Headers is the addition: it is where ClientInfo reads the User-Agent from.
	newPctx := func(dir pipeline.Direction, phase pipeline.InvocationPhase, ua string) *pipeline.Context {
		h := http.Header{}
		if ua != "" {
			h.Set("User-Agent", ua)
		}
		return &pipeline.Context{
			Direction: dir,
			Method:    "POST",
			Host:      "api.openai.com",
			Path:      "/v1/chat/completions",
			Headers:   h,
			StartedAt: time.Now(),
			Extensions: pipeline.Extensions{
				Invocations: &pipeline.Invocations{
					Inbound: []pipeline.Invocation{{
						Plugin: "jwt-validation", Phase: phase,
						Action: pipeline.ActionAllow, Reason: "authorized",
					}},
					Outbound: []pipeline.Invocation{{
						Plugin: "token-exchange", Phase: phase,
						Action: pipeline.ActionAllow, Reason: "exchanged",
					}},
				},
			},
		}
	}

	recorders := []struct {
		name   string
		record func(s *Server, pctx *pipeline.Context)
		dir    pipeline.Direction
		phase  pipeline.InvocationPhase
	}{
		{"recordInboundSession", func(s *Server, p *pipeline.Context) { s.recordInboundSession(p) },
			pipeline.Inbound, pipeline.InvocationPhaseRequest},
		{"recordOutboundSession", func(s *Server, p *pipeline.Context) { s.recordOutboundSession(p) },
			pipeline.Outbound, pipeline.InvocationPhaseRequest},
		{"recordInboundResponseSession", func(s *Server, p *pipeline.Context) { s.recordInboundResponseSession(p) },
			pipeline.Inbound, pipeline.InvocationPhaseResponse},
		{"recordOutboundResponseSession", func(s *Server, p *pipeline.Context) { s.recordOutboundResponseSession(p) },
			pipeline.Outbound, pipeline.InvocationPhaseResponse},
		{"recordInboundReject", func(s *Server, p *pipeline.Context) {
			s.recordInboundReject(p, pipeline.Action{Type: pipeline.Reject})
		}, pipeline.Inbound, pipeline.InvocationPhaseRequest},
		{"recordOutboundReject", func(s *Server, p *pipeline.Context) {
			s.recordOutboundReject(p, pipeline.Action{Type: pipeline.Reject})
		}, pipeline.Outbound, pipeline.InvocationPhaseRequest},
	}

	firstEvent := func(t *testing.T, name string, rec func(*Server, *pipeline.Context), pctx *pipeline.Context) pipeline.SessionEvent {
		t.Helper()
		store := session.New(5*time.Minute, 100, 0)
		defer store.Close()
		rec(&Server{Sessions: store}, pctx)
		v := store.View(session.DefaultSessionID)
		if v == nil || len(v.Events) == 0 {
			t.Fatalf("%s recorded no event; cannot assert population", name)
		}
		return v.Events[0]
	}

	// BOTH DIRECTIONS IN ONE SUBTEST, per recorder. They used to be two loops, and the
	// absence half asserted only "want nil" — which a recorder that never populates Client
	// at all satisfies, so deleting the `Client:` assignment left that half green while it
	// read as coverage of the same contract. Present-then-absent through the same recorder
	// is what makes each subtest able to fail on its own: the first assertion catches an
	// unwired call site, the second catches one that fabricates a label for traffic that
	// named no agent.
	//
	// The nil's Label() is deliberately NOT asserted. Label is nil-safe by construction, so
	// `ev.Client.Label() == "unknown"` restates pipeline.EventClient.Label's own contract —
	// pinned by its "nil is unknown" table row in that package — and cannot fail for
	// anything a recorder here does or omits.
	for _, tc := range recorders {
		t.Run(tc.name, func(t *testing.T) {
			ev := firstEvent(t, tc.name, tc.record, newPctx(tc.dir, tc.phase, "claude-cli/2.1.14 (external, cli)"))
			if ev.Client == nil {
				t.Fatalf("%s: Client is nil; the recorder does not copy pctx.ClientInfo()", tc.name)
			}
			if ev.Client.Name != "claude-code" || ev.Client.Version != "2.1.14" {
				t.Errorf("%s: Client = %+v, want claude-code/2.1.14", tc.name, ev.Client)
			}

			absent := firstEvent(t, tc.name, tc.record, newPctx(tc.dir, tc.phase, ""))
			if absent.Client != nil {
				t.Errorf("%s: Client = %+v, want nil for a request with no User-Agent; an invented agent in a cost table reads as a real program that spent real money",
					tc.name, absent.Client)
			}
		})
	}
}
