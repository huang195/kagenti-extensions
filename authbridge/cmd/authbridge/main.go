// Command authbridge is a unified auth proxy supporting three deployment modes:
// envoy-sidecar (ext_proc), waypoint (ext_authz + forward proxy), and
// proxy-sidecar (reverse proxy + forward proxy).
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/authbridge/listener/extauthz"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/authbridge/listener/extproc"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/authbridge/listener/forwardproxy"
	"github.com/kagenti/kagenti-extensions/authbridge/cmd/authbridge/listener/reverseproxy"
)

func main() {
	mode := flag.String("mode", "", "deployment mode: envoy-sidecar, waypoint, proxy-sidecar")
	configPath := flag.String("config", "", "path to config YAML file")
	flag.Parse()

	if *configPath == "" {
		log.Fatal("--config is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if *mode != "" {
		cfg.Mode = *mode // flag overrides YAML
	}

	// Resolve config into auth dependencies
	resolved, err := config.Resolve(ctx, cfg)
	if err != nil {
		log.Fatalf("resolving config: %v", err)
	}
	handler := auth.New(*resolved)

	slog.Info("authbridge starting", "mode", cfg.Mode)

	// Track servers for graceful shutdown
	var grpcServers []*grpc.Server
	var httpServers []*http.Server

	// Start listeners based on mode
	switch cfg.Mode {
	case config.ModeEnvoySidecar:
		grpcServers = append(grpcServers, startGRPCExtProc(handler, cfg.Listener.ExtProcAddr))

	case config.ModeWaypoint:
		grpcServers = append(grpcServers, startGRPCExtAuthz(handler, cfg.Listener.ExtAuthzAddr))
		httpServers = append(httpServers, startHTTPServer("forward-proxy", forwardproxy.NewServer(handler).Handler(), cfg.Listener.ForwardProxyAddr))

	case config.ModeProxySidecar:
		rpSrv, err := reverseproxy.NewServer(handler, cfg.Listener.ReverseProxyBackend)
		if err != nil {
			log.Fatalf("creating reverse proxy: %v", err)
		}
		httpServers = append(httpServers, startHTTPServer("reverse-proxy", rpSrv.Handler(), cfg.Listener.ReverseProxyAddr))
		httpServers = append(httpServers, startHTTPServer("forward-proxy", forwardproxy.NewServer(handler).Handler(), cfg.Listener.ForwardProxyAddr))

	default:
		log.Fatalf("unhandled mode %q", cfg.Mode)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("shutting down", "signal", sig)

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	for _, srv := range grpcServers {
		// GracefulStop blocks until all RPCs complete. If streams are long-lived
		// (e.g., ext_proc), fall back to hard Stop after the shutdown timeout.
		go func(s *grpc.Server) {
			<-shutdownCtx.Done()
			s.Stop()
		}(srv)
		srv.GracefulStop()
	}
	for _, srv := range httpServers {
		srv.Shutdown(shutdownCtx)
	}
}

func startGRPCExtProc(handler *auth.Auth, addr string) *grpc.Server {
	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, &extproc.Server{Auth: handler})
	registerHealth(srv)

	go func() {
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("ext_proc listen %s: %v", addr, err)
		}
		slog.Info("ext_proc gRPC listening", "addr", addr)
		if err := srv.Serve(lis); err != nil {
			log.Fatalf("ext_proc serve: %v", err)
		}
	}()
	return srv
}

func startGRPCExtAuthz(handler *auth.Auth, addr string) *grpc.Server {
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, &extauthz.Server{Auth: handler})
	registerHealth(srv)

	go func() {
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("ext_authz listen %s: %v", addr, err)
		}
		slog.Info("ext_authz gRPC listening", "addr", addr)
		if err := srv.Serve(lis); err != nil {
			log.Fatalf("ext_authz serve: %v", err)
		}
	}()
	return srv
}

func startHTTPServer(name string, handler http.Handler, addr string) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("HTTP server listening", "name", name, "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("%s serve: %v", name, err)
		}
	}()
	return srv
}

func registerHealth(srv *grpc.Server) {
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
}
