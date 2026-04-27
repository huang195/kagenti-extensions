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

func NewStatServer(addr string, config *config.Config, stats *auth.Stats) *StatServer {
	mux := http.NewServeMux()

	mux.HandleFunc("/config", handleConfigFactory(config))
	mux.HandleFunc("/stats", handleStatsFactory(stats))

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
		// Rather than outputting the entire config,
		// customize the output to redact the client secret.
		err := json.NewEncoder(w).Encode(config.Config{
			Mode:     cfg.Mode,
			Inbound:  cfg.Inbound,
			Outbound: cfg.Outbound,
			Identity: config.IdentityConfig{
				Type: cfg.Identity.Type,
				// We report the ClientID unredacted.  In Kagenti, the ID will be something like
				// "spiffe://localtest.me/ns/team1/sa/my-weather-service-with-authbridge"
				// Although a brute force attack is possible, showing the ClientID here does
				// not introduce new security concerns, as an attacker can already construct
				// the ClientID from the pod's namespace and name, available in the UI.
				ClientID:         cfg.Identity.ClientID,
				ClientSecret:     "*redacted*",
				ClientIDFile:     "*redacted*",
				ClientSecretFile: "*redacted*",
				SocketPath:       "*redacted*",
				JWTSVIDPath:      "*redacted*",
				JWTAudience:      cfg.Identity.JWTAudience,
			},
			Listener: cfg.Listener,
			Bypass:   cfg.Bypass,
			Routes:   cfg.Routes,
			Stats:    cfg.Stats,
		})
		if err != nil {
			slog.Default().Info("Failed to send configuration", "err", err)
		}
	}
}

func handleStatsFactory(stats *auth.Stats) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
