package parity

// Divergence fixtures: the same request through both shapes of one direction,
// where the shapes are expected to answer DIFFERENTLY.
//
// Everything else in this package asserts the opposite, and that is the point
// of keeping these apart. The body-size caps are not the same number on every
// listener:
//
//	direction      extproc    reverseproxy   forwardproxy
//	request body   1 MiB      1 MiB          32 MiB
//	response body  1 MiB      1 MiB          10 MiB
//
// So a body between 1 MiB and 32 MiB is served on the laptop path and refused
// on the Kubernetes path, and an Anthropic Claude Code session produces 5 MiB
// request bodies (measured in #904) — the band is where real traffic lives, not
// a corner. Until these fixtures existed nothing stated which way each shape
// goes, so neither answer was a decision and either could change silently.
//
// Why a sibling of assertParity rather than a tolerance inside it. #987
// suggested a fixture field that makes assertParity accept per-listener
// answers. A field like that is an escape hatch: the next fixture that fails on
// real drift can be made green by filling it in, in a harness whose own
// comments (parity_test.go:706, :739) exist because it once passed having
// compared nothing. assertDivergence instead never calls observationDiff, so
// there is no pairwise check to weaken — each leg is pinned absolutely against
// its own declared row — and three guards stop it being used for anything else:
// every listener needs a row, the rows must actually disagree, and each row has
// to say why.

import (
	"fmt"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
)

// divergentExpectation is one listener's whole expected answer. Every field is
// asserted on every row — there is no "unset means don't check", because a
// divergence fixture that left a field open would be hiding the field most
// likely to have moved.
type divergentExpectation struct {
	// why is required: one line on why THIS listener differs from its peer.
	// The complaint in #987 is that the gap is undocumented, so the
	// documentation is a run-time requirement rather than a comment someone
	// may forget to update.
	why string

	// pipelineRan is false when the listener refused before the pipeline. Pair
	// it with refusedPreRunBy on the fixture, which is what tells the driver to
	// expect no session event from this leg.
	pipelineRan bool

	// wireStatus is the transport's code. Compared ALWAYS, including the 0 that
	// extproc reports on a success path for want of an HTTP transport of its
	// own — an explicit 0 in a fixture says "this shape has no wire answer
	// here", which is itself a divergence worth stating.
	wireStatus int

	// eventStatusCode is SessionEvent.StatusCode: what the session API, agentop
	// and /v1/usage will show an operator. It is NOT always wireStatus — on the
	// reverse proxy's buffer refusal the event carries the 502 while the
	// upstream's own 200 moves to error.code.
	eventStatusCode int

	// errorKind and errorCode are SessionEvent.Error, with "" asserting the
	// event carried no error at all. That empty case is the sharp one: the
	// silent truncation this file pins records a clean 200 with no error, so a
	// fixture that only looked at errors would see two legs agreeing.
	//
	// Message is deliberately not here. It is prose with byte counts in it, so
	// pinning it would make these fixtures fail on a reworded log line, and
	// Kind is the field consumers actually branch on.
	errorKind string
	errorCode string

	// pluginEvents is an EXACT set of SessionEvent.Plugins: every key listed
	// must be present and byte-equal, and any key not listed is a failure. nil
	// therefore asserts the event carried no plugin payload — which is the
	// whole claim on the reverse proxy's leg below, where OnResponse never ran.
	pluginEvents map[string]string
}

