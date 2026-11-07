package forwardproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// destWriter sets the hairpin headers itself, as any plugin can.
type destWriter struct{ dest string }

func (destWriter) Name() string { return "dest-writer" }
func (destWriter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Description: "test"}
}
func (w destWriter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Headers.Set(spikeDestHeader, w.dest)
	pctx.Headers.Set(spikeTokenHeader, "forged")
	return pipeline.Action{Type: pipeline.Continue}
}
func (destWriter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Checklist item 1: the hop changes only the dial target. A validated redirect
// picks X-Cortex-Dest; a plugin that writes pctx.Host or the hairpin headers does
// not; Host stays the real destination; the row records the real host.
func TestSpikeHairpin_OnlyTheDialTargetMoves(t *testing.T) {
	a := newOrigin(t, "FROM-A", (*httptest.Server).Start)
	b := newOrigin(t, "FROM-B", (*httptest.Server).Start)
	c := newOrigin(t, "FROM-C", (*httptest.Server).Start)

	var gotDest, gotToken, gotHost string
	praxis := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDest, gotToken, gotHost = r.Header.Get(spikeDestHeader), r.Header.Get(spikeTokenHeader), r.Host
		_, _ = io.WriteString(w, "FROM-PRAXIS")
	}))
	defer praxis.Close()
	oldAddr, oldToken := spikeHairpinAddr, spikeHairpinToken
	spikeHairpinAddr, spikeHairpinToken = mustParseURL(praxis.URL).Host, "secret"
	defer func() { spikeHairpinAddr, spikeHairpinToken = oldAddr, oldToken }()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	proxy := newRedirectProxy(t, store, nil,
		&redirectPlugin{target: b.URL}, hostWriter{host: c.authority()}, destWriter{dest: c.URL})

	resp, err := proxyClient(proxy, nil).Get(a.URL + "/x")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "FROM-PRAXIS" {
		t.Fatalf("body %q: the request did not go to the hairpin", body)
	}
	if gotDest != "http://"+b.authority() || gotToken != "secret" || gotHost != b.authority() {
		t.Errorf("hairpin saw dest %q token %q Host %q, want %q %q %q",
			gotDest, gotToken, gotHost, "http://"+b.authority(), "secret", b.authority())
	}
	for _, ev := range eventsOf(t, store) {
		if ev.Phase == pipeline.SessionRequest && ev.Host != b.authority() {
			t.Errorf("request row host %q, want %q", ev.Host, b.authority())
		}
	}
}
