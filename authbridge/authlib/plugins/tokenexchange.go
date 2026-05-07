package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
)

// tokenExchangeConfig is the plugin's local config schema. See
// authlib/plugins/CONVENTIONS.md for the pattern.
type tokenExchangeConfig struct {
	// TokenURL is the OAuth token endpoint. Explicit value wins; else
	// derived from KeycloakURL + KeycloakRealm using Keycloak's
	// convention.
	TokenURL string `json:"token_url"`

	// KeycloakURL and KeycloakRealm are a convenience for deriving
	// TokenURL when the operator prefers to supply Keycloak base + realm
	// rather than the full token endpoint.
	KeycloakURL   string `json:"keycloak_url"`
	KeycloakRealm string `json:"keycloak_realm"`

	// DefaultPolicy is applied when a request's host matches no route:
	// "passthrough" (default) forwards the request unchanged;
	// "exchange" attempts a client-credentials exchange with an empty
	// audience (usually fails — kept for rare use cases where the IdP
	// allows it).
	DefaultPolicy string `json:"default_policy"`

	// NoTokenPolicy controls how the plugin handles outbound requests
	// that arrive without a bearer token: "client-credentials" does an
	// unprompted client_credentials exchange; "allow" forwards
	// unchanged; "deny" rejects. Default varies by mode — see
	// config.NoTokenPolicyForMode.
	NoTokenPolicy string `json:"no_token_policy"`

	// Identity carries client credentials used for token exchange.
	Identity tokenExchangeIdentity `json:"identity"`

	// Routes drives host-to-audience matching. A host that matches no
	// route falls through to DefaultPolicy.
	Routes tokenExchangeRoutes `json:"routes"`

	// AudienceFromHost — when true, requests with no matching route use
	// routing.ServiceNameFromHost(host) as the target audience. Used in
	// waypoint mode.
	AudienceFromHost bool `json:"audience_from_host"`
}

type tokenExchangeIdentity struct {
	// Type is one of "spiffe" or "client-secret".
	Type string `json:"type"`

	// ClientID identifies the client in Keycloak. Explicit value wins;
	// else read from ClientIDFile at Configure time (or by Init if the
	// file isn't yet available).
	ClientID     string `json:"client_id"`
	ClientIDFile string `json:"client_id_file"`

	// ClientSecret / ClientSecretFile are the client-secret credentials
	// (type=client-secret).
	ClientSecret     string `json:"client_secret"`
	ClientSecretFile string `json:"client_secret_file"`

	// JWTSVIDPath is the file path where spiffe-helper writes the
	// JWT-SVID (type=spiffe).
	JWTSVIDPath string `json:"jwt_svid_path"`
}

type tokenExchangeRoutes struct {
	// File is an optional path to a routes.yaml file (see
	// authlib/routing.LoadRoutes).
	File string `json:"file"`

	// Rules are inline route entries; combined with routes loaded from
	// File.
	Rules []tokenExchangeRoute `json:"rules"`
}

type tokenExchangeRoute struct {
	Host           string `json:"host"`
	TargetAudience string `json:"target_audience"`
	TokenScopes    string `json:"token_scopes"`
	TokenURL       string `json:"token_url"`
	Action         string `json:"action"`
	// Passthrough is accepted for backwards compatibility with the
	// pre-migration routes.yaml shape (Action:"passthrough" is preferred).
	Passthrough bool `json:"passthrough"`
}

func (c *tokenExchangeConfig) applyDefaults() {
	if c.TokenURL == "" && c.KeycloakURL != "" && c.KeycloakRealm != "" {
		base := strings.TrimRight(c.KeycloakURL, "/") + "/realms/" + c.KeycloakRealm
		c.TokenURL = base + "/protocol/openid-connect/token"
	}
	if c.DefaultPolicy == "" {
		c.DefaultPolicy = "passthrough"
	}
	if c.NoTokenPolicy == "" {
		c.NoTokenPolicy = auth.NoTokenPolicyDeny
	}
}

