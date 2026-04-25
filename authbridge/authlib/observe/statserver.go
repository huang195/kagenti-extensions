package observe

import (
	"context"
	"encoding/json"
	"fmt"
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
		fmt.Fprint(w, `
<html>
  <body>
    <ul>
    <li><a href="/config">Kagenti AuthBridge configuration</a>
    <li><a href="/stats">Kagenti AuthBridge statistics</a>
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
		json.NewEncoder(w).Encode(config.Config{
			Mode:     cfg.Mode,
			Inbound:  cfg.Inbound,
			Outbound: cfg.Outbound,
			Identity: config.IdentityConfig{
				Type:             cfg.Identity.Type,
				ClientID:         cfg.Identity.ClientID,
				ClientSecret:     "*redacted*",
				ClientIDFile:     cfg.Identity.ClientIDFile,
				ClientSecretFile: cfg.Identity.ClientSecretFile,
				SocketPath:       cfg.Identity.SocketPath,
				JWTSVIDPath:      cfg.Identity.JWTSVIDPath,
				JWTAudience:      cfg.Identity.JWTAudience,
			},
			Listener: cfg.Listener,
			Bypass:   cfg.Bypass,
			Routes:   cfg.Routes,
			Stats:    cfg.Stats,
		})
	}
}

func handleStatsFactory(stats *auth.Stats) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	}
}

func (s *StatServer) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *StatServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
