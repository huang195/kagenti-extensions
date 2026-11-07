package forwardproxy

// SPIKE (#1491 W1). Throwaway code: nothing on spike/hairpin-w1 merges.
//
// CORTEX_SPIKE_HAIRPIN=127.0.0.1:<praxis> turns on the dial-only hop: after the
// pipeline, a non-skipped request is dialed to Praxis with its path and Host
// unchanged, and its real destination in X-Cortex-Dest. CORTEX_SPIKE_EGRESS starts
// the egress listener that Praxis's catch-all cluster points at (see
// SpikeEgressHandler).

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

const (
	spikeDestHeader  = "X-Cortex-Dest"
	spikeTokenHeader = "X-Cortex-Token"
)

var (
	spikeHairpinAddr  = os.Getenv("CORTEX_SPIKE_HAIRPIN")
	spikeHairpinToken = os.Getenv("CORTEX_SPIKE_HAIRPIN_TOKEN")
	// No Proxy field: the hop never reads the proxy env (BL4).
	spikeHairpinClient = &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 100, IdleConnTimeout: 90 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

// spikeHairpin rewrites r's dial target to Praxis and returns the client to send it
// with. It reads only r.URL, which the pipeline cannot write except through a
// validated redirect, and it sets its headers after the pipeline's header sync, so
// no plugin can steer it.
func spikeHairpin(r *http.Request, chain []attribution.Proc) (*http.Client, bool) {
	if spikeHairpinAddr == "" || spikePraxisProc(chain) {
		return nil, false
	}
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	host, port := r.URL.Hostname(), r.URL.Port()
	if port == "" {
		port = "80"
		if r.URL.Scheme == "https" {
			port = "443"
		}
	}
	r.Header.Set(spikeDestHeader, r.URL.Scheme+"://"+net.JoinHostPort(host, port))
	r.Header.Set(spikeTokenHeader, spikeHairpinToken)
	r.URL.Scheme, r.URL.Host = "http", spikeHairpinAddr
	return spikeHairpinClient, true
}

// spikePraxisProc is the peerproc self-traffic exemption (BL4): a request from the
// Praxis process itself is never sent back to Praxis.
func spikePraxisProc(chain []attribution.Proc) bool {
	for _, p := range chain {
		if b := filepath.Base(p.Exe); b == "praxis" || b == "praxis-ai" {
			return true
		}
	}
	return false
}

// SpikeEgressHandler is the egress listener: it checks the token, moves the request
// to X-Cortex-Dest, strips both headers and relays the stream. It records nothing.
func SpikeEgressHandler(token string) http.Handler {
	upstream, err := tlsbridge.NewUpstreamClient(nil, false)
	if err != nil {
		panic(err)
	}
	rp := &httputil.ReverseProxy{
		Transport:     upstream.Transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			dest := pr.In.Header.Get(spikeDestHeader)
			scheme, authority, _ := strings.Cut(dest, "://")
			pr.Out.URL.Scheme, pr.Out.URL.Host = scheme, authority
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del(spikeDestHeader)
			pr.Out.Header.Del(spikeTokenHeader)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("spike-egress: upstream failed", "dest", r.Header.Get(spikeDestHeader), "error", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dest := r.Header.Get(spikeDestHeader)
		if token == "" || r.Header.Get(spikeTokenHeader) != token || !strings.Contains(dest, "://") {
			slog.Warn("spike-egress: refused", "remote", r.RemoteAddr, "dest", dest)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		start := time.Now()
		rp.ServeHTTP(w, r)
		slog.Info("spike-egress: done", "dest", dest, "path", r.URL.Path, "ms", time.Since(start).Milliseconds(), "ctx_err", r.Context().Err())
	})
}