func (c *tokenExchangeConfig) validate() error {
	if c.TokenURL == "" {
		return errors.New("token_url is required (or set keycloak_url + keycloak_realm)")
	}
	switch c.DefaultPolicy {
	case "exchange", "passthrough":
	default:
		return fmt.Errorf("default_policy must be exchange or passthrough, got %q", c.DefaultPolicy)
	}
	switch c.NoTokenPolicy {
	case auth.NoTokenPolicyAllow, auth.NoTokenPolicyDeny, auth.NoTokenPolicyClientCredentials:
	default:
		return fmt.Errorf("no_token_policy must be allow, deny, or client-credentials, got %q", c.NoTokenPolicy)
	}
	switch c.Identity.Type {
	case "spiffe":
		if c.Identity.JWTSVIDPath == "" {
			return errors.New("identity.type=spiffe requires identity.jwt_svid_path")
		}
		if c.Identity.ClientID == "" && c.Identity.ClientIDFile == "" {
			return errors.New("identity.type=spiffe requires identity.client_id or identity.client_id_file")
		}
	case "client-secret":
		if c.Identity.ClientID == "" && c.Identity.ClientIDFile == "" {
			return errors.New("identity.type=client-secret requires identity.client_id or identity.client_id_file")
		}
		if c.Identity.ClientSecret == "" && c.Identity.ClientSecretFile == "" {
			return errors.New("identity.type=client-secret requires identity.client_secret or identity.client_secret_file")
		}
	case "":
		return errors.New("identity.type is required (spiffe or client-secret)")
	default:
		return fmt.Errorf("unknown identity.type %q", c.Identity.Type)
	}
	return nil
}

// TokenExchange performs outbound token exchange. Configure builds the
// internal exchanger / router / auth handler; Init polls for credential
// files that weren't available at Configure time and swaps them in via
// auth.UpdateIdentity.
type TokenExchange struct {
	cfg   tokenExchangeConfig
	inner *auth.Auth
}

// NewTokenExchange constructs an unconfigured plugin.
func NewTokenExchange() *TokenExchange { return &TokenExchange{} }

func (p *TokenExchange) Name() string { return "token-exchange" }

func (p *TokenExchange) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (p *TokenExchange) Configure(raw json.RawMessage) error {
	var c tokenExchangeConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("token-exchange config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("token-exchange config: %w", err)
	}
	p.cfg = c

	// Best-effort synchronous credential load. Missing files are
	// tolerated; Init will retry.
	if c.Identity.ClientID == "" && c.Identity.ClientIDFile != "" {
		if v, err := config.ReadCredentialFile(c.Identity.ClientIDFile); err == nil {
			p.cfg.Identity.ClientID = v
		}
	}
	if c.Identity.ClientSecret == "" && c.Identity.ClientSecretFile != "" {
		if v, err := config.ReadCredentialFile(c.Identity.ClientSecretFile); err == nil {
			p.cfg.Identity.ClientSecret = v
		}
	}

	clientAuth, err := p.buildClientAuth()
	if err != nil {
		return fmt.Errorf("token-exchange: %w", err)
	}

	exchanger := exchange.NewClient(c.TokenURL, clientAuth)

	router, err := p.buildRouter()
	if err != nil {
		return fmt.Errorf("token-exchange routes: %w", err)
	}

	authCfg := auth.Config{
		Verifier:      nil, // token-exchange doesn't validate inbound
		Exchanger:     exchanger,
		Cache:         cache.New(),
		Router:        router,
		Identity:      auth.IdentityConfig{ClientID: p.cfg.Identity.ClientID},
		NoTokenPolicy: c.NoTokenPolicy,
	}
	if c.AudienceFromHost {
		authCfg.AudienceDeriver = routing.ServiceNameFromHost
	}
	p.inner = auth.New(authCfg)
	return nil
}

func (p *TokenExchange) buildClientAuth() (exchange.ClientAuth, error) {
	switch p.cfg.Identity.Type {
	case "spiffe":
		source := spiffe.NewFileJWTSource(p.cfg.Identity.JWTSVIDPath)
		return &exchange.JWTAssertionAuth{
			ClientID:      p.cfg.Identity.ClientID,
			AssertionType: "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe",
			TokenSource:   source.FetchToken,
		}, nil
	case "client-secret":
		return &exchange.ClientSecretAuth{
			ClientID:     p.cfg.Identity.ClientID,
			ClientSecret: p.cfg.Identity.ClientSecret,
		}, nil
	default:
		return nil, fmt.Errorf("unknown identity.type %q", p.cfg.Identity.Type)
	}
}

