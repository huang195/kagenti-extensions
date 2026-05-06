package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

// Resolve applies presets, validates, and instantiates all authlib components
// from the configuration. Returns a fully wired auth.Config ready for auth.New().
func Resolve(_ context.Context, cfg *Config) (*auth.Config, error) {
	ApplyPreset(cfg)

	// Derive URLs from KEYCLOAK_URL + KEYCLOAK_REALM when explicit values are missing
	deriveKeycloakURLs(cfg)

	// Derive JWKS URL from TOKEN_URL when not explicitly set (Keycloak convention)
	deriveJWKSURL(cfg)

	// Credential files are NOT resolved here — caller handles them
	// asynchronously via ResolveCredentialFiles() after the listener starts.
	// This avoids blocking gRPC startup for up to 2 minutes.

	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	if err := ValidateOutboundPolicy(cfg.Outbound.DefaultPolicy); err != nil {
		return nil, err
	}

	// Bypass matcher
	matcher, err := bypass.NewMatcher(cfg.Bypass.InboundPaths)
	if err != nil {
		return nil, fmt.Errorf("bypass patterns: %w", err)
	}

	// JWT verifier — lazy initialization defers the JWKS HTTP fetch to the
	// first Verify() call, so the gRPC listener can start immediately.
	verifier := validation.NewLazyJWKSVerifier(cfg.Inbound.JWKSURL, cfg.Inbound.Issuer)

	// Client auth for token exchange
	clientAuth, err := ResolveClientAuth(cfg)
	if err != nil {
		return nil, fmt.Errorf("client auth: %w", err)
	}

	exchanger := exchange.NewClient(cfg.Outbound.TokenURL, clientAuth)

	// Router
	router, err := resolveRouter(cfg)
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}

	result := &auth.Config{
		Verifier:      verifier,
		Exchanger:     exchanger,
		Cache:         cache.New(),
		Bypass:        matcher,
		Router:        router,
		Identity: auth.IdentityConfig{
			ClientID: cfg.Identity.ClientID,
			Audience: cfg.Identity.ClientID, // inbound audience defaults to client ID
		},
		NoTokenPolicy: NoTokenPolicyForMode(cfg.Mode),
	}

	// Waypoint mode: derive audience from destination hostname when no route matches
	if cfg.Mode == ModeWaypoint {
		result.AudienceDeriver = routing.ServiceNameFromHost
	}

	return result, nil
}

// deriveKeycloakURLs derives TOKEN_URL and ISSUER from KEYCLOAK_URL + KEYCLOAK_REALM.
// Explicit values always take precedence.
func deriveKeycloakURLs(cfg *Config) {
	keycloakURL := strings.TrimRight(cfg.Outbound.KeycloakURL, "/")
	realm := cfg.Outbound.KeycloakRealm
	if keycloakURL == "" || realm == "" {
		return
	}
	base := keycloakURL + "/realms/" + realm
	if cfg.Outbound.TokenURL == "" {
		cfg.Outbound.TokenURL = base + "/protocol/openid-connect/token"
		slog.Info("derived token_url from keycloak_url + keycloak_realm",
			"token_url", cfg.Outbound.TokenURL)
	}
	if cfg.Inbound.Issuer == "" {
		cfg.Inbound.Issuer = base
		slog.Info("derived issuer from keycloak_url + keycloak_realm",
			"issuer", cfg.Inbound.Issuer)
	}
}

// deriveJWKSURL derives the JWKS endpoint from TOKEN_URL using Keycloak's convention:
// .../protocol/openid-connect/token → .../protocol/openid-connect/certs
// Uses suffix-based replacement to avoid modifying hostnames containing "token".
func deriveJWKSURL(cfg *Config) {
	if cfg.Inbound.JWKSURL != "" || cfg.Outbound.TokenURL == "" {
		return
	}
	if strings.HasSuffix(cfg.Outbound.TokenURL, "/token") {
		cfg.Inbound.JWKSURL = strings.TrimSuffix(cfg.Outbound.TokenURL, "/token") + "/certs"
		slog.Info("derived jwks_url from token_url", "jwks_url", cfg.Inbound.JWKSURL)
	}
}

