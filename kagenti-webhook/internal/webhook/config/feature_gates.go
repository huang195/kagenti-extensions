package config

// FeatureGates controls which sidecars are globally enabled/disabled.
// This is the highest-priority layer in the injection precedence chain.
type FeatureGates struct {
	GlobalEnabled      bool `json:"globalEnabled" yaml:"globalEnabled"`
	EnvoyProxy         bool `json:"envoyProxy" yaml:"envoyProxy"`
	SpiffeHelper       bool `json:"spiffeHelper" yaml:"spiffeHelper"`
	ClientRegistration bool `json:"clientRegistration" yaml:"clientRegistration"`
	// InjectTools controls whether tool workloads (kagenti.io/type=tool) receive
	// sidecar injection. Defaults to false — tools are not injected by default.
	InjectTools bool `json:"injectTools" yaml:"injectTools"`
	// PerWorkloadConfigResolution controls the env-var injection mode:
	//   false (default) → legacy path: env vars use ValueFrom ConfigMapKeyRef/
	//                     SecretKeyRef references; kubelet resolves at container start.
	//   true            → resolved path: webhook reads namespace ConfigMaps at
	//                     admission time and injects literal env var values.
	PerWorkloadConfigResolution bool `json:"perWorkloadConfigResolution" yaml:"perWorkloadConfigResolution"`
	// CombinedSidecar controls whether sidecars are injected as a single combined
	// "authbridge" container (true) or as separate containers (false, default).
	// When true, envoy-proxy + spiffe-helper + client-registration are combined
	// into one container. proxy-init is still injected as a separate init container.
	CombinedSidecar bool `json:"combinedSidecar" yaml:"combinedSidecar"`
}

// DefaultFeatureGates returns feature gates with sidecar injection enabled for
// agents and disabled for tools.
func DefaultFeatureGates() *FeatureGates {
	return &FeatureGates{
		GlobalEnabled:               true,
		EnvoyProxy:                  true,
		SpiffeHelper:                true,
		ClientRegistration:          true,
		InjectTools:                 false,
		PerWorkloadConfigResolution: false,
	}
}

// DeepCopy creates a copy of the feature gates.
func (fg *FeatureGates) DeepCopy() *FeatureGates {
	if fg == nil {
		return nil
	}
	result := *fg
	return &result
}
