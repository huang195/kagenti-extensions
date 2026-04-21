package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/cache"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/exchange"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

// IdentityConfig holds the agent's identity for audience validation and token exchange.
type IdentityConfig struct {
	ClientID string // agent's OAuth client ID
	Audience string // expected inbound JWT audience (usually same as ClientID)
}

// ActorTokenSource provides actor tokens for RFC 8693 Section 4.1 act claim chaining.
// Returns ("", nil) when no actor token is available.
type ActorTokenSource func(ctx context.Context) (string, error)

// AudienceDeriver derives a target audience from a request host.
// Used by waypoint mode to auto-derive audience from the destination service name.
// Returns "" if no derivation is possible (falls back to route config).
type AudienceDeriver func(host string) string

// Config holds the resolved dependencies for the auth layer.
type Config struct {
	Verifier         validation.Verifier
	Exchanger        *exchange.Client
	Cache            *cache.Cache
	Bypass           *bypass.Matcher
	Router           *routing.Router
	Identity         IdentityConfig
	NoTokenPolicy    string           // NoTokenClientCredentials, NoTokenAllow, or NoTokenDeny
	ActorTokenSource ActorTokenSource // optional, for act claim chaining
	AudienceDeriver  AudienceDeriver  // optional, derives audience from host (waypoint mode)
	Logger           *slog.Logger
}

// Auth composes authlib building blocks into inbound validation and outbound exchange.
type Auth struct {
	verifier         validation.Verifier
	exchanger        *exchange.Client
	cache            *cache.Cache
	bypass           *bypass.Matcher
	router           *routing.Router
	identity         atomic.Pointer[IdentityConfig]
	noTokenPolicy    string
	actorTokenSource ActorTokenSource
	audienceDeriver  AudienceDeriver
	log              *slog.Logger
}

// New creates an Auth instance from resolved configuration.
func New(cfg Config) *Auth {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	a := &Auth{
		verifier:         cfg.Verifier,
		exchanger:        cfg.Exchanger,
		cache:            cfg.Cache,
		bypass:           cfg.Bypass,
		router:           cfg.Router,
		noTokenPolicy:    cfg.NoTokenPolicy,
		actorTokenSource: cfg.ActorTokenSource,
		audienceDeriver:  cfg.AudienceDeriver,
		log:              logger,
	}
	id := cfg.Identity
	a.identity.Store(&id)
	return a
}

// UpdateIdentity updates the agent's identity and exchanger credentials
// after credential files have been resolved. This is called from a background
// goroutine after the gRPC listener has started.
func (a *Auth) UpdateIdentity(id IdentityConfig, clientAuth exchange.ClientAuth) {
	a.identity.Store(&id)
	if clientAuth != nil {
		a.exchanger.UpdateAuth(clientAuth)
	}
	a.log.Info("identity updated", "client_id", id.ClientID)
}

// HandleInbound validates an inbound request's JWT token.
// audience overrides the default expected audience when non-empty. This supports
// waypoint mode where audience is derived per-request from the destination host.
// For envoy-sidecar and proxy-sidecar modes, pass "" to use the configured default.
func (a *Auth) HandleInbound(ctx context.Context, authHeader, path, audience string) *InboundResult {
	// 1. Bypass check
	if a.bypass != nil && a.bypass.Match(path) {
		a.log.Debug("bypass path matched", "path", path)
		return &InboundResult{Action: ActionAllow}
	}

	// 2. Extract bearer token
	if authHeader == "" {
		a.log.Debug("inbound denied: no Authorization header", "path", path)
		return &InboundResult{
			Action:     ActionDeny,
			DenyStatus: http.StatusUnauthorized,
			DenyReason: "missing Authorization header",
		}
	}
	token := extractBearer(authHeader)
	if token == "" {
		a.log.Debug("inbound denied: malformed Authorization header", "path", path)
		return &InboundResult{
			Action:     ActionDeny,
			DenyStatus: http.StatusUnauthorized,
			DenyReason: "invalid Authorization header format",
		}
	}

	// 3. Validate JWT
	if a.verifier == nil {
		return &InboundResult{
			Action:     ActionDeny,
			DenyStatus: http.StatusUnauthorized,
			DenyReason: "inbound validation not configured",
		}
	}
	if audience == "" {
		audience = a.identity.Load().Audience
	}
	a.log.Debug("validating inbound JWT", "path", path, "expectedAudience", audience)
	claims, err := a.verifier.Verify(ctx, token, audience)
	if err != nil {
		// Log full error at Info; log detailed context at Debug.
		// Generic message returned to client to avoid leaking details.
		a.log.Info("JWT validation failed", "error", err)
		a.log.Debug("JWT validation details",
			"path", path,
			"expectedAudience", audience,
			"expectedIssuer", a.identity.Load().Audience,
			"error", err)
		return &InboundResult{
			Action:     ActionDeny,
			DenyStatus: http.StatusUnauthorized,
			DenyReason: "token validation failed",
		}
	}

	// 4. Allow with claims
	a.log.Debug("inbound authorized",
		"path", path,
		"subject", claims.Subject,
		"clientID", claims.ClientID,
		"audience", claims.Audience,
		"scopes", claims.Scopes)
	return &InboundResult{Action: ActionAllow, Claims: claims}
}

