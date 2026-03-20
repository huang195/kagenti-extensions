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
	"fmt"
	"strconv"
	"strings"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var builderLog = logf.Log.WithName("container-builder")

const (
	// Container names for AuthBridge sidecars
	EnvoyProxyContainerName  = "envoy-proxy"
	ProxyInitContainerName   = "proxy-init"
	AuthBridgeContainerName  = "authbridge"

	// Client registration container configuration
	// Keep in sync with AuthBridge/client-registration/Dockerfile
	ClientRegistrationUID = 1000
	ClientRegistrationGID = 1000
)

// ContainerBuilder creates container specs from resolved config.
// It supports two modes:
//   - Legacy mode: constructed with NewContainerBuilder(platformConfig) — uses
//     ValueFrom refs for env vars (backward compatible)
//   - Resolved mode: constructed with NewResolvedContainerBuilder(resolvedConfig)
//     — uses literal env var values read at admission time
type ContainerBuilder struct {
	cfg      *config.PlatformConfig
	resolved *ResolvedConfig
}

// NewContainerBuilder creates a ContainerBuilder that uses ValueFrom refs
// for environment variables (legacy behavior).
func NewContainerBuilder(cfg *config.PlatformConfig) *ContainerBuilder {
	if cfg == nil {
		cfg = config.CompiledDefaults()
	}
	return &ContainerBuilder{cfg: cfg}
}

// NewResolvedContainerBuilder creates a ContainerBuilder that uses literal
// env var values from the resolved config (admission-time resolution).
func NewResolvedContainerBuilder(resolved *ResolvedConfig) *ContainerBuilder {
	if resolved == nil {
		resolved = ResolveConfig(nil, nil, nil)
	}
	return &ContainerBuilder{
		cfg:      resolved.Platform,
		resolved: resolved,
	}
}

func (b *ContainerBuilder) BuildSpiffeHelperContainer() corev1.Container {
	builderLog.Info("building SpiffeHelper Container")

	return corev1.Container{
		Name:            SpiffeHelperContainerName,
		Image:           b.cfg.Images.SpiffeHelper,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.SpiffeHelper,
		Command: []string{
			"/spiffe-helper",
			"-config=/etc/spiffe-helper/helper.conf",
			"run",
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "spiffe-helper-config",
				MountPath: "/etc/spiffe-helper",
			},
			{
				Name:      "spire-agent-socket",
				MountPath: "/spiffe-workload-api",
			},
			{
				Name:      "svid-output",
				MountPath: "/opt",
			},
			{
				Name:      "shared-data",
				MountPath: "/shared",
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    ptr.To(int64(ClientRegistrationUID)),
			RunAsGroup:   ptr.To(int64(ClientRegistrationGID)),
			RunAsNonRoot: ptr.To(true),
		},
	}
}

func (b *ContainerBuilder) BuildClientRegistrationContainer(name, namespace string) corev1.Container {
	// Default to SPIRE enabled for backward compatibility
	return b.BuildClientRegistrationContainerWithSpireOption(name, namespace, true)
}

