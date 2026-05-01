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
	Mode     string         `yaml:"mode" json:"mode"` // "envoy-sidecar", "waypoint", "proxy-sidecar"
	Inbound  InboundConfig  `yaml:"inbound" json:"inbound"`
	Outbound OutboundConfig `yaml:"outbound" json:"outbound"`
	Identity IdentityConfig `yaml:"identity" json:"identity"`
	Listener ListenerConfig `yaml:"listener" json:"listener"`
	Bypass   BypassConfig   `yaml:"bypass" json:"bypass"`
	Routes   RoutesConfig   `yaml:"routes" json:"routes"`
	Pipeline PipelineConfig `yaml:"pipeline" json:"pipeline"`
	Stats    StatsConfig    `yaml:"stats" json:"stats"`
}

// PipelineConfig holds the plugin pipeline composition.
// If omitted (empty), default pipelines are used:
// inbound=[jwt-validation], outbound=[token-exchange].
type PipelineConfig struct {
	Inbound  PipelineStageConfig `yaml:"inbound" json:"inbound"`
	Outbound PipelineStageConfig `yaml:"outbound" json:"outbound"`
}

// PipelineStageConfig lists the plugins for a pipeline stage in execution order.
type PipelineStageConfig struct {
	Plugins []string `yaml:"plugins" json:"plugins"`
}

// InboundConfig holds JWT validation settings.
type InboundConfig struct {
	JWKSURL string `yaml:"jwks_url" json:"jwks_url"`
	Issuer  string `yaml:"issuer" json:"issuer"`
}

// OutboundConfig holds token exchange settings.
type OutboundConfig struct {
	TokenURL      string `yaml:"token_url" json:"token_url"`
	KeycloakURL   string `yaml:"keycloak_url" json:"keycloak_url"`     // alternative: derives token_url and issuer
	KeycloakRealm string `yaml:"keycloak_realm" json:"keycloak_realm"` // used with keycloak_url
	DefaultPolicy string `yaml:"default_policy" json:"default_policy"` // "exchange" or "passthrough"
}

// IdentityConfig holds agent identity and credentials.
type IdentityConfig struct {
	Type             string   `yaml:"type" json:"type"` // "spiffe", "client-secret", "k8s-sa"
	ClientID         string   `yaml:"client_id" json:"client_id"`
	ClientSecret     string   `yaml:"client_secret" json:"client_secret"`
	ClientIDFile     string   `yaml:"client_id_file" json:"client_id_file"`         // alternative: read client_id from file
	ClientSecretFile string   `yaml:"client_secret_file" json:"client_secret_file"` // alternative: read client_secret from file
	SocketPath       string   `yaml:"socket_path" json:"socket_path"`               // SPIFFE Workload API
	JWTSVIDPath      string   `yaml:"jwt_svid_path" json:"jwt_svid_path"`           // file-based SPIFFE
	JWTAudience      []string `yaml:"jwt_audience" json:"jwt_audience"`             // SPIFFE JWT audience
}

// ListenerConfig holds per-mode listener addresses.
type ListenerConfig struct {
	ExtProcAddr         string `yaml:"ext_proc_addr" json:"ext_proc_addr"`
	ExtAuthzAddr        string `yaml:"ext_authz_addr" json:"ext_authz_addr"`
	ForwardProxyAddr    string `yaml:"forward_proxy_addr" json:"forward_proxy_addr"`
	ReverseProxyAddr    string `yaml:"reverse_proxy_addr" json:"reverse_proxy_addr"`
	ReverseProxyBackend string `yaml:"reverse_proxy_backend" json:"reverse_proxy_backend"`
}

// BypassConfig holds path patterns that skip inbound JWT validation.
type BypassConfig struct {
	InboundPaths []string `yaml:"inbound_paths" json:"inbound_paths"`
}

// RoutesConfig holds outbound routing rules.
type RoutesConfig struct {
	File  string        `yaml:"file" json:"file"`   // path to routes YAML file
	Rules []RouteConfig `yaml:"rules" json:"rules"` // inline rules (alternative to file)
}

// RouteConfig is the YAML representation of an outbound route.
// Supports both legacy `passthrough: true` and new `action: passthrough` formats.
// Note that the JSON doesn't omitempty because we want to make it obvious
// to human readers which fields are empty.
type RouteConfig struct {
	Host           string `yaml:"host" json:"host"`
	TargetAudience string `yaml:"target_audience,omitempty" json:"target_audience"`
	TokenScopes    string `yaml:"token_scopes,omitempty" json:"token_scopes"`
	TokenURL       string `yaml:"token_url,omitempty" json:"token_url"`
	Passthrough    bool   `yaml:"passthrough,omitempty" json:"passthrough"` // legacy format
	Action         string `yaml:"action,omitempty" json:"action"`           // "exchange" or "passthrough"
}

// StatsConfig represents the configuration for reporting config and statistics
type StatsConfig struct {
	StatsAddress string `yaml:"address" json:"address"` // for example, ":9093"
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

	// Default stats server address
	if cfg.Stats.StatsAddress == "" {
		// Note that we default to an open port, not localhost 127.0.0.1:9093,
		// because the Kagenti UI needs to see this.  (If there are concerns
		// about the data exposed, use TLS or redact fields.)
		cfg.Stats.StatsAddress = ":9093"
	}

	return &cfg, nil
}
