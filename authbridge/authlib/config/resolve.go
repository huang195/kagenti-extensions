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
func Resolve(ctx context.Context, cfg *Config) (*auth.Config, error) {
	ApplyPreset(cfg)

	// Derive URLs from KEYCLOAK_URL + KEYCLOAK_REALM when explicit values are missing
	deriveKeycloakURLs(cfg)

	// Derive JWKS URL from TOKEN_URL when not explicitly set (Keycloak convention)
	deriveJWKSURL(cfg)

	// Wait for and load credentials from files when configured
	if err := resolveCredentialFiles(cfg); err != nil {
		return nil, fmt.Errorf("credential files: %w", err)
	}

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

	// JWT verifier
	verifier, err := validation.NewJWKSVerifier(ctx, cfg.Inbound.JWKSURL, cfg.Inbound.Issuer)
	if err != nil {
		return nil, fmt.Errorf("JWKS verifier: %w", err)
	}

	// Client auth for token exchange
	clientAuth, err := resolveClientAuth(cfg)
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

// resolveCredentialFiles waits for and reads credential files when configured.
// This handles the startup race with client-registration and spiffe-helper.
func resolveCredentialFiles(cfg *Config) error {
	if cfg.Identity.ClientIDFile != "" {
		if err := waitAndReadFile(cfg.Identity.ClientIDFile, &cfg.Identity.ClientID, 60*time.Second); err != nil {
			slog.Warn("client_id_file not available, using client_id from config", "error", err)
		}
	}
	if cfg.Identity.ClientSecretFile != "" {
		if err := waitAndReadFile(cfg.Identity.ClientSecretFile, &cfg.Identity.ClientSecret, 60*time.Second); err != nil {
			slog.Warn("client_secret_file not available, using client_secret from config", "error", err)
		}
	}
	// Wait for JWT-SVID file if SPIFFE identity is configured
	if cfg.Identity.Type == "spiffe" && cfg.Identity.JWTSVIDPath != "" {
		if err := waitForFile(cfg.Identity.JWTSVIDPath, 60*time.Second); err != nil {
			slog.Warn("jwt_svid_path not available at startup, will be read on each exchange", "error", err)
		}
	}
	return nil
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
func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for %s (%v)", path, timeout)
}

func resolveClientAuth(cfg *Config) (exchange.ClientAuth, error) {
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