// BuildClientRegistrationContainerWithSpireOption creates the client registration container
// with optional SPIRE support
func (b *ContainerBuilder) BuildClientRegistrationContainerWithSpireOption(name, namespace string, spireEnabled bool) corev1.Container {
	builderLog.Info("building ClientRegistration Container", "spireEnabled", spireEnabled)

	clientName := namespace + "/" + name

	var env []corev1.EnvVar
	if b.resolved != nil {
		// Resolved mode: literal values
		env = b.buildClientRegistrationEnvResolved(clientName, spireEnabled)
	} else {
		// Legacy mode: ValueFrom refs
		env = b.buildClientRegistrationEnvLegacy(clientName, spireEnabled)
	}

	// Volume mounts depend on SPIRE enablement
	var volumeMounts []corev1.VolumeMount
	if spireEnabled {
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "svid-output",
				MountPath: "/opt",
			},
			{
				Name:      "shared-data",
				MountPath: "/shared",
			},
		}
	} else {
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "shared-data",
				MountPath: "/shared",
			},
		}
	}

	// Build the command based on SPIRE enablement
	var command string
	if spireEnabled {
		command = `
echo "Waiting for SPIFFE credentials..."
while [ ! -f /opt/jwt_svid.token ]; do
  echo "waiting for SVID"
  sleep 1
done
echo "SPIFFE credentials ready!"

# Extract client ID (SPIFFE ID) from JWT and save to file
JWT_PAYLOAD=$(cat /opt/jwt_svid.token | cut -d'.' -f2)
if ! CLIENT_ID=$(echo "${JWT_PAYLOAD}==" | base64 -d | python -c "import sys,json; print(json.load(sys.stdin).get('sub',''))"); then
  echo "Error: Failed to decode JWT payload or extract client ID" >&2
  exit 1
fi
if [ -z "$CLIENT_ID" ]; then
  echo "Error: Extracted client ID is empty" >&2
  exit 1
fi
echo "$CLIENT_ID" > /shared/client-id.txt
echo "Client ID (SPIFFE ID): $CLIENT_ID"

echo "Starting client registration..."
python client_registration.py
echo "Client registration complete!"
tail -f /dev/null
`
	} else {
		command = `
echo "SPIRE disabled - using static client ID"

# Use CLIENT_NAME as the client ID
echo "$CLIENT_NAME" > /shared/client-id.txt
echo "Client ID: $CLIENT_NAME"

echo "Starting client registration..."
python client_registration.py
echo "Client registration complete!"
tail -f /dev/null
`
	}

	return corev1.Container{
		Name:            ClientRegistrationContainerName,
		Image:           b.cfg.Images.ClientRegistration,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.ClientRegistration,
		Command: []string{
			"/bin/sh",
			"-c",
			command,
		},
		Env:          env,
		VolumeMounts: volumeMounts,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    ptr.To(int64(ClientRegistrationUID)),
			RunAsGroup:   ptr.To(int64(ClientRegistrationGID)),
			RunAsNonRoot: ptr.To(true),
		},
	}
}

// buildClientRegistrationEnvResolved returns env vars from resolved config.
// Non-sensitive values (URLs, realm, client name) are injected as literals.
// Sensitive values (KEYCLOAK_ADMIN_USERNAME/PASSWORD) use SecretKeyRef to keep
// credentials out of the Pod spec — only a reference to the Secret is stored.
func (b *ContainerBuilder) buildClientRegistrationEnvResolved(clientName string, spireEnabled bool) []corev1.EnvVar {
	secretName := b.resolved.AdminCredentialsSecretName
	if secretName == "" {
		secretName = KeycloakAdminSecretName
	}
	return []corev1.EnvVar{
		{Name: "SPIRE_ENABLED", Value: fmt.Sprintf("%t", spireEnabled)},
		{Name: "KEYCLOAK_URL", Value: b.resolved.KeycloakURL},
		{Name: "KEYCLOAK_REALM", Value: b.resolved.KeycloakRealm},
		{
			Name: "KEYCLOAK_ADMIN_USERNAME",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "KEYCLOAK_ADMIN_USERNAME",
				},
			},
		},
		{
			Name: "KEYCLOAK_ADMIN_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "KEYCLOAK_ADMIN_PASSWORD",
				},
			},
		},
		{Name: "CLIENT_NAME", Value: clientName},
		{Name: "SECRET_FILE_PATH", Value: "/shared/client-secret.txt"},
		{Name: "PLATFORM_CLIENT_IDS", Value: b.resolved.PlatformClientIDs},
	}
}

// buildClientRegistrationEnvLegacy returns ValueFrom-based env vars (backward compat).
func (b *ContainerBuilder) buildClientRegistrationEnvLegacy(clientName string, spireEnabled bool) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name:  "SPIRE_ENABLED",
			Value: fmt.Sprintf("%t", spireEnabled),
		},
		{
			Name: "KEYCLOAK_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "KEYCLOAK_URL",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "KEYCLOAK_REALM",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "KEYCLOAK_REALM",
				},
			},
		},
		{
			Name: "KEYCLOAK_ADMIN_USERNAME",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "keycloak-admin-secret"},
					Key:                  "KEYCLOAK_ADMIN_USERNAME",
				},
			},
		},
		{
			Name: "KEYCLOAK_ADMIN_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "keycloak-admin-secret"},
					Key:                  "KEYCLOAK_ADMIN_PASSWORD",
				},
			},
		},
		{
			Name:  "CLIENT_NAME",
			Value: clientName,
		},
		{
			Name:  "SECRET_FILE_PATH",
			Value: "/shared/client-secret.txt",
		},
		{
			Name: "PLATFORM_CLIENT_IDS",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "PLATFORM_CLIENT_IDS",
					Optional:             ptr.To(true),
				},
			},
		},
	}
}

