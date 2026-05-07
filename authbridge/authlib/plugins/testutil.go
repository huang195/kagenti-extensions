package plugins

import (
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// The helpers below exist so listener tests in sibling packages
// (extproc, extauthz, forwardproxy, reverseproxy) can build pipelines
// around a pre-configured *auth.Auth without going through Configure.
// Production code paths must go through Configure — these are not an
// alternate public API.
//
// If you find yourself reaching for these outside tests, something is
// wrong: plugins build their own auth.Auth from their local config.

// NewJWTValidationForTest returns a JWTValidation plugin wrapping a
// pre-built *auth.Auth. Skips the config decode + JWKS / bypass
// construction that Configure does from plugin config.
func NewJWTValidationForTest(a *auth.Auth, audienceFromHost bool) *JWTValidation {
	p := &JWTValidation{inner: a}
	if audienceFromHost {
		p.cfg.AudienceMode = "per-host"
		p.audienceDeriver = perHostAudienceDeriver
	}
	return p
}

// NewTokenExchangeForTest returns a TokenExchange plugin wrapping a
// pre-built *auth.Auth.
func NewTokenExchangeForTest(a *auth.Auth) *TokenExchange {
	return &TokenExchange{inner: a}
}

// BuildForTest constructs a pipeline from pre-built plugins directly.
// Bypasses registry lookup and Configure so listener tests can inject
// stubs.
func BuildForTest(plugins []pipeline.Plugin, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	return pipeline.New(plugins, opts...)
}
