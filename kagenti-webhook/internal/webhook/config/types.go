package config

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// PlatformConfig represents the complete platform configuration
type PlatformConfig struct {
	Images        ImageConfig           `json:"images" yaml:"images"`
	Proxy         ProxyConfig           `json:"proxy" yaml:"proxy"`
	Resources     ResourcesConfig       `json:"resources" yaml:"resources"`
	TokenExchange TokenExchangeDefaults `json:"tokenExchange" yaml:"tokenExchange"`
	Spiffe        SpiffeConfig          `json:"spiffe" yaml:"spiffe"`
	Observability ObservabilityConfig   `json:"observability" yaml:"observability"`
	Sidecars      SidecarDefaults       `json:"sidecars" yaml:"sidecars"`
}

type ImageConfig struct {
	EnvoyProxy         string            `json:"envoyProxy" yaml:"envoyProxy"`
	ProxyInit          string            `json:"proxyInit" yaml:"proxyInit"`
	SpiffeHelper       string            `json:"spiffeHelper" yaml:"spiffeHelper"`
	ClientRegistration string            `json:"clientRegistration" yaml:"clientRegistration"`
	AuthBridge         string            `json:"authbridge" yaml:"authbridge"`
	PullPolicy         corev1.PullPolicy `json:"pullPolicy" yaml:"pullPolicy"`
}

type ProxyConfig struct {
	Port             int32 `json:"port" yaml:"port"`
	UID              int64 `json:"uid" yaml:"uid"`
	InboundProxyPort int32 `json:"inboundProxyPort" yaml:"inboundProxyPort"`
	AdminPort        int32 `json:"adminPort" yaml:"adminPort"`
}

type ResourcesConfig struct {
	EnvoyProxy         corev1.ResourceRequirements `json:"envoyProxy" yaml:"envoyProxy"`
	ProxyInit          corev1.ResourceRequirements `json:"proxyInit" yaml:"proxyInit"`
	SpiffeHelper       corev1.ResourceRequirements `json:"spiffeHelper" yaml:"spiffeHelper"`
	ClientRegistration corev1.ResourceRequirements `json:"clientRegistration" yaml:"clientRegistration"`
	AuthBridge         corev1.ResourceRequirements `json:"authbridge" yaml:"authbridge"`
}

type TokenExchangeDefaults struct {
	TokenURL        string   `json:"tokenUrl" yaml:"tokenUrl"`
	DefaultAudience string   `json:"defaultAudience" yaml:"defaultAudience"`
	DefaultScopes   []string `json:"defaultScopes" yaml:"defaultScopes"`
}

type SpiffeConfig struct {
	TrustDomain string `json:"trustDomain" yaml:"trustDomain"`
	SocketPath  string `json:"socketPath" yaml:"socketPath"`
}

type ObservabilityConfig struct {
	LogLevel       string `json:"logLevel" yaml:"logLevel"`
	EnableMetrics  bool   `json:"enableMetrics" yaml:"enableMetrics"`
	EnableTracing  bool   `json:"enableTracing" yaml:"enableTracing"`
	TracingBackend string `json:"tracingBackend" yaml:"tracingBackend"`
}

// SidecarDefaults controls per-sidecar enable/disable at the platform level.
// This is the lowest-priority layer in the injection precedence chain.
type SidecarDefaults struct {
	EnvoyProxy         SidecarDefault `json:"envoyProxy" yaml:"envoyProxy"`
	SpiffeHelper       SidecarDefault `json:"spiffeHelper" yaml:"spiffeHelper"`
	ClientRegistration SidecarDefault `json:"clientRegistration" yaml:"clientRegistration"`
}

type SidecarDefault struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// DeepCopy creates a copy of the config
func (c *PlatformConfig) DeepCopy() *PlatformConfig {
	if c == nil {
		return nil
	}
	result := *c

	if c.TokenExchange.DefaultScopes != nil {
		result.TokenExchange.DefaultScopes = make([]string, len(c.TokenExchange.DefaultScopes))
		copy(result.TokenExchange.DefaultScopes, c.TokenExchange.DefaultScopes)
	}

	// Deep copy ResourceRequirements — ResourceList is a map that would be shared
	result.Resources.EnvoyProxy = deepCopyResourceRequirements(c.Resources.EnvoyProxy)
	result.Resources.ProxyInit = deepCopyResourceRequirements(c.Resources.ProxyInit)
	result.Resources.SpiffeHelper = deepCopyResourceRequirements(c.Resources.SpiffeHelper)
	result.Resources.ClientRegistration = deepCopyResourceRequirements(c.Resources.ClientRegistration)
	result.Resources.AuthBridge = deepCopyResourceRequirements(c.Resources.AuthBridge)

	return &result
}

func deepCopyResourceRequirements(rr corev1.ResourceRequirements) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	if rr.Requests != nil {
		out.Requests = make(corev1.ResourceList, len(rr.Requests))
		for k, v := range rr.Requests {
			out.Requests[k] = v.DeepCopy()
		}
	}
	if rr.Limits != nil {
		out.Limits = make(corev1.ResourceList, len(rr.Limits))
		for k, v := range rr.Limits {
			out.Limits[k] = v.DeepCopy()
		}
	}
	return out
}

// Validate checks if the config is valid
func (c *PlatformConfig) Validate() error {
	if c.Proxy.Port < 1024 || c.Proxy.Port > 65535 {
		return fmt.Errorf("proxy.port must be between 1024 and 65535")
	}
	if c.Proxy.InboundProxyPort < 1024 || c.Proxy.InboundProxyPort > 65535 {
		return fmt.Errorf("proxy.inboundProxyPort must be between 1024 and 65535")
	}
	if c.Proxy.AdminPort < 1024 || c.Proxy.AdminPort > 65535 {
		return fmt.Errorf("proxy.adminPort must be between 1024 and 65535")
	}
	if c.Images.EnvoyProxy == "" {
		return fmt.Errorf("images.envoyProxy is required")
	}
	if c.Images.ProxyInit == "" {
		return fmt.Errorf("images.proxyInit is required")
	}
	if c.Images.SpiffeHelper == "" {
		return fmt.Errorf("images.spiffeHelper is required")
	}
	if c.Images.ClientRegistration == "" {
		return fmt.Errorf("images.clientRegistration is required")
	}
	return nil
}
