package plugins

import (
	"context"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// TokenExchange is a pipeline plugin that performs outbound token exchange.
// It delegates to auth.HandleOutbound and mutates pctx.Headers on token replacement.
type TokenExchange struct {
	auth *auth.Auth
}

func NewTokenExchange(a *auth.Auth) *TokenExchange {
	return &TokenExchange{auth: a}
}

func (p *TokenExchange) Name() string { return "token-exchange" }

func (p *TokenExchange) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}

func (p *TokenExchange) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	authHeader := pctx.Headers.Get("Authorization")
	host := pctx.Host

	result := p.auth.HandleOutbound(ctx, authHeader, host)
	switch result.Action {
	case auth.ActionDeny:
		return pipeline.Action{Type: pipeline.Reject, Status: result.DenyStatus, Reason: result.DenyReason}
	case auth.ActionReplaceToken:
		pctx.Headers.Set("Authorization", "Bearer "+result.Token)
	}
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *TokenExchange) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