// BuildEnvoyProxyContainer creates the envoy-proxy sidecar container with SPIRE enabled (default).
func (b *ContainerBuilder) BuildEnvoyProxyContainer() corev1.Container {
	return b.BuildEnvoyProxyContainerWithSpireOption(true)
}

// BuildEnvoyProxyContainerWithSpireOption creates the envoy-proxy sidecar container.
// When spireEnabled is true, the svid-output volume is mounted (read-only) so the
// go-processor can read the SPIFFE JWT SVID for use as a subject token in RFC 8693
// token exchange on outbound requests.
func (b *ContainerBuilder) BuildEnvoyProxyContainerWithSpireOption(spireEnabled bool) corev1.Container {
	builderLog.Info("building EnvoyProxy Container", "spireEnabled", spireEnabled)

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "envoy-config",
			MountPath: "/etc/envoy",
			ReadOnly:  true,
		},
		{
			Name:      "shared-data",
			MountPath: "/shared",
			ReadOnly:  true,
		},
		{
			Name:      "authproxy-routes",
			MountPath: "/etc/authproxy",
			ReadOnly:  true,
		},
	}
	if spireEnabled {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "svid-output",
			MountPath: "/opt",
			ReadOnly:  true,
		})
	}

	var env []corev1.EnvVar
	if b.resolved != nil {
		env = b.buildEnvoyProxyEnvResolved()
	} else {
		env = b.buildEnvoyProxyEnvLegacy()
	}


	return corev1.Container{
		Name:            EnvoyProxyContainerName,
		Image:           b.cfg.Images.EnvoyProxy,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.EnvoyProxy,
		Ports: []corev1.ContainerPort{
			{
				Name:          "envoy-outbound",
				ContainerPort: b.cfg.Proxy.Port,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "envoy-inbound",
				ContainerPort: b.cfg.Proxy.InboundProxyPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "envoy-admin",
				ContainerPort: b.cfg.Proxy.AdminPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "ext-proc",
				ContainerPort: 9090,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: env,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  ptr.To(b.cfg.Proxy.UID),
			RunAsGroup: ptr.To(b.cfg.Proxy.UID),
		},
		VolumeMounts: volumeMounts,
	}
}

// buildEnvoyProxyEnvResolved returns literal env vars from resolved config.
func (b *ContainerBuilder) buildEnvoyProxyEnvResolved() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "KEYCLOAK_URL", Value: b.resolved.KeycloakURL},
		{Name: "KEYCLOAK_REALM", Value: b.resolved.KeycloakRealm},
		{Name: "TOKEN_URL", Value: b.resolved.TokenURL},
		{Name: "ISSUER", Value: b.resolved.Issuer},
		{Name: "EXPECTED_AUDIENCE", Value: b.resolved.ExpectedAudience},
		{Name: "TARGET_AUDIENCE", Value: b.resolved.TargetAudience},
		{Name: "TARGET_SCOPES", Value: b.resolved.TargetScopes},
		{Name: "CLIENT_ID_FILE", Value: "/shared/client-id.txt"},
		{Name: "CLIENT_SECRET_FILE", Value: "/shared/client-secret.txt"},
		{Name: "ROUTES_CONFIG_PATH", Value: "/etc/authproxy/routes.yaml"},
		{Name: "DEFAULT_OUTBOUND_POLICY", Value: b.resolved.DefaultOutboundPolicy},
	}
}

// buildEnvoyProxyEnvLegacy returns ValueFrom-based env vars (backward compat).
func (b *ContainerBuilder) buildEnvoyProxyEnvLegacy() []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name: "KEYCLOAK_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "KEYCLOAK_URL",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "KEYCLOAK_REALM",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "KEYCLOAK_REALM",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TOKEN_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TOKEN_URL",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "ISSUER",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "ISSUER",
					Optional:             ptr.To(false),
				},
			},
		},
		{
			Name: "EXPECTED_AUDIENCE",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "EXPECTED_AUDIENCE",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TARGET_AUDIENCE",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TARGET_AUDIENCE",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TARGET_SCOPES",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TARGET_SCOPES",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name:  "CLIENT_ID_FILE",
			Value: "/shared/client-id.txt",
		},
		{
			Name:  "CLIENT_SECRET_FILE",
			Value: "/shared/client-secret.txt",
		},
		{
			Name:  "ROUTES_CONFIG_PATH",
			Value: "/etc/authproxy/routes.yaml",
		},
		{
			Name: "DEFAULT_OUTBOUND_POLICY",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "DEFAULT_OUTBOUND_POLICY",
					Optional:             ptr.To(true),
				},
			},
		},
	}
}

