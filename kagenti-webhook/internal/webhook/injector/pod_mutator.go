/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package injector

import (
	"context"
	"fmt"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var mutatorLog = logf.Log.WithName("pod-mutator")

const (
	// Container names
	SpiffeHelperContainerName       = "spiffe-helper"
	ClientRegistrationContainerName = "kagenti-client-registration"

	// Label selector for authbridge injection opt-out.
	// Injection uses opt-out semantics for agents: sidecars are injected by
	// default. Setting AuthBridgeInjectLabel=AuthBridgeDisabledValue on a
	// workload explicitly opts it out. Any other value (including absent) does
	// not block injection.
	AuthBridgeInjectLabel   = "kagenti.io/inject"
	AuthBridgeInjectValue   = "enabled" // retained for backwards compat / tests
	AuthBridgeDisabledValue = "disabled"

	// SPIRE opt-out label. Setting kagenti.io/spire=disabled on a workload blocks
	// spiffe-helper injection (layer 7 of the precedence chain). Any other value
	// (including absence of the label) leaves spiffe-helper injection to the
	// upstream precedence layers.
	SpireEnableLabel   = "kagenti.io/spire"
	SpireDisabledValue = "disabled"
	// SpireEnabledValue is a non-operative label value under the opt-out model.
	// Retained as a named constant so tests can assert that a non-disabled value
	// does not block injection.
	SpireEnabledValue = "enabled"
	// Istio exclusion annotations
	IstioSidecarInjectAnnotation = "sidecar.istio.io/inject"
	AmbientRedirectionAnnotation = "ambient.istio.io/redirection"

	// Port exclusion annotations — per-workload iptables overrides.
	// Values are comma-separated port numbers. Outbound values are appended
	// to the mandatory exclusion (8080). Example: "11434,4317"
	OutboundPortsExcludeAnnotation = "kagenti.io/outbound-ports-exclude"
	InboundPortsExcludeAnnotation  = "kagenti.io/inbound-ports-exclude"

	// KagentiTypeLabel is the label key that identifies the workload type
	KagentiTypeLabel = "kagenti.io/type"
	// KagentiTypeAgent is the label value that identifies agent workloads
	KagentiTypeAgent = "agent"
	// KagentiTypeTool is the label value that identifies tool workloads
	KagentiTypeTool = "tool"
)

type PodMutator struct {
	Client                   client.Client
	EnableClientRegistration bool
	// Getter functions for hot-reloadable config (used by precedence evaluator)
	GetPlatformConfig func() *config.PlatformConfig
	GetFeatureGates   func() *config.FeatureGates
}

func NewPodMutator(
	client client.Client,
	enableClientRegistration bool,
	getPlatformConfig func() *config.PlatformConfig,
	getFeatureGates func() *config.FeatureGates,
) *PodMutator {
	return &PodMutator{
		Client:                   client,
		EnableClientRegistration: enableClientRegistration,
		GetPlatformConfig:        getPlatformConfig,
		GetFeatureGates:          getFeatureGates,
	}
}

// InjectAuthBridge evaluates the multi-layer precedence chain and conditionally injects sidecars.
func (m *PodMutator) InjectAuthBridge(ctx context.Context, podSpec *corev1.PodSpec, namespace, crName string, labels, annotations map[string]string) (bool, error) {
	mutatorLog.Info("InjectAuthBridge called", "namespace", namespace, "crName", crName, "labels", labels)

	// Pre-filter: kagenti.io/type must be agent or tool.
	kagentiType, hasKagentiLabel := labels[KagentiTypeLabel]
	if !hasKagentiLabel || (kagentiType != KagentiTypeAgent && kagentiType != KagentiTypeTool) {
		mutatorLog.Info("Skipping mutation: workload is not an agent or a tool",
			"hasLabel", hasKagentiLabel,
			"labelValue", kagentiType)
		return false, nil
	}

	// Get fresh config snapshots for this request (hot-reloadable)
	currentConfig := m.GetPlatformConfig()
	currentGates := m.GetFeatureGates()

	// Global kill switch — disables all injection cluster-wide.
	if !currentGates.GlobalEnabled {
		mutatorLog.Info("Skipping mutation: global feature gate disabled",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Tool workloads are only injected when the injectTools feature gate is on.
	if kagentiType == KagentiTypeTool && !currentGates.InjectTools {
		mutatorLog.Info("Skipping mutation: tool injection disabled via injectTools feature gate",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Opt-out: skip injection when kagenti.io/inject=disabled is explicitly set.
	if labels[AuthBridgeInjectLabel] == AuthBridgeDisabledValue {
		mutatorLog.Info("Skipping mutation: workload opted out via kagenti.io/inject=disabled",
			"namespace", namespace, "crName", crName)
		return false, nil
	}

	// Evaluate the per-sidecar precedence chain
	evaluator := NewPrecedenceEvaluator(currentGates, currentConfig)
	decision := evaluator.Evaluate(labels)

	// Log each sidecar decision
	for _, d := range []struct {
		name string
		sd   SidecarDecision
	}{
		{"envoy-proxy", decision.EnvoyProxy},
		{"proxy-init", decision.ProxyInit},
		{"spiffe-helper", decision.SpiffeHelper},
		{"client-registration", decision.ClientRegistration},
	} {
		mutatorLog.Info("injection decision",
			"sidecar", d.name,
			"inject", d.sd.Inject,
			"reason", d.sd.Reason,
			"layer", d.sd.Layer,
		)
	}

	if !decision.AnyInjected() {
		mutatorLog.Info("Skipping mutation (no sidecars to inject)", "namespace", namespace, "crName", crName)
		return false, nil
	}

	// Derive SPIRE mode from the injection decision: if spiffe-helper is being
	// injected then SPIRE volumes and a dedicated ServiceAccount are needed.
	spireEnabled := decision.SpiffeHelper.Inject

	// When SPIRE is enabled, ensure a dedicated ServiceAccount exists so
	// the SPIFFE ID reflects the workload name instead of "default".
	if spireEnabled && (podSpec.ServiceAccountName == "" || podSpec.ServiceAccountName == "default") {
		if err := m.ensureServiceAccount(ctx, namespace, crName); err != nil {
			mutatorLog.Error(err, "Failed to ensure ServiceAccount", "namespace", namespace, "name", crName)
			return false, fmt.Errorf("failed to ensure service account: %w", err)
		}
		podSpec.ServiceAccountName = crName
		mutatorLog.Info("Set ServiceAccountName for SPIRE identity", "namespace", namespace, "serviceAccount", crName)
	}

	// Initialize slices
	if podSpec.Containers == nil {
		podSpec.Containers = []corev1.Container{}
	}
	if podSpec.InitContainers == nil {
		podSpec.InitContainers = []corev1.Container{}
	}
	if podSpec.Volumes == nil {
		podSpec.Volumes = []corev1.Volume{}
	}

	// ========================================
	// Build containers + volumes
	// ========================================
	//
	// Two modes controlled by the perWorkloadConfigResolution feature gate:
	//   false (default) → legacy path: ValueFrom refs for env vars, kubelet
	//                     resolves ConfigMap/Secret values at container start.
	//   true            → resolved path: webhook reads namespace ConfigMaps/
	//                     Secrets at admission time and injects literal values.

	var builder *ContainerBuilder
	var requiredVolumes []corev1.Volume

	if currentGates.PerWorkloadConfigResolution {
		// Resolved path: read namespace config and build literal env vars
		var nsConfig *NamespaceConfig
		var err error
		mutatorLog.V(1).Info("reading namespace config per-workload", "namespace", namespace)
		nsConfig, err = ReadNamespaceConfig(ctx, m.Client, namespace)
		if err != nil {
			mutatorLog.Error(err, "failed to read namespace config, using empty defaults",
				"namespace", namespace)
			nsConfig = &NamespaceConfig{}
		}

		// Read AgentRuntime overrides (nil if CR not found or CRD not installed)
		var arOverrides *AgentRuntimeOverrides
		arOverrides, err = ReadAgentRuntimeOverrides(ctx, m.Client, namespace, crName)
		if err != nil {
			mutatorLog.Error(err, "failed to read AgentRuntime overrides, continuing without",
				"namespace", namespace, "crName", crName)
		}

		resolved := ResolveConfig(currentConfig, nsConfig, arOverrides)
		builder = NewResolvedContainerBuilder(resolved)
		requiredVolumes = BuildResolvedVolumes(spireEnabled, "")

		mutatorLog.Info("Using resolved config path",
			"namespace", namespace, "crName", crName,
			"hasAgentRuntimeOverrides", arOverrides != nil)
	} else {
		// Legacy path: ValueFrom refs, kubelet resolves at runtime
		builder = NewContainerBuilder(currentConfig)
		if spireEnabled {
			requiredVolumes = BuildRequiredVolumes()
		} else {
			requiredVolumes = BuildRequiredVolumesNoSpire()
		}
		mutatorLog.Info("Using legacy ValueFrom config path",
			"namespace", namespace, "crName", crName)
	}

	// Conditionally inject sidecars based on precedence decisions
	if decision.EnvoyProxy.Inject && !containerExists(podSpec.Containers, EnvoyProxyContainerName) {
		podSpec.Containers = append(podSpec.Containers, builder.BuildEnvoyProxyContainerWithSpireOption(spireEnabled))
	}

	if decision.ProxyInit.Inject && !containerExists(podSpec.InitContainers, ProxyInitContainerName) {
		outboundExclude := annotations[OutboundPortsExcludeAnnotation]
		inboundExclude := annotations[InboundPortsExcludeAnnotation]
		podSpec.InitContainers = append(podSpec.InitContainers, builder.BuildProxyInitContainer(outboundExclude, inboundExclude))
	}

	if decision.SpiffeHelper.Inject && !containerExists(podSpec.Containers, SpiffeHelperContainerName) {
		podSpec.Containers = append(podSpec.Containers, builder.BuildSpiffeHelperContainer())
	}

	if decision.ClientRegistration.Inject && !containerExists(podSpec.Containers, ClientRegistrationContainerName) {
		podSpec.Containers = append(podSpec.Containers, builder.BuildClientRegistrationContainerWithSpireOption(crName, namespace, spireEnabled))
	}

	// Inject volumes
	for i := range requiredVolumes {
		if !volumeExists(podSpec.Volumes, requiredVolumes[i].Name) {
			podSpec.Volumes = append(podSpec.Volumes, requiredVolumes[i])
		}
	}

	mutatorLog.Info("Successfully mutated pod spec", "namespace", namespace, "crName", crName,
		"containers", len(podSpec.Containers),
		"initContainers", len(podSpec.InitContainers),
		"volumes", len(podSpec.Volumes),
		"spireEnabled", spireEnabled)
	return true, nil
}

const managedByLabel = "kagenti.io/managed-by"
const managedByValue = "webhook"

// ensureServiceAccount creates a ServiceAccount in the given namespace if it
// does not already exist. This gives SPIRE-enabled workloads a dedicated
// identity so the SPIFFE ID is spiffe://<trust-domain>/ns/<ns>/sa/<name>
// rather than .../sa/default.
func (m *PodMutator) ensureServiceAccount(ctx context.Context, namespace, name string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
	}
	if err := m.Client.Create(ctx, sa); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := &corev1.ServiceAccount{}
			if getErr := m.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing); getErr != nil {
				return fmt.Errorf("failed to fetch existing ServiceAccount %s/%s: %w", namespace, name, getErr)
			}
			if existing.Labels[managedByLabel] != managedByValue {
				mutatorLog.Info("WARNING: ServiceAccount exists but is not managed by this webhook",
					"namespace", namespace, "name", name,
					"existingLabels", existing.Labels)
			} else {
				mutatorLog.Info("ServiceAccount already exists", "namespace", namespace, "name", name)
			}
			return nil
		}
		return fmt.Errorf("failed to create ServiceAccount %s/%s: %w", namespace, name, err)
	}
	mutatorLog.Info("Created ServiceAccount", "namespace", namespace, "name", name)
	return nil
}

func containerExists(containers []corev1.Container, name string) bool {
	for i := range containers {
		if containers[i].Name == name {
			return true
		}
	}
	return false
}

func volumeExists(volumes []corev1.Volume, name string) bool {
	for i := range volumes {
		if volumes[i].Name == name {
			return true
		}
	}
	return false
}