// assertDivergence runs the fixture through every listener and checks each one
// against its own row. No pairwise comparison, by design — see the file comment.
func assertDivergence(t *testing.T, f fixture, wantPhase pipeline.SessionPhase, listeners []listenerRun, want map[string]divergentExpectation) {
	t.Helper()

	if len(listeners) < 2 {
		t.Fatalf("assertDivergence: fixture %q was given %d listener(s); a divergence needs at least two shapes to diverge between", f.name, len(listeners))
	}

	// Guard: a row for every listener, each with a reason. A missing row must
	// be fatal rather than defaulted, or the cheapest way to silence a failing
	// leg would be to delete its expectation.
	inSet := make(map[string]bool, len(listeners))
	for _, l := range listeners {
		inSet[l.name] = true
		e, ok := want[l.name]
		if !ok {
			t.Fatalf("assertDivergence: fixture %q declares nothing for listener %q; every listener in the set needs a row", f.name, l.name)
		}
		if e.why == "" {
			t.Fatalf("assertDivergence: fixture %q listener %q has an empty why; an unexplained divergence is the problem this file exists to fix", f.name, l.name)
		}
	}
	// Guard: a row naming a listener that is not being run would look like
	// coverage and check nothing. Typos in a map key are otherwise invisible.
	for name := range want {
		if !inSet[name] {
			t.Fatalf("assertDivergence: fixture %q declares listener %q, which is not in the set being run; nothing would have checked it", f.name, name)
		}
	}
	// Guard: the rows must actually disagree. Without this, assertDivergence
	// would be a strictly weaker assertParity that any fixture could be moved
	// to in order to drop the pairwise diff.
	if !divergentRows(listeners, want) {
		t.Fatalf("assertDivergence: fixture %q expects identical behaviour from every listener; that is parity — use assertParity, which also runs the pairwise diff", f.name)
	}

	type namedObs struct {
		listener string
		observed *observation
	}
	// Collected OUTSIDE t.Run for the reason assertParity spells out at
	// parity_test.go:706: appending inside the closure makes the whole thing
	// fail open under t.Parallel, with nothing compared and exit 0.
	got := make([]namedObs, 0, len(listeners))
	for _, l := range listeners {
		var obs *observation
		t.Run(f.name+"/"+l.name, func(t *testing.T) {
			obs = l.run(t, f, wantPhase)
			if obs == nil {
				t.Fatalf("listener %q produced no observation for fixture %q (phase=%v)", l.name, f.name, wantPhase)
			}
		})
		if obs != nil {
			got = append(got, namedObs{listener: l.name, observed: obs})
		}
	}
	// And a fixture that checked nothing is a broken test, not a pass.
	if len(got) == 0 {
		t.Fatalf("fixture %q collected no observations from %d listeners: nothing was checked", f.name, len(listeners))
	}

	for _, g := range got {
		e := want[g.listener]
		obs := g.observed
		where := fmt.Sprintf("fixture %q listener %s (%s)", f.name, g.listener, e.why)

		if obs.PipelineRan != e.pipelineRan {
			t.Errorf("%s: PipelineRan = %v, want %v", where, obs.PipelineRan, e.pipelineRan)
		}
		if obs.WireStatus != e.wireStatus {
			t.Errorf("%s: WireStatus = %d, want %d", where, obs.WireStatus, e.wireStatus)
		}
		if obs.StatusCode != e.eventStatusCode {
			t.Errorf("%s: event StatusCode = %d, want %d", where, obs.StatusCode, e.eventStatusCode)
		}
		switch {
		case e.errorKind == "" && obs.Error != nil:
			t.Errorf("%s: event carried an error it should not have: %s", where, jsonPretty(obs.Error))
		case e.errorKind != "" && obs.Error == nil:
			t.Errorf("%s: event carried no error; want kind %q code %q", where, e.errorKind, e.errorCode)
		case e.errorKind != "":
			if obs.Error.Kind != e.errorKind {
				t.Errorf("%s: error.Kind = %q, want %q", where, obs.Error.Kind, e.errorKind)
			}
			if obs.Error.Code != e.errorCode {
				t.Errorf("%s: error.Code = %q, want %q", where, obs.Error.Code, e.errorCode)
			}
		}
		for key, wantJSON := range e.pluginEvents {
			gotJSON, ok := obs.PluginEventJSON[key]
			if !ok {
				t.Errorf("%s: missing expected plugin event %q", where, key)
				continue
			}
			if !jsonEqual(gotJSON, wantJSON) {
				t.Errorf("%s: plugin event %q\n  got:  %s\n  want: %s", where, key, gotJSON, wantJSON)
			}
		}
		for key, gotJSON := range obs.PluginEventJSON {
			if _, ok := e.pluginEvents[key]; !ok {
				t.Errorf("%s: unexpected plugin event %q: %s", where, key, gotJSON)
			}
		}
	}
}

