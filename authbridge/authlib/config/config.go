// Package config provides YAML-based configuration with mode presets
// and startup validation for the AuthBridge auth layer.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level AuthBridge configuration.
type Config struct {
	Mode     string         `yaml:"mode"` // "envoy-sidecar", "waypoint", "proxy-sidecar"
	Inbound  InboundConfig  `yaml:"inbound"`
	Outbound OutboundConfig `yaml:"outbound"`
	Identity IdentityConfig `yaml:"identity"`
	Listener ListenerConfig `yaml:"listener"`
	Bypass   BypassConfig   `yaml:"bypass"`
	Routes   RoutesConfig   `yaml:"routes"`
	Stats    StatsConfig    `yaml:"stats"`
}

// InboundConfig holds JWT validation settings.
type InboundConfig struct {
	JWKSURL string `yaml:"jwks_url"`
	Issuer  string `yaml:"issuer"`
}

// OutboundConfig holds token exchange settings.
type OutboundConfig struct {
	TokenURL      string `yaml:"token_url"`
	KeycloakURL   string `yaml:"keycloak_url"`   // alternative: derives token_url and issuer
	KeycloakRealm string `yaml:"keycloak_realm"` // used with keycloak_url
	DefaultPolicy string `yaml:"default_policy"` // "exchange" or "passthrough"
}

// IdentityConfig holds agent identity and credentials.
type IdentityConfig struct {
	Type             string   `yaml:"type"` // "spiffe", "client-secret", "k8s-sa"
	ClientID         string   `yaml:"client_id"`
	ClientSecret     string   `yaml:"client_secret"`
	ClientIDFile     string   `yaml:"client_id_file"`     // alternative: read client_id from file
	ClientSecretFile string   `yaml:"client_secret_file"` // alternative: read client_secret from file
	SocketPath       string   `yaml:"socket_path"`        // SPIFFE Workload API
	JWTSVIDPath      string   `yaml:"jwt_svid_path"`      // file-based SPIFFE
	JWTAudience      []string `yaml:"jwt_audience"`       // SPIFFE JWT audience
}

// ListenerConfig holds per-mode listener addresses.
type ListenerConfig struct {
	ExtProcAddr         string `yaml:"ext_proc_addr"`
	ExtAuthzAddr        string `yaml:"ext_authz_addr"`
	ForwardProxyAddr    string `yaml:"forward_proxy_addr"`
	ReverseProxyAddr    string `yaml:"reverse_proxy_addr"`
	ReverseProxyBackend string `yaml:"reverse_proxy_backend"`
}

// BypassConfig holds path patterns that skip inbound JWT validation.
type BypassConfig struct {
	InboundPaths []string `yaml:"inbound_paths"`
}

// RoutesConfig holds outbound routing rules.
type RoutesConfig struct {
	File  string        `yaml:"file"`  // path to routes YAML file
	Rules []RouteConfig `yaml:"rules"` // inline rules (alternative to file)
}

// RouteConfig is the YAML representation of an outbound route.
// Supports both legacy `passthrough: true` and new `action: passthrough` formats.
type RouteConfig struct {
	Host           string `yaml:"host"`
	TargetAudience string `yaml:"target_audience,omitempty"`
	TokenScopes    string `yaml:"token_scopes,omitempty"`
	TokenURL       string `yaml:"token_url,omitempty"`
	Passthrough    bool   `yaml:"passthrough,omitempty"` // legacy format
	Action         string `yaml:"action,omitempty"`      // "exchange" or "passthrough"
}

// StatsConfig represents the configuration for reporting config and statistics
type StatsConfig struct {
	StatsAddress string `yaml:"address"` // for example, ":9093"
}

// Valid mode strings.
const (
	ModeEnvoySidecar = "envoy-sidecar"
	ModeWaypoint     = "waypoint"
	ModeProxySidecar = "proxy-sidecar"
)

// Load reads and parses a YAML config file with environment variable expansion.
// Defined env vars are expanded; undefined references like ${UNDEFINED} are left as-is
// to avoid silent empty-string substitution.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	expanded := os.Expand(string(data), func(key string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return "${" + key + "}" // preserve undefined references
	})
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}