// BuildAuthBridgeContainer creates the combined authbridge sidecar container
// that includes envoy-proxy, go-processor, spiffe-helper, and client-registration
// in a single container. This is used when the CombinedSidecar feature gate is enabled.
func (b *ContainerBuilder) BuildAuthBridgeContainer(name, namespace string, spireEnabled, clientRegistrationEnabled bool) corev1.Container {
	builderLog.Info("building AuthBridge combined Container",
		"spireEnabled", spireEnabled,
		"clientRegistrationEnabled", clientRegistrationEnabled)

	clientName := namespace + "/" + name

	var env []corev1.EnvVar
	if b.resolved != nil {
		env = b.buildAuthBridgeEnvResolved(clientName, spireEnabled, clientRegistrationEnabled)
	} else {
		env = b.buildAuthBridgeEnvLegacy(clientName, spireEnabled, clientRegistrationEnabled)
	}

	// Volume mounts: union of envoy-proxy + spiffe-helper + client-registration mounts.
	// shared-data and svid-output are read-write (same container reads and writes).
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "envoy-config",
			MountPath: "/etc/envoy",
			ReadOnly:  true,
		},
		{
			Name:      "authproxy-routes",
			MountPath: "/etc/authproxy",
			ReadOnly:  true,
		},
		{
			Name:      "shared-data",
			MountPath: "/shared",
		},
	}
	if spireEnabled {
		volumeMounts = append(volumeMounts,
			corev1.VolumeMount{
				Name:      "svid-output",
				MountPath: "/opt",
			},
			corev1.VolumeMount{
				Name:      "spiffe-helper-config",
				MountPath: "/etc/spiffe-helper",
				ReadOnly:  true,
			},
			corev1.VolumeMount{
				Name:      "spire-agent-socket",
				MountPath: "/spiffe-workload-api",
				ReadOnly:  true,
			},
		)
	}

	return corev1.Container{
		Name:            AuthBridgeContainerName,
		Image:           b.cfg.Images.AuthBridge,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.AuthBridge,
		Ports: []corev1.ContainerPort{
			{
				Name:          "envoy-outbound",
				ContainerPort: b.cfg.Proxy.Port,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "envoy-inbound",
				ContainerPort: b.cfg.Proxy.InboundProxyPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "envoy-admin",
				ContainerPort: b.cfg.Proxy.AdminPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "ext-proc",
				ContainerPort: 9090,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: env,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  ptr.To(b.cfg.Proxy.UID),
			RunAsGroup: ptr.To(b.cfg.Proxy.UID),
		},
		VolumeMounts: volumeMounts,
	}
}

// buildAuthBridgeEnvResolved returns env vars for the combined container from resolved config.
func (b *ContainerBuilder) buildAuthBridgeEnvResolved(clientName string, spireEnabled, clientRegistrationEnabled bool) []corev1.EnvVar {
	secretName := b.resolved.AdminCredentialsSecretName
	if secretName == "" {
		secretName = KeycloakAdminSecretName
	}

	env := []corev1.EnvVar{
		// Control flags for the entrypoint
		{Name: "SPIRE_ENABLED", Value: fmt.Sprintf("%t", spireEnabled)},
		{Name: "CLIENT_REGISTRATION_ENABLED", Value: fmt.Sprintf("%t", clientRegistrationEnabled)},
		// Envoy/go-processor env vars
		{Name: "KEYCLOAK_URL", Value: b.resolved.KeycloakURL},
		{Name: "KEYCLOAK_REALM", Value: b.resolved.KeycloakRealm},
		{Name: "TOKEN_URL", Value: b.resolved.TokenURL},
		{Name: "ISSUER", Value: b.resolved.Issuer},
		{Name: "EXPECTED_AUDIENCE", Value: b.resolved.ExpectedAudience},
		{Name: "TARGET_AUDIENCE", Value: b.resolved.TargetAudience},
		{Name: "TARGET_SCOPES", Value: b.resolved.TargetScopes},
		{Name: "CLIENT_ID_FILE", Value: "/shared/client-id.txt"},
		{Name: "CLIENT_SECRET_FILE", Value: "/shared/client-secret.txt"},
		{Name: "ROUTES_CONFIG_PATH", Value: "/etc/authproxy/routes.yaml"},
		{Name: "DEFAULT_OUTBOUND_POLICY", Value: b.resolved.DefaultOutboundPolicy},
		// Client-registration env vars (sensitive values stay as SecretKeyRef)
		{
			Name: "KEYCLOAK_ADMIN_USERNAME",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "KEYCLOAK_ADMIN_USERNAME",
				},
			},
		},
		{
			Name: "KEYCLOAK_ADMIN_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "KEYCLOAK_ADMIN_PASSWORD",
				},
			},
		},
		{Name: "CLIENT_NAME", Value: clientName},
		{Name: "SECRET_FILE_PATH", Value: "/shared/client-secret.txt"},
		{Name: "PLATFORM_CLIENT_IDS", Value: b.resolved.PlatformClientIDs},
	}

	return env
}