func (p *TokenExchange) buildRouter() (*routing.Router, error) {
	var rules []routing.Route
	if p.cfg.Routes.File != "" {
		fileRoutes, err := routing.LoadRoutes(p.cfg.Routes.File)
		if err != nil {
			return nil, err
		}
		rules = append(rules, fileRoutes...)
	}
	for _, rc := range p.cfg.Routes.Rules {
		action := rc.Action
		if action == "" && rc.Passthrough {
			action = "passthrough"
		}
		rules = append(rules, routing.Route{
			Host:          rc.Host,
			Audience:      rc.TargetAudience,
			Scopes:        rc.TokenScopes,
			TokenEndpoint: rc.TokenURL,
			Action:        action,
		})
	}
	return routing.NewRouter(p.cfg.DefaultPolicy, rules)
}

// Init polls for credential files that weren't available during
// Configure. When both client_id and client_secret (or jwt_svid) become
// available, it reconstructs the client-auth and calls UpdateIdentity
// so in-flight exchanges pick up the new credentials.
func (p *TokenExchange) Init(ctx context.Context) error {
	needID := p.cfg.Identity.ClientID == "" && p.cfg.Identity.ClientIDFile != ""
	needSecret := p.cfg.Identity.ClientSecret == "" && p.cfg.Identity.ClientSecretFile != ""
	if !needID && !needSecret {
		return nil
	}
	go p.pollCredentials(ctx, needID, needSecret)
	return nil
}

func (p *TokenExchange) pollCredentials(ctx context.Context, needID, needSecret bool) {
	if needID {
		v, err := config.WaitForCredentialFile(ctx, p.cfg.Identity.ClientIDFile)
		if err != nil {
			slog.Warn("token-exchange: client_id_file never became available",
				"path", p.cfg.Identity.ClientIDFile, "error", err)
			return
		}
		p.cfg.Identity.ClientID = v
	}
	if needSecret {
		v, err := config.WaitForCredentialFile(ctx, p.cfg.Identity.ClientSecretFile)
		if err != nil {
			slog.Warn("token-exchange: client_secret_file never became available",
				"path", p.cfg.Identity.ClientSecretFile, "error", err)
			return
		}
		p.cfg.Identity.ClientSecret = v
	}
	clientAuth, err := p.buildClientAuth()
	if err != nil {
		slog.Warn("token-exchange: failed to rebuild client auth after credential load", "error", err)
		return
	}
	p.inner.UpdateIdentity(
		auth.IdentityConfig{ClientID: p.cfg.Identity.ClientID},
		clientAuth,
	)
	slog.Info("token-exchange: credentials loaded", "client_id", p.cfg.Identity.ClientID)
}

func (p *TokenExchange) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.inner == nil {
		return pipeline.DenyStatus(503, "upstream.unreachable", "token-exchange not configured")
	}
	authHeader := pctx.Headers.Get("Authorization")
	host := pctx.Host

	result := p.inner.HandleOutbound(ctx, authHeader, host)
	switch result.Action {
	case auth.ActionDeny:
		// Outbound denials almost always come from failed token exchange
		// at the IdP (upstream unreachable, bad credentials, audience
		// refused). The auth layer returns the HTTP status it wants to
		// expose; pick the closest well-known code for the body.
		code := "upstream.token-exchange-failed"
		if result.DenyStatus == http.StatusForbidden {
			code = "policy.forbidden"
		}
		return pipeline.DenyStatus(result.DenyStatus, code, result.DenyReason)
	case auth.ActionReplaceToken:
		pctx.Headers.Set("Authorization", "Bearer "+result.Token)
	}
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *TokenExchange) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Compile-time interface checks.
var (
	_ pipeline.Configurable = (*TokenExchange)(nil)
	_ pipeline.Initializer  = (*TokenExchange)(nil)
)
