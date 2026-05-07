package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
)

type StatServer struct {
	server *http.Server
}

// StatsProvider returns a fresh *auth.Stats per /stats request. The
// host typically implements this by calling auth.MergeStats over the
// per-plugin stats collected from each pipeline — see
// plugins.CollectStats. Called per HTTP request, so implementations
// should be cheap (a few map copies).
type StatsProvider func() *auth.Stats

// NewStatServer builds the stat HTTP server. The statsProvider is
// invoked per /stats request so per-plugin counters can be aggregated
// live rather than captured at process start.
func NewStatServer(addr string, config *config.Config, statsProvider StatsProvider) *StatServer {
	mux := http.NewServeMux()

	mux.HandleFunc("/config", handleConfigFactory(config))
	mux.HandleFunc("/stats", handleStatsFactory(statsProvider))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `
<!DOCTYPE html>
<html>
  <body>
    <ul>
    <li><a href="/config">Kagenti AuthBridge configuration</a></li>
    <li><a href="/stats">Kagenti AuthBridge statistics</a></li>
    </ul>
  </body>
</html>`)
	})

	return &StatServer{
		server: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

func handleConfigFactory(cfg *config.Config) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Plugin config subtrees are captured verbatim as json.RawMessage
		// by the PluginEntry unmarshaler. Operators shouldn't put
		// secrets in the runtime config — the per-plugin convention is
		// to reference a file path instead (client_secret_file, etc.) —
		// so we render the config as-is. If a plugin ever needs to
		// suppress a known-sensitive field here, it can be added to a
		// redaction pass in a follow-up.
		err := json.NewEncoder(w).Encode(cfg)
		if err != nil {
			slog.Default().Info("Failed to send configuration", "err", err)
		}
	}
}

func handleStatsFactory(provider StatsProvider) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Provider returns a freshly-merged *auth.Stats. Nil means
		// "no source plugins" — render an empty object rather than
		// failing, so the endpoint shape is stable even on pipelines
		// that register no stats sources.
		stats := provider()
		if stats == nil {
			stats = auth.NewStats()
		}
		err := json.NewEncoder(w).Encode(stats)
		if err != nil {
			slog.Default().Info("Failed to send stats", "err", err)
		}
	}
}

func (s *StatServer) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *StatServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