// HandleOutbound processes an outbound request, performing token exchange if needed.
func (a *Auth) HandleOutbound(ctx context.Context, authHeader, host string) *OutboundResult {
	// 1. Resolve route
	var resolved *routing.ResolvedRoute
	if a.router != nil {
		resolved = a.router.Resolve(host)
	}

	// 2. Passthrough
	if resolved == nil {
		a.log.Debug("outbound passthrough: no matching route", "host", host)
		return &OutboundResult{Action: ActionAllow}
	}
	if resolved.Passthrough {
		a.log.Debug("outbound passthrough: route configured as passthrough", "host", host)
		return &OutboundResult{Action: ActionAllow}
	}

	// 3. Determine audience/scopes
	audience := resolved.Audience
	scopes := resolved.Scopes

	// If no audience from route and deriver is set, derive from host (waypoint pattern)
	if audience == "" && a.audienceDeriver != nil {
		audience = a.audienceDeriver(host)
		a.log.Debug("audience derived from host", "host", host, "audience", audience)
	}

	a.log.Debug("outbound exchange requested",
		"host", host, "audience", audience, "scopes", scopes,
		"hasSubjectToken", authHeader != "")

	// 4. Extract bearer token
	subjectToken := extractBearer(authHeader)

	if subjectToken == "" {
		// No token — apply no-token policy
		a.log.Debug("no subject token, applying no-token policy",
			"policy", a.noTokenPolicy, "host", host, "audience", audience)
		return a.handleNoToken(ctx, audience, scopes)
	}

	// 5. Cache check
	if a.cache != nil {
		if cached, ok := a.cache.Get(subjectToken, audience); ok {
			a.log.Debug("outbound cache hit", "host", host, "audience", audience)
			return &OutboundResult{Action: ActionReplaceToken, Token: cached}
		}
	}

	// 6. Token exchange
	if a.exchanger == nil {
		a.log.Warn("exchanger not configured, passing through",
			"host", host, "audience", audience)
		return &OutboundResult{Action: ActionAllow}
	}

	// Obtain actor token for act claim chaining (RFC 8693 Section 4.1)
	var actorToken string
	if a.actorTokenSource != nil {
		var err error
		actorToken, err = a.actorTokenSource(ctx)
		if err != nil {
			a.log.Warn("failed to obtain actor token, proceeding without",
				"error", err, "host", host)
		}
	}

	resp, err := a.exchanger.Exchange(ctx, &exchange.ExchangeRequest{
		SubjectToken:  subjectToken,
		Audience:      audience,
		Scopes:        scopes,
		ActorToken:    actorToken,
		TokenEndpoint: resolved.TokenEndpoint, // per-route override
	})
	if err != nil {
		a.log.Info("token exchange failed", "host", host, "error", err)
		a.log.Debug("token exchange failure details",
			"host", host,
			"audience", audience,
			"scopes", scopes,
			"hasActorToken", actorToken != "",
			"tokenEndpoint", resolved.TokenEndpoint,
			"error", err)
		return &OutboundResult{
			Action:     ActionDeny,
			DenyStatus: http.StatusServiceUnavailable,
			DenyReason: "token exchange failed",
		}
	}

	// 7. Cache result
	if a.cache != nil && resp.ExpiresIn > 0 {
		a.cache.Set(subjectToken, audience, resp.AccessToken,
			time.Duration(resp.ExpiresIn)*time.Second)
	}

	a.log.Debug("outbound token exchanged",
		"host", host, "audience", audience, "expiresIn", resp.ExpiresIn)
	return &OutboundResult{Action: ActionReplaceToken, Token: resp.AccessToken}
}

func (a *Auth) handleNoToken(ctx context.Context, audience, scopes string) *OutboundResult {
	switch a.noTokenPolicy {
	case NoTokenPolicyAllow:
		a.log.Debug("no token, policy=allow")
		return &OutboundResult{Action: ActionAllow}

	case NoTokenPolicyClientCredentials:
		if a.exchanger == nil {
			a.log.Debug("no token, client_credentials requested but exchanger not configured",
				"audience", audience)
			return &OutboundResult{
				Action:     ActionDeny,
				DenyStatus: http.StatusServiceUnavailable,
				DenyReason: "exchanger not configured for client credentials",
			}
		}
		a.log.Debug("no token, falling back to client_credentials",
			"audience", audience, "scopes", scopes)
		resp, err := a.exchanger.ClientCredentials(ctx, audience, scopes)
		if err != nil {
			a.log.Info("client credentials grant failed", "error", err)
			a.log.Debug("client credentials failure details",
				"audience", audience, "scopes", scopes, "error", err)
			return &OutboundResult{
				Action:     ActionDeny,
				DenyStatus: http.StatusServiceUnavailable,
				DenyReason: "client credentials token acquisition failed",
			}
		}
		return &OutboundResult{Action: ActionReplaceToken, Token: resp.AccessToken}

	default: // NoTokenDeny or unknown
		a.log.Debug("no token, policy denies request",
			"policy", a.noTokenPolicy, "audience", audience)
		return &OutboundResult{
			Action:     ActionDeny,
			DenyStatus: http.StatusUnauthorized,
			DenyReason: "missing Authorization header",
		}
	}
}

func extractBearer(authHeader string) string {
	// RFC 7235: auth scheme is case-insensitive
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return authHeader[7:]
	}
	return ""
}
