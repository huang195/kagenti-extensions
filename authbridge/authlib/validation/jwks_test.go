package validation

import (
	"testing"
)

func TestClaims_HasAudience(t *testing.T) {
	c := &Claims{Audience: []string{"aud-1", "aud-2"}}
	if !c.HasAudience("aud-1") {
		t.Error("expected HasAudience(aud-1) = true")
	}
	if c.HasAudience("aud-3") {
		t.Error("expected HasAudience(aud-3) = false")
	}
}

func TestClaims_HasScope(t *testing.T) {
	c := &Claims{Scopes: []string{"openid", "profile"}}
	if !c.HasScope("openid") {
		t.Error("expected HasScope(openid) = true")
	}
	if c.HasScope("email") {
		t.Error("expected HasScope(email) = false")
	}
}

func TestClaims_EmptyAudience(t *testing.T) {
	c := &Claims{}
	if c.HasAudience("anything") {
		t.Error("expected HasAudience = false on nil audience")
	}
}

// Note: Integration tests for JWKSVerifier.Verify() require a running JWKS endpoint
// (e.g., Keycloak). These are deferred to Phase 2+ when the auth layer is wired up.
// The jwx library's own test suite validates JWT parsing and JWKS resolution.
