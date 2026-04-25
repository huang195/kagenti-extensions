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

func handleConfigFactory(config *config.Config) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(config)
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