// buildAuthBridgeEnvLegacy returns ValueFrom-based env vars for the combined container.
func (b *ContainerBuilder) buildAuthBridgeEnvLegacy(clientName string, spireEnabled, clientRegistrationEnabled bool) []corev1.EnvVar {
	return []corev1.EnvVar{
		// Control flags for the entrypoint
		{Name: "SPIRE_ENABLED", Value: fmt.Sprintf("%t", spireEnabled)},
		{Name: "CLIENT_REGISTRATION_ENABLED", Value: fmt.Sprintf("%t", clientRegistrationEnabled)},
		// Envoy/go-processor env vars (from ConfigMap)
		{
			Name: "KEYCLOAK_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "KEYCLOAK_URL",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "KEYCLOAK_REALM",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "KEYCLOAK_REALM",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TOKEN_URL",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TOKEN_URL",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "ISSUER",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "ISSUER",
					Optional:             ptr.To(false),
				},
			},
		},
		{
			Name: "EXPECTED_AUDIENCE",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "EXPECTED_AUDIENCE",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TARGET_AUDIENCE",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TARGET_AUDIENCE",
					Optional:             ptr.To(true),
				},
			},
		},
		{
			Name: "TARGET_SCOPES",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "TARGET_SCOPES",
					Optional:             ptr.To(true),
				},
			},
		},
		{Name: "CLIENT_ID_FILE", Value: "/shared/client-id.txt"},
		{Name: "CLIENT_SECRET_FILE", Value: "/shared/client-secret.txt"},
		{Name: "ROUTES_CONFIG_PATH", Value: "/etc/authproxy/routes.yaml"},
		{
			Name: "DEFAULT_OUTBOUND_POLICY",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
					Key:                  "DEFAULT_OUTBOUND_POLICY",
					Optional:             ptr.To(true),
				},
			},
		},
		// Client-registration env vars
		{
			Name: "KEYCLOAK_ADMIN_USERNAME",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "keycloak-admin-secret"},
					Key:                  "KEYCLOAK_ADMIN_USERNAME",
				},
			},
		},
		{
			Name: "KEYCLOAK_ADMIN_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "keycloak-admin-secret"},
					Key:                  "KEYCLOAK_ADMIN_PASSWORD",
				},
			},
		},
		{Name: "CLIENT_NAME", Value: clientName},
		{Name: "SECRET_FILE_PATH", Value: "/shared/client-secret.txt"},
		{
			Name: "PLATFORM_CLIENT_IDS",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: AuthBridgeConfigMapName},
					Key:                  "PLATFORM_CLIENT_IDS",
					Optional:             ptr.To(true),
				},
			},
		},
	}
}

// BuildProxyInitContainer creates the init container that sets up iptables
// to redirect outbound traffic to the Envoy proxy.
//
// SECURITY NOTE: This init container requires elevated capabilities:
//   - RunAsUser: 0 (root) - Required to modify network namespace iptables rules
//   - RunAsNonRoot: false - Explicitly allows root execution
//   - NET_ADMIN capability - Required for iptables manipulation
//   - NET_RAW capability - Required for raw socket operations used by iptables
//
// The init container does NOT require privileged mode. It uses DNAT to the pod's
// own IP instead of REDIRECT for the ztunnel inbound interception rule, which
// avoids the need for sysctl route_localnet=1 (which would require privileged
// mode to write to read-only /proc/sys). All other capabilities are dropped.
//
// Risk mitigations:
//   - This runs as an init container (not a long-running sidecar), limiting exposure window
//   - The container exits immediately after configuring iptables rules
//   - Minimal resource limits are applied (10m CPU, 10Mi memory)
//   - Only NET_ADMIN and NET_RAW capabilities are granted (all others dropped)
//   - The container image should be regularly updated and scanned for vulnerabilities
//
// mandatoryOutboundExclude is always prepended so that Keycloak traffic
// (port 8080) is never intercepted by Envoy.
const mandatoryOutboundExclude = "8080"

