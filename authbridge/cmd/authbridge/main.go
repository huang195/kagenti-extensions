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
	"strings"
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

// logLevel is the dynamic log level, togglable at runtime via SIGUSR1.
var logLevel = new(slog.LevelVar)

func initLogging() {
	// LOG_LEVEL env var sets the initial level: debug, info, warn, error.
	// Default: info. Override at runtime with SIGUSR1 (toggles debug/info).
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))
}

func startSignalToggle() {
	// SIGUSR1 toggles between info and debug at runtime, regardless of
	// the initial LOG_LEVEL (warn/error are treated as "not debug").
	// Usage: kubectl exec <pod> -c authbridge-proxy -- kill -USR1 1
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
			if logLevel.Level() == slog.LevelDebug {
				logLevel.Set(slog.LevelInfo)
				slog.Info("log level toggled to INFO (send SIGUSR1 to switch back to DEBUG)")
			} else {
				logLevel.Set(slog.LevelDebug)
				slog.Info("log level toggled to DEBUG (send SIGUSR1 to switch back to INFO)")
			}
		}
	}()
}

func main() {
	mode := flag.String("mode", "", "deployment mode: envoy-sidecar, waypoint, proxy-sidecar")
	configPath := flag.String("config", "", "path to config YAML file")
	flag.Parse()

	initLogging()
	startSignalToggle()

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

	// Resolve config into auth dependencies.
	// Credential files and JWKS are resolved lazily — the gRPC listener
	// starts immediately so Envoy can connect without waiting.
	resolved, err := config.Resolve(ctx, cfg)
	if err != nil {
		log.Fatalf("resolving config: %v", err)
	}
	handler := auth.New(*resolved)

	// Track servers for graceful shutdown
	var grpcServers []*grpc.Server
	var httpServers []*http.Server

	// Start listeners FIRST — before credential resolution
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

	slog.Info("authbridge starting", "mode", cfg.Mode, "logLevel", logLevel.Level().String())

	// Resolve credentials in background — doesn't block the listener.
	// Once credential files are available, update the handler's identity
	// and exchanger so token exchange requests use the loaded credentials.
	go func() {
		slog.Info("resolving credentials in background...")
		config.ResolveCredentialFiles(cfg)
		// Safe to read cfg.Identity here — ResolveCredentialFiles completed
		// and this is the only goroutine that writes these fields.
		clientAuth, err := config.ResolveClientAuth(cfg)
		if err != nil {
			slog.Warn("failed to resolve client auth after credential load", "error", err)
			return
		}
		handler.UpdateIdentity(auth.IdentityConfig{
			ClientID: cfg.Identity.ClientID,
			Audience: cfg.Identity.ClientID,
		}, clientAuth)
	}()

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