// divergentRows reports whether at least two rows differ in something other
// than their prose.
func divergentRows(listeners []listenerRun, want map[string]divergentExpectation) bool {
	first := want[listeners[0].name]
	first.why = ""
	for _, l := range listeners[1:] {
		other := want[l.name]
		other.why = ""
		if !sameRow(first, other) {
			return true
		}
	}
	return false
}

// sameRow compares two rows field by field. Hand-written rather than
// reflect.DeepEqual so that nil and empty pluginEvents maps count as the same
// claim — both mean "no plugin payload" — and a fixture cannot manufacture a
// fake divergence out of that distinction to get past the guard above.
func sameRow(a, b divergentExpectation) bool {
	if a.why != b.why || a.pipelineRan != b.pipelineRan || a.wireStatus != b.wireStatus ||
		a.eventStatusCode != b.eventStatusCode || a.errorKind != b.errorKind || a.errorCode != b.errorCode {
		return false
	}
	if len(a.pluginEvents) != len(b.pluginEvents) {
		return false
	}
	for k, v := range a.pluginEvents {
		if other, ok := b.pluginEvents[k]; !ok || !jsonEqual(v, other) {
			return false
		}
	}
	return true
}

// digestJSON is the exact payload the spy publishes for a body digest. Written
// out here rather than marshalled from bodyDigest so the JSON FIELD NAMES are
// asserted too, matching what bodyMutationJSON does for the framework's own
// event.
func digestJSON(b []byte) string {
	return fmt.Sprintf(`{"len":%d,"sha256":%q}`, len(b), sha256Hex(b))
}

// divergenceBody returns n bytes whose every 26-byte window is distinct enough
// that a PREFIX and a SUFFIX of the same length hash differently.
//
// That matters here and does not in #937's overflow fixture, which fills with a
// single repeated 'x'. These fixtures assert which 1 MiB of a 2 MiB body
// survived truncation; with a uniform fill, keeping the tail instead of the
// head would produce an identical digest and the assertion would be blind to
// the difference it is for.
func divergenceBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

// maxBufferedBody is the 1 MiB cap extproc and reverseproxy share
// (extproc/server.go:33, reverseproxy/server.go:47). Spelled here so the
// fixtures below read as "one over" / "the first cap's worth" rather than as
// bit-shift arithmetic.
const maxBufferedBody = 1 << 20

