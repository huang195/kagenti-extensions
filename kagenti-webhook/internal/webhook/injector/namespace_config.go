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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var nsConfigLog = logf.Log.WithName("namespace-config")

// Well-known ConfigMap/Secret names in the target namespace.
const (
	AuthBridgeConfigMapName      = "authbridge-config"
	KeycloakAdminSecretName      = "keycloak-admin-secret"
	SpiffeHelperConfigMapName    = "spiffe-helper-config"
	EnvoyConfigMapName           = "envoy-config"
	AuthproxyRoutesConfigMapName = "authproxy-routes"
)

// NamespaceConfig holds resolved values from namespace ConfigMaps/Secrets.
type NamespaceConfig struct {
	// From "authbridge-config" ConfigMap
	KeycloakURL           string
	KeycloakRealm         string
	SpireEnabled          string
	PlatformClientIDs     string
	TokenURL              string
	Issuer                string
	ExpectedAudience      string
	TargetAudience        string
	TargetScopes          string
	DefaultOutboundPolicy string

	// From "spiffe-helper-config" ConfigMap
	SpiffeHelperConf string // raw helper.conf content

	// From "envoy-config" ConfigMap
	EnvoyYAML string // raw envoy.yaml content

	// From "authproxy-routes" ConfigMap
	AuthproxyRoutesYAML string // raw routes.yaml content
}

// ReadNamespaceConfig reads the well-known ConfigMaps/Secrets from the target
// namespace at admission time. Missing resources result in empty strings for
// those fields; each read is independent.
func ReadNamespaceConfig(ctx context.Context, c client.Reader, namespace string) (*NamespaceConfig, error) {
	cfg := &NamespaceConfig{}

	// Read "authbridge-config" ConfigMap (all identity + token exchange settings)
	if cm, err := getConfigMap(ctx, c, namespace, AuthBridgeConfigMapName); err != nil {
		nsConfigLog.V(1).Info("ConfigMap not found", "name", AuthBridgeConfigMapName, "namespace", namespace, "error", err)
	} else {
		cfg.KeycloakURL = cm.Data["KEYCLOAK_URL"]
		cfg.KeycloakRealm = cm.Data["KEYCLOAK_REALM"]
		cfg.SpireEnabled = cm.Data["SPIRE_ENABLED"]
		cfg.PlatformClientIDs = cm.Data["PLATFORM_CLIENT_IDS"]
		cfg.TokenURL = cm.Data["TOKEN_URL"]
		cfg.Issuer = cm.Data["ISSUER"]
		cfg.ExpectedAudience = cm.Data["EXPECTED_AUDIENCE"]
		cfg.TargetAudience = cm.Data["TARGET_AUDIENCE"]
		cfg.TargetScopes = cm.Data["TARGET_SCOPES"]
		cfg.DefaultOutboundPolicy = cm.Data["DEFAULT_OUTBOUND_POLICY"]
	}

	// Note: keycloak-admin-secret is not read here. The resolved container builder
	// uses SecretKeyRef to reference the secret by name, keeping credentials out of
	// the NamespaceConfig struct and the webhook's memory.

	// Read "spiffe-helper-config" ConfigMap
	if cm, err := getConfigMap(ctx, c, namespace, SpiffeHelperConfigMapName); err != nil {
		nsConfigLog.V(1).Info("ConfigMap not found", "name", SpiffeHelperConfigMapName, "namespace", namespace, "error", err)
	} else {
		cfg.SpiffeHelperConf = cm.Data["helper.conf"]
	}

	// Read "envoy-config" ConfigMap
	if cm, err := getConfigMap(ctx, c, namespace, EnvoyConfigMapName); err != nil {
		nsConfigLog.V(1).Info("ConfigMap not found", "name", EnvoyConfigMapName, "namespace", namespace, "error", err)
	} else {
		cfg.EnvoyYAML = cm.Data["envoy.yaml"]
	}

	// Read "authproxy-routes" ConfigMap
	if cm, err := getConfigMap(ctx, c, namespace, AuthproxyRoutesConfigMapName); err != nil {
		nsConfigLog.V(1).Info("ConfigMap not found", "name", AuthproxyRoutesConfigMapName, "namespace", namespace, "error", err)
	} else {
		cfg.AuthproxyRoutesYAML = cm.Data["routes.yaml"]
	}

	return cfg, nil
}

func getConfigMap(ctx context.Context, c client.Reader, namespace, name string) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return nil, err
	}
	return cm, nil
}
