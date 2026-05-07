package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)

// jwtValidationConfig is the plugin's local config schema. See
// authlib/plugins/CONVENTIONS.md for the decode → applyDefaults →
// validate pattern.
type jwtValidationConfig struct {
	// Issuer is the JWT `iss` claim expected on inbound tokens.
	Issuer string `json:"issuer"`

	// JWKSURL points at the JWKS endpoint used to verify signatures.
	// When empty, derived from Issuer using Keycloak's convention
	// (/protocol/openid-connect/certs).
	JWKSURL string `json:"jwks_url"`

	// Audience is the literal audience value expected on inbound
	// tokens. One of {Audience, AudienceFile, AudienceMode:"per-host"}
	// is required.
	Audience string `json:"audience"`

	// AudienceFile reads the expected audience from a file. Used
	// together with client-registration's /shared/client-id.txt. The
	// file may not exist at Configure time; a background poll started
	// by Init waits for it and updates the plugin when it appears.
	AudienceFile string `json:"audience_file"`

	// AudienceMode chooses how the expected audience is resolved:
	// "static" (default) uses Audience/AudienceFile; "per-host" derives
	// it from pctx.Host via routing.ServiceNameFromHost (waypoint mode).
	AudienceMode string `json:"audience_mode"`

	// BypassPaths are URL path globs (see authlib/bypass) that skip
	// validation entirely.
	BypassPaths []string `json:"bypass_paths"`
}

func (c *jwtValidationConfig) applyDefaults() {
	if c.JWKSURL == "" && c.Issuer != "" {
		c.JWKSURL = strings.TrimRight(c.Issuer, "/") + "/protocol/openid-connect/certs"
	}
	if c.AudienceMode == "" {
		c.AudienceMode = "static"
	}
}

func (c *jwtValidationConfig) validate() error {
	if c.Issuer == "" {
		return errors.New("issuer is required")
	}
	if c.JWKSURL == "" {
		return errors.New("jwks_url could not be derived; set it explicitly")
	}
	switch c.AudienceMode {
	case "static":
		if c.Audience == "" && c.AudienceFile == "" {
			return errors.New("audience or audience_file is required when audience_mode=static")
		}
	case "per-host":
		// Audience derived at request time from pctx.Host — nothing to check.
	default:
		return fmt.Errorf("audience_mode must be static or per-host, got %q", c.AudienceMode)
	}
	return nil
}

// JWTValidation validates inbound JWTs. Internal state is built during
// Configure and later updated by Init's background audience-file poller
// via auth.UpdateIdentity, which is atomic with respect to in-flight
// requests.
type JWTValidation struct {
	cfg             jwtValidationConfig
	inner           *auth.Auth
	audienceDeriver func(string) string
}

// NewJWTValidation constructs an unconfigured plugin. Configure must be
// called before the pipeline accepts traffic.
func NewJWTValidation() *JWTValidation { return &JWTValidation{} }

func (p *JWTValidation) Name() string { return "jwt-validation" }

func (p *JWTValidation) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Writes: []string{"security"}}
}

// Configure decodes the plugin's config subtree, applies defaults,
// validates, and constructs the internal auth handler. If AudienceFile
// is set but the file isn't yet readable (client-registration still
// provisioning during pod boot), the handler is created with an empty
// audience and Init's goroutine fills it in when the file appears.
func (p *JWTValidation) Configure(raw json.RawMessage) error {
	var c jwtValidationConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("jwt-validation config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("jwt-validation config: %w", err)
	}
	p.cfg = c

	if c.AudienceMode == "per-host" {
		p.audienceDeriver = perHostAudienceDeriver
	}

	audience := c.Audience
	if audience == "" && c.AudienceFile != "" {
		if v, err := config.ReadCredentialFile(c.AudienceFile); err == nil {
			audience = v
		}
	}

	matcher, err := bypass.NewMatcher(c.BypassPaths)
	if err != nil {
		return fmt.Errorf("jwt-validation bypass patterns: %w", err)
	}
	verifier := validation.NewLazyJWKSVerifier(c.JWKSURL, c.Issuer)
	p.inner = auth.New(auth.Config{
		Verifier: verifier,
		Bypass:   matcher,
		Identity: auth.IdentityConfig{Audience: audience},
	})
	return nil
}

// Init starts a background poll for AudienceFile when the file wasn't
// readable during Configure. The goroutine runs until the file appears
// or ctx is cancelled.
func (p *JWTValidation) Init(ctx context.Context) error {
	if p.cfg.AudienceFile == "" || p.cfg.Audience != "" || p.inner.Ready() {
		return nil
	}
	go func() {
		v, err := config.WaitForCredentialFile(ctx, p.cfg.AudienceFile)
		if err != nil {
			slog.Warn("jwt-validation: audience_file never became available",
				"path", p.cfg.AudienceFile, "error", err)
			return
		}
		p.inner.UpdateIdentity(auth.IdentityConfig{Audience: v}, nil)
		slog.Info("jwt-validation: audience loaded from file",
			"path", p.cfg.AudienceFile, "audience", v)
	}()
	return nil
}

func (p *JWTValidation) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.inner == nil {
		return pipeline.DenyStatus(503, "upstream.unreachable", "jwt-validation not configured")
	}
	authHeader := pctx.Headers.Get("Authorization")
	path := pctx.Path
	var audience string
	if p.audienceDeriver != nil {
		audience = p.audienceDeriver(pctx.Host)
	}

	result := p.inner.HandleInbound(ctx, authHeader, path, audience)
	if result.Action == auth.ActionDeny {
		// result.DenyReason carries the specific failure (missing header,
		// audience mismatch, expired, etc.). Pick a code whose default
		// HTTP status matches what auth returned, so the fallback body is
		// meaningful even before auth.HandleInbound grows a structured
		// code of its own.
		code := "auth.unauthorized"
		if result.DenyStatus == 503 {
			code = "upstream.unreachable"
		}
		return pipeline.DenyStatus(result.DenyStatus, code, result.DenyReason)
	}
	pctx.Claims = result.Claims
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *JWTValidation) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Compile-time interface checks.
var (
	_ pipeline.Configurable = (*JWTValidation)(nil)
	_ pipeline.Initializer  = (*JWTValidation)(nil)
)

// perHostAudienceDeriver is a package-local alias so testutil.go can
// reference the same derivation function the Configure path wires up.
var perHostAudienceDeriver = routing.ServiceNameFromHost