// TestDivergence_OutboundRequestBodyCap: a 2 MiB outbound request is refused
// before the pipeline by extproc (413) and carried in full by the forward
// proxy, whose request cap is 32 MiB.
//
// This is the band #904 measured real Claude Code traffic in, so the
// consequence is concrete: the same agent that works on a laptop loses every
// request over 1 MiB the moment it is deployed to Kubernetes — and loses it as
// a 413 from the sidecar, which is not a status the upstream ever sent.
func TestDivergence_OutboundRequestBodyCap(t *testing.T) {
	big := divergenceBody(2 << 20)
	f := fixture{
		name:      "outbound-request-body-cap",
		direction: pipeline.Outbound,
		// Per listener, which is the whole shape of this fixture: one leg
		// refuses pre-run while the other runs the pipeline on the same bytes.
		refusedPreRunBy: map[string]bool{"extproc": true},
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:               true,
			RecordRequestBodyDigest: true,
		})},
		method: "POST",
		path:   "/divergence/big-request",
		// An upstream is needed for the leg that does NOT refuse. The extproc
		// driver skips the response phase for a leg declared refusedPreRunBy,
		// because Envoy stops calling ext_proc after an ImmediateResponse.
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"ok":true}`),
		reqBody:        big,
	}
	assertDivergence(t, f, pipeline.SessionRequest, outboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:         "1 MiB request cap, refused at the body message before any plugin ran (extproc/server.go:220)",
			pipelineRan: false,
			wireStatus:  413,
		},
		"forwardproxy": {
			why:             "32 MiB request cap, so 2 MiB is ordinary traffic and the plugin sees all of it",
			pipelineRan:     true,
			wireStatus:      200,
			eventStatusCode: 0, // request-phase event; no status has been chosen yet
			pluginEvents: map[string]string{
				spyPluginA + bodyReqDigestStrippedSuffix: digestJSON(big),
			},
		},
	})
}

// TestDivergence_InboundResponseBodyCap: a 2 MiB upstream response on the
// inbound path. extproc TRUNCATES to 1 MiB and reports success; the reverse
// proxy refuses with a 502 it generated itself.
//
// Both halves contradict #987's prescription, which expected the two to agree
// on 502. extproc's appendBoundedBody (extproc/server.go:1248) cuts the body,
// logs a WARN and lets the pipeline run on the prefix — so the client gets its
// whole response and only the plugin's view is short. The function's own
// comment names the consequence: "a JSON body cut short parses as nothing and
// the silence would otherwise look like a response that carried no usage."
func TestDivergence_InboundResponseBodyCap(t *testing.T) {
	big := divergenceBody(2 << 20)
	f := fixture{
		name:      "inbound-response-body-cap",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:                true,
			RecordResponseBodyDigest: true,
		})},
		method:         "GET",
		path:           "/divergence/big-response",
		upstreamStatus: 200,
		upstreamBody:   big,
	}
	assertDivergence(t, f, pipeline.SessionResponse, inboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:             "1 MiB response cap enforced by truncation, so the pipeline runs on the first 1 MiB and nothing reports a problem",
			pipelineRan:     true,
			wireStatus:      0, // no HTTP transport of its own; Envoy owns the connection
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(big[:maxBufferedBody]),
			},
		},
		"reverseproxy": {
			why:             "same 1 MiB cap enforced by refusal: modifyResponse returns responseBufferError{overLimit} and the request ends 502 (reverseproxy/server.go:522)",
			pipelineRan:     true,
			wireStatus:      502,
			eventStatusCode: 502,
			errorKind:       "proxy_error",
			// The upstream's own status survives here rather than being
			// overwritten: the 502 is ours, and an operator still needs to know
			// the backend answered 200 before we dropped its response.
			errorCode: "200",
			// Nothing: the buffer check returns before RunResponse, so
			// OnResponse never ran and published no digest. The absence IS the
			// assertion — it is what distinguishes this refusal from extproc's
			// truncate-and-continue, where OnResponse runs on a short body.
			pluginEvents: nil,
		},
	})
}

// TestDivergence_OutboundResponseBodyCap: the silent one, and the reason the
// digest knob had to exist before this fixture could be written.
//
// Both legs report 200 with no error. The ONLY observable difference is how
// much of the response the plugin was handed — 1 MiB on extproc, all 2 MiB on
// the forward proxy — so on the Kubernetes path an LLM response over 1 MiB
// reaches the client intact while its token counts, and therefore its cost,
// silently become zero. Nothing in the session event, the wire status or the
// logs the operator reads says a byte went missing.
func TestDivergence_OutboundResponseBodyCap(t *testing.T) {
	big := divergenceBody(2 << 20)
	f := fixture{
		name:      "outbound-response-body-cap",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:                true,
			RecordResponseBodyDigest: true,
		})},
		method:         "GET",
		path:           "/divergence/big-response",
		upstreamStatus: 200,
		upstreamBody:   big,
	}
	assertDivergence(t, f, pipeline.SessionResponse, outboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:             "1 MiB response cap, truncated silently: the plugin sees the first 1 MiB and the event looks entirely healthy",
			pipelineRan:     true,
			wireStatus:      0,
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(big[:maxBufferedBody]),
			},
		},
		"forwardproxy": {
			why:             "10 MiB response cap (forwardproxy/server.go:79), so the plugin is handed the whole 2 MiB and can count it",
			pipelineRan:     true,
			wireStatus:      200,
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(big),
			},
		},
	})
}