// BuildProxyInitContainer creates the proxy-init container.
// outboundPortsExclude is a comma-separated list of additional ports to
// exclude from outbound interception (mandatory 8080 is always included).
// inboundPortsExclude is a comma-separated list of ports to exclude from
// inbound interception (only set when non-empty). Both come from the
// kagenti.io/outbound-ports-exclude and kagenti.io/inbound-ports-exclude
// pod annotations.
func (b *ContainerBuilder) BuildProxyInitContainer(outboundPortsExclude, inboundPortsExclude string) corev1.Container {
	outboundValue := buildOutboundExcludeValue(outboundPortsExclude)
	inboundValue := buildPortExcludeValue(inboundPortsExclude, "inbound-ports-exclude")

	builderLog.Info("building ProxyInit Container",
		"resolvedOutboundPortsExclude", outboundValue,
		"resolvedInboundPortsExclude", inboundValue)

	env := []corev1.EnvVar{
		{
			Name:  "PROXY_PORT",
			Value: fmt.Sprintf("%d", b.cfg.Proxy.Port),
		},
		{
			Name:  "INBOUND_PROXY_PORT",
			Value: fmt.Sprintf("%d", b.cfg.Proxy.InboundProxyPort),
		},
		{
			Name:  "PROXY_UID",
			Value: fmt.Sprintf("%d", b.cfg.Proxy.UID),
		},
		{
			Name:  "OUTBOUND_PORTS_EXCLUDE",
			Value: outboundValue,
		},
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
	}
	if inboundValue != "" {
		env = append(env, corev1.EnvVar{
			Name:  "INBOUND_PORTS_EXCLUDE",
			Value: inboundValue,
		})
	}

	return corev1.Container{
		Name:            ProxyInitContainerName,
		Image:           b.cfg.Images.ProxyInit,
		ImagePullPolicy: b.cfg.Images.PullPolicy,
		Resources:       b.cfg.Resources.ProxyInit,
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    ptr.To(int64(0)),
			RunAsNonRoot: ptr.To(false),
			Privileged:   ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW"},
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}
}

// validateAndDeduplicatePorts parses a comma-separated port string, validates
// each token (numeric, 1-65535), deduplicates, and returns the clean list.
// initialPorts are prepended and excluded from duplicates.
func validateAndDeduplicatePorts(raw, annotationName string, initialPorts []string) []string {
	seen := map[string]bool{}
	ports := make([]string, 0, len(initialPorts)+4)
	for _, p := range initialPorts {
		seen[p] = true
		ports = append(ports, p)
	}

	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		p, err := strconv.Atoi(tok)
		if err != nil || p < 1 || p > 65535 {
			builderLog.V(0).Info("WARNING: ignoring invalid port in "+annotationName+" annotation", "value", tok)
			continue
		}
		normalized := strconv.Itoa(p)
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		ports = append(ports, normalized)
	}
	return ports
}

// buildOutboundExcludeValue merges the mandatory 8080 with validated
// user-supplied ports. Invalid tokens (non-numeric, out of range) are
// silently dropped and logged. Duplicates of 8080 are removed.
func buildOutboundExcludeValue(extra string) string {
	if extra == "" {
		return mandatoryOutboundExclude
	}
	return strings.Join(validateAndDeduplicatePorts(extra, "outbound-ports-exclude", []string{mandatoryOutboundExclude}), ",")
}

// buildPortExcludeValue validates and deduplicates a comma-separated port
// list. Returns "" when the input is empty. Used for inbound port exclusion
// where there is no mandatory port.
func buildPortExcludeValue(raw, annotationName string) string {
	if raw == "" {
		return ""
	}
	return strings.Join(validateAndDeduplicatePorts(raw, annotationName, nil), ",")
}
