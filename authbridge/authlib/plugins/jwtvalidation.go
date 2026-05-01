package plugins

import (
	"context"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// JWTValidation is a pipeline plugin that validates inbound JWTs.
// It delegates to auth.HandleInbound and populates pctx.Claims on success.
type JWTValidation struct {
	auth            *auth.Auth
	audienceDeriver func(string) string
}

// JWTValidationOption configures the JWTValidation plugin.
type JWTValidationOption func(*JWTValidation)

// WithAudienceDeriver sets a function that derives the expected JWT audience
// from the request host. Used in waypoint mode where audience varies per-request.
func WithAudienceDeriver(f func(string) string) JWTValidationOption {
	return func(j *JWTValidation) { j.audienceDeriver = f }
}

func NewJWTValidation(a *auth.Auth, opts ...JWTValidationOption) *JWTValidation {
	p := &JWTValidation{auth: a}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *JWTValidation) Name() string { return "jwt-validation" }

func (p *JWTValidation) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Writes: []string{"security"}}
}

func (p *JWTValidation) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	authHeader := pctx.Headers.Get("Authorization")
	path := pctx.Path

	var audience string
	if p.audienceDeriver != nil {
		audience = p.audienceDeriver(pctx.Host)
	}

	result := p.auth.HandleInbound(ctx, authHeader, path, audience)
	if result.Action == auth.ActionDeny {
		return pipeline.Action{Type: pipeline.Reject, Status: result.DenyStatus, Reason: result.DenyReason}
	}
	pctx.Claims = result.Claims
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *JWTValidation) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
