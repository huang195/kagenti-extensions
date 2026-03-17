package resolver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStaticResolver_NoConfigFile(t *testing.T) {
	r, err := NewStaticResolver("/nonexistent/path/routes.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	config, err := r.Resolve(context.Background(), "any-host.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config != nil {
		t.Errorf("expected nil config for missing file, got %+v", config)
	}
}

func TestStaticResolver_NoMatch(t *testing.T) {
	yaml := `
- host: "service-a.example.com"
  target_audience: "audience-a"
`
	r := resolverFromYAML(t, yaml)

	config, err := r.Resolve(context.Background(), "other-service.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config != nil {
		t.Errorf("expected nil config for non-matching host, got %+v", config)
	}
}

func TestStaticResolver_ExactMatch(t *testing.T) {
	yaml := `
- host: "service-a.example.com"
  target_audience: "audience-a"
  token_scopes: "openid scope-a"
`
	r := resolverFromYAML(t, yaml)

	config, err := r.Resolve(context.Background(), "service-a.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config == nil {
		t.Fatal("expected config, got nil")
	}
	if config.Audience != "audience-a" {
		t.Errorf("expected audience 'audience-a', got %q", config.Audience)
	}
	if config.Scopes != "openid scope-a" {
		t.Errorf("expected scopes 'openid scope-a', got %q", config.Scopes)
	}
}

func TestStaticResolver_GlobSingleLevel(t *testing.T) {
	yaml := `
- host: "*.example.com"
  target_audience: "wildcard-audience"
`
	r := resolverFromYAML(t, yaml)

	tests := []struct {
		host    string
		matches bool
	}{
		{"foo.example.com", true},
		{"bar.example.com", true},
		{"foo.bar.example.com", false}, // * doesn't cross '.' separator
		{"example.com", false},
	}

	for _, tc := range tests {
		config, err := r.Resolve(context.Background(), tc.host)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", tc.host, err)
		}
		if tc.matches && config == nil {
			t.Errorf("expected %q to match, got nil", tc.host)
		}
		if !tc.matches && config != nil {
			t.Errorf("expected %q to not match, got %+v", tc.host, config)
		}
	}
}

func TestStaticResolver_GlobMultiLevel(t *testing.T) {
	yaml := `
- host: "**.example.com"
  target_audience: "super-wildcard"
`
	r := resolverFromYAML(t, yaml)

	tests := []struct {
		host    string
		matches bool
	}{
		{"foo.example.com", true},
		{"foo.bar.example.com", true}, // ** crosses '.' separator
		{"a.b.c.example.com", true},
		{"example.com", false},
	}

	for _, tc := range tests {
		config, err := r.Resolve(context.Background(), tc.host)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", tc.host, err)
		}
		if tc.matches && config == nil {
			t.Errorf("expected %q to match, got nil", tc.host)
		}
		if !tc.matches && config != nil {
			t.Errorf("expected %q to not match, got %+v", tc.host, config)
		}
	}
}

func TestStaticResolver_FirstMatchWins(t *testing.T) {
	yaml := `
- host: "specific.example.com"
  target_audience: "specific"
- host: "*.example.com"
  target_audience: "wildcard"
`
	r := resolverFromYAML(t, yaml)

	// Specific match should win because it comes first
	config, err := r.Resolve(context.Background(), "specific.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config == nil {
		t.Fatal("expected config, got nil")
	}
	if config.Audience != "specific" {
		t.Errorf("expected 'specific', got %q", config.Audience)
	}

	// Other hosts match wildcard
	config, err = r.Resolve(context.Background(), "other.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config == nil {
		t.Fatal("expected config, got nil")
	}
	if config.Audience != "wildcard" {
		t.Errorf("expected 'wildcard', got %q", config.Audience)
	}
}

func TestStaticResolver_OrderMatters(t *testing.T) {
	// If wildcard comes first, it wins even for specific hosts
	yaml := `
- host: "*.example.com"
  target_audience: "wildcard"
- host: "specific.example.com"
  target_audience: "specific"
`
	r := resolverFromYAML(t, yaml)

	config, err := r.Resolve(context.Background(), "specific.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config == nil {
		t.Fatal("expected config, got nil")
	}
	// Wildcard matches first, so it wins
	if config.Audience != "wildcard" {
		t.Errorf("expected 'wildcard' (first match), got %q", config.Audience)
	}
}

func TestStaticResolver_PortStripping(t *testing.T) {
	yaml := `
- host: "service.example.com"
  target_audience: "audience"
`
	r := resolverFromYAML(t, yaml)

	tests := []string{
		"service.example.com:8080",
		"service.example.com:443",
		"service.example.com:80",
	}

	for _, host := range tests {
		config, err := r.Resolve(context.Background(), host)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", host, err)
		}
		if config == nil {
			t.Errorf("expected %q to match after port stripping, got nil", host)
		}
	}
}

func TestStaticResolver_IPv6(t *testing.T) {
	yaml := `
- host: "::1"
  target_audience: "localhost-v6"
`
	r := resolverFromYAML(t, yaml)

	// IPv6 with port
	config, err := r.Resolve(context.Background(), "[::1]:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config == nil {
		t.Fatal("expected config for IPv6 with port, got nil")
	}
	if config.Audience != "localhost-v6" {
		t.Errorf("expected 'localhost-v6', got %q", config.Audience)
	}
}

func TestStaticResolver_Passthrough(t *testing.T) {
	yaml := `
- host: "internal.service.local"
  passthrough: true
`
	r := resolverFromYAML(t, yaml)

	config, err := r.Resolve(context.Background(), "internal.service.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config == nil {
		t.Fatal("expected config, got nil")
	}
	if !config.Passthrough {
		t.Error("expected Passthrough to be true")
	}
}

func TestStaticResolver_AllFields(t *testing.T) {
	yaml := `
- host: "full.example.com"
  target_audience: "aud"
  token_scopes: "openid profile"
  token_url: "https://custom.idp/token"
  passthrough: false
`
	r := resolverFromYAML(t, yaml)

	config, err := r.Resolve(context.Background(), "full.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config == nil {
		t.Fatal("expected config, got nil")
	}
	if config.Audience != "aud" {
		t.Errorf("Audience: expected 'aud', got %q", config.Audience)
	}
	if config.Scopes != "openid profile" {
		t.Errorf("Scopes: expected 'openid profile', got %q", config.Scopes)
	}
	if config.TokenEndpoint != "https://custom.idp/token" {
		t.Errorf("TokenEndpoint: expected 'https://custom.idp/token', got %q", config.TokenEndpoint)
	}
	if config.Passthrough != false {
		t.Errorf("Passthrough: expected false, got true")
	}
}

// resolverFromYAML creates a StaticResolver from inline YAML for testing
func resolverFromYAML(t *testing.T, yaml string) *StaticResolver {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "routes.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("failed to write test yaml: %v", err)
	}

	r, err := NewStaticResolver(path)
	if err != nil {
		t.Fatalf("failed to create resolver: %v", err)
	}
	return r
}