// ResolveCredentialFiles polls for credential files and signals readiness.
// It never gives up: after initialTimeout it switches to exponential backoff.
// Call this after the server listener starts to avoid blocking startup.
func ResolveCredentialFiles(cfg *Config, initialTimeout time.Duration) <-chan struct{} {
	ready := make(chan struct{})

	needClientID := cfg.Identity.ClientIDFile != ""
	needClientSecret := cfg.Identity.ClientSecretFile != ""
	needSVID := cfg.Identity.Type == "spiffe" && cfg.Identity.JWTSVIDPath != ""

	if !needClientID && !needClientSecret && !needSVID {
		close(ready)
		return ready
	}

	go func() {
		// Phase 1: fast poll (2s interval) up to initialTimeout
		if resolveAllCredentials(cfg, needClientID, needClientSecret, needSVID, initialTimeout) {
			close(ready)
			return
		}
		slog.Warn("credential files not available after initial timeout, continuing in background",
			"timeout", initialTimeout)

		// Phase 2: exponential backoff, never gives up
		backoff := 5 * time.Second
		const maxBackoff = 60 * time.Second
		for {
			time.Sleep(backoff)
			if resolveAllCredentials(cfg, needClientID, needClientSecret, needSVID, 0) {
				close(ready)
				slog.Info("credential files loaded after extended wait")
				return
			}
			if backoff < maxBackoff {
				backoff = min(backoff*2, maxBackoff)
			}
		}
	}()
	return ready
}

// resolveAllCredentials attempts to read all needed credential files.
// With timeout=0 it does a single non-blocking check.
// Returns true only when ALL needed files have been successfully loaded.
func resolveAllCredentials(cfg *Config, needClientID, needClientSecret, needSVID bool, timeout time.Duration) bool {
	allOK := true

	if needClientID {
		if err := waitAndReadFile(cfg.Identity.ClientIDFile, &cfg.Identity.ClientID, timeout); err != nil {
			allOK = false
		}
	}
	if needClientSecret {
		if err := waitAndReadFile(cfg.Identity.ClientSecretFile, &cfg.Identity.ClientSecret, timeout); err != nil {
			allOK = false
		}
	}
	if needSVID {
		if err := waitForFile(cfg.Identity.JWTSVIDPath, timeout); err != nil {
			allOK = false
		}
	}
	return allOK
}

// waitAndReadFile polls for a file to exist, then reads its content into dest.
func waitAndReadFile(path string, dest *string, timeout time.Duration) error {
	if err := waitForFile(path, timeout); err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	*dest = strings.TrimSpace(string(content))
	slog.Info("loaded credential from file", "path", path)
	return nil
}

// waitForFile polls for a file to exist, returning nil when found or error on timeout.
// With timeout=0, performs a single non-blocking check.
func waitForFile(path string, timeout time.Duration) error {
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return nil
	}
	if timeout <= 0 {
		return fmt.Errorf("file not ready: %s", path)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return nil
		}
	}
	return fmt.Errorf("timeout waiting for %s (%v)", path, timeout)
}

// ResolveClientAuth creates the appropriate client authentication from config.
// Exported so main.go can rebuild it after credential files are loaded.
func ResolveClientAuth(cfg *Config) (exchange.ClientAuth, error) {
	switch cfg.Identity.Type {
	case "spiffe":
		if cfg.Identity.JWTSVIDPath != "" {
			source := spiffe.NewFileJWTSource(cfg.Identity.JWTSVIDPath)
			return &exchange.JWTAssertionAuth{
				ClientID:      cfg.Identity.ClientID,
				AssertionType: "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe",
				TokenSource:   source.FetchToken,
			}, nil
		}
		return nil, fmt.Errorf("spiffe identity requires jwt_svid_path (Workload API not yet supported)")

	case "client-secret":
		return &exchange.ClientSecretAuth{
			ClientID:     cfg.Identity.ClientID,
			ClientSecret: cfg.Identity.ClientSecret,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported identity type %q for client auth", cfg.Identity.Type)
	}
}

func resolveRouter(cfg *Config) (*routing.Router, error) {
	var rules []routing.Route

	// Load from file if specified
	if cfg.Routes.File != "" {
		fileRoutes, err := routing.LoadRoutes(cfg.Routes.File)
		if err != nil {
			return nil, err
		}
		rules = append(rules, fileRoutes...)
	}

	// Add inline rules, converting from RouteConfig to routing.Route
	for _, rc := range cfg.Routes.Rules {
		action := rc.Action
		if action == "" && rc.Passthrough {
			action = "passthrough" // backwards compatibility
		}
		rules = append(rules, routing.Route{
			Host:          rc.Host,
			Audience:      rc.TargetAudience,
			Scopes:        rc.TokenScopes,
			TokenEndpoint: rc.TokenURL,
			Action:        action,
		})
	}

	return routing.NewRouter(cfg.Outbound.DefaultPolicy, rules)
}
