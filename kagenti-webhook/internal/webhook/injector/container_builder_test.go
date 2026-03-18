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
	"testing"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/config"
)

func TestBuildEnvoyProxyContainer_SpireEnabled_HasSvidOutputMount(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			found = true
			if vm.MountPath != "/opt" {
				t.Errorf("svid-output mount path = %q, want /opt", vm.MountPath)
			}
			if !vm.ReadOnly {
				t.Error("svid-output mount should be read-only")
			}
			break
		}
	}
	if !found {
		t.Error("envoy-proxy container missing svid-output volume mount when SPIRE is enabled")
	}
}

func TestBuildEnvoyProxyContainer_SpireDisabled_NoSvidOutputMount(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(false)

	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			t.Error("envoy-proxy container should NOT have svid-output mount when SPIRE is disabled")
		}
	}
}

func TestBuildEnvoyProxyContainer_DefaultIncludesSvidOutput(t *testing.T) {
	// The no-arg BuildEnvoyProxyContainer defaults to SPIRE enabled
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainer()

	found := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "svid-output" {
			found = true
			break
		}
	}
	if !found {
		t.Error("default BuildEnvoyProxyContainer should include svid-output mount")
	}
}

func TestBuildEnvoyProxyContainer_HasAllRequiredMounts(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	requiredMounts := map[string]string{
		"envoy-config": "/etc/envoy",
		"shared-data":  "/shared",
		"svid-output":  "/opt",
	}

	mountsByName := make(map[string]string)
	for _, vm := range container.VolumeMounts {
		mountsByName[vm.Name] = vm.MountPath
	}

	for name, expectedPath := range requiredMounts {
		path, ok := mountsByName[name]
		if !ok {
			t.Errorf("missing volume mount %q", name)
			continue
		}
		if path != expectedPath {
			t.Errorf("volume mount %q path = %q, want %q", name, path, expectedPath)
		}
	}
}

func TestBuildEnvoyProxyContainer_Name(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainer()

	if container.Name != EnvoyProxyContainerName {
		t.Errorf("container name = %q, want %q", container.Name, EnvoyProxyContainerName)
	}
}

func TestBuildClientRegistrationContainer_HasPlatformClientIDsEnv(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildClientRegistrationContainerWithSpireOption("test-agent", "team1", true)

	found := false
	for _, env := range container.Env {
		if env.Name == "PLATFORM_CLIENT_IDS" {
			found = true
			if env.ValueFrom == nil || env.ValueFrom.ConfigMapKeyRef == nil {
				t.Error("PLATFORM_CLIENT_IDS should reference a ConfigMap key")
				break
			}
			if env.ValueFrom.ConfigMapKeyRef.Name != "authbridge-config" {
				t.Errorf("PLATFORM_CLIENT_IDS ConfigMapKeyRef.Name = %q, want %q",
					env.ValueFrom.ConfigMapKeyRef.Name, "authbridge-config")
			}
			if env.ValueFrom.ConfigMapKeyRef.Key != "PLATFORM_CLIENT_IDS" {
				t.Errorf("PLATFORM_CLIENT_IDS key = %q, want PLATFORM_CLIENT_IDS",
					env.ValueFrom.ConfigMapKeyRef.Key)
			}
			if env.ValueFrom.ConfigMapKeyRef.Optional == nil || !*env.ValueFrom.ConfigMapKeyRef.Optional {
				t.Error("PLATFORM_CLIENT_IDS should be optional")
			}
			break
		}
	}
	if !found {
		t.Error("client-registration container missing PLATFORM_CLIENT_IDS env var")
	}
}

func TestBuildClientRegistrationContainer_AdminCredentialsFromSecret(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildClientRegistrationContainerWithSpireOption("my-app", "my-ns", true)

	sensitiveKeys := []string{"KEYCLOAK_ADMIN_USERNAME", "KEYCLOAK_ADMIN_PASSWORD"}
	for _, key := range sensitiveKeys {
		found := false
		for _, env := range container.Env {
			if env.Name != key {
				continue
			}
			found = true
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Errorf("env %q must use SecretKeyRef, got ConfigMapKeyRef or literal", key)
				continue
			}
			if env.ValueFrom.SecretKeyRef.Name != "keycloak-admin-secret" {
				t.Errorf("env %q SecretKeyRef.Name = %q, want %q", key, env.ValueFrom.SecretKeyRef.Name, "keycloak-admin-secret")
			}
		}
		if !found {
			t.Errorf("client-registration container missing env var %q", key)
		}
	}
}

func TestBuildClientRegistrationContainer_ResolvedPath_AdminCredentialsFromSecret(t *testing.T) {
	resolved := &ResolvedConfig{
		Platform:    config.CompiledDefaults(),
		KeycloakURL: "https://keycloak.example.com",
	}
	builder := NewResolvedContainerBuilder(resolved)
	container := builder.BuildClientRegistrationContainerWithSpireOption("my-app", "my-ns", true)

	sensitiveKeys := []string{"KEYCLOAK_ADMIN_USERNAME", "KEYCLOAK_ADMIN_PASSWORD"}
	for _, key := range sensitiveKeys {
		found := false
		for _, env := range container.Env {
			if env.Name != key {
				continue
			}
			found = true
			if env.Value != "" {
				t.Errorf("env %q must NOT have a literal Value in resolved path (security: keeps credentials out of Pod spec)", key)
			}
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Errorf("env %q must use SecretKeyRef, got literal or ConfigMapKeyRef", key)
				continue
			}
			if env.ValueFrom.SecretKeyRef.Name != "keycloak-admin-secret" {
				t.Errorf("env %q SecretKeyRef.Name = %q, want %q", key, env.ValueFrom.SecretKeyRef.Name, "keycloak-admin-secret")
			}
		}
		if !found {
			t.Errorf("client-registration container missing env var %q", key)
		}
	}
}

func TestBuildClientRegistrationContainer_NonSensitiveKeysFromConfigMap(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildClientRegistrationContainerWithSpireOption("my-app", "my-ns", true)

	nonSensitiveKeys := []string{"KEYCLOAK_URL", "KEYCLOAK_REALM"}
	for _, key := range nonSensitiveKeys {
		found := false
		for _, env := range container.Env {
			if env.Name != key {
				continue
			}
			found = true
			if env.ValueFrom == nil || env.ValueFrom.ConfigMapKeyRef == nil {
				t.Errorf("env %q must use ConfigMapKeyRef", key)
				continue
			}
			if env.ValueFrom.ConfigMapKeyRef.Name != "authbridge-config" {
				t.Errorf("env %q ConfigMapKeyRef.Name = %q, want %q", key, env.ValueFrom.ConfigMapKeyRef.Name, "authbridge-config")
			}
		}
		if !found {
			t.Errorf("client-registration container missing env var %q", key)
		}
	}
}

func TestBuildClientRegistrationContainer_HasSecretFilePath(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildClientRegistrationContainerWithSpireOption("my-app", "my-ns", true)

	found := false
	for _, env := range container.Env {
		if env.Name == "SECRET_FILE_PATH" {
			found = true
			if env.Value != "/shared/client-secret.txt" {
				t.Errorf("SECRET_FILE_PATH should be /shared/client-secret.txt, got %s", env.Value)
			}
		}
	}
	if !found {
		t.Error("client-registration container should have SECRET_FILE_PATH env var for backwards compatibility")
	}
}

func TestBuildEnvoyProxyContainer_HasKeycloakURLAndRealm(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	for _, key := range []string{"KEYCLOAK_URL", "KEYCLOAK_REALM"} {
		found := false
		for _, env := range container.Env {
			if env.Name == key {
				found = true
				if env.ValueFrom == nil || env.ValueFrom.ConfigMapKeyRef == nil {
					t.Errorf("env %q must use ConfigMapKeyRef", key)
					break
				}
				if env.ValueFrom.ConfigMapKeyRef.Name != "authbridge-config" {
					t.Errorf("env %q ConfigMapKeyRef.Name = %q, want %q", key, env.ValueFrom.ConfigMapKeyRef.Name, "authbridge-config")
				}
				if env.ValueFrom.ConfigMapKeyRef.Optional == nil || !*env.ValueFrom.ConfigMapKeyRef.Optional {
					t.Errorf("env %q should be optional", key)
				}
				break
			}
		}
		if !found {
			t.Errorf("envoy-proxy container missing env var %q", key)
		}
	}
}

func TestBuildOutboundExcludeValue_Empty(t *testing.T) {
	got := buildOutboundExcludeValue("")
	if got != "8080" {
		t.Errorf("buildOutboundExcludeValue(\"\") = %q, want %q", got, "8080")
	}
}

func TestBuildOutboundExcludeValue_SinglePort(t *testing.T) {
	got := buildOutboundExcludeValue("11434")
	if got != "8080,11434" {
		t.Errorf("buildOutboundExcludeValue(\"11434\") = %q, want %q", got, "8080,11434")
	}
}

func TestBuildOutboundExcludeValue_MultiplePorts(t *testing.T) {
	got := buildOutboundExcludeValue("11434,4317")
	if got != "8080,11434,4317" {
		t.Errorf("buildOutboundExcludeValue(\"11434,4317\") = %q, want %q", got, "8080,11434,4317")
	}
}

func TestBuildOutboundExcludeValue_Deduplicates8080(t *testing.T) {
	got := buildOutboundExcludeValue("8080,11434")
	if got != "8080,11434" {
		t.Errorf("buildOutboundExcludeValue(\"8080,11434\") = %q, want %q", got, "8080,11434")
	}
}

func TestBuildOutboundExcludeValue_TrimsWhitespace(t *testing.T) {
	got := buildOutboundExcludeValue(" 11434 , 4317 ")
	if got != "8080,11434,4317" {
		t.Errorf("buildOutboundExcludeValue(\" 11434 , 4317 \") = %q, want %q", got, "8080,11434,4317")
	}
}

func TestBuildOutboundExcludeValue_DropsInvalidTokens(t *testing.T) {
	got := buildOutboundExcludeValue("11434,abc,0,65536,-1,,99999")
	if got != "8080,11434" {
		t.Errorf("buildOutboundExcludeValue with invalid tokens = %q, want %q", got, "8080,11434")
	}
}

func TestBuildOutboundExcludeValue_BoundaryPorts(t *testing.T) {
	got := buildOutboundExcludeValue("1,65535")
	if got != "8080,1,65535" {
		t.Errorf("buildOutboundExcludeValue(\"1,65535\") = %q, want %q", got, "8080,1,65535")
	}
}

func TestBuildProxyInitContainer_DefaultExclude(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("", "")

	var foundOutbound bool
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
			foundOutbound = true
			if env.Value != "8080" {
				t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080")
			}
		}
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			t.Error("INBOUND_PORTS_EXCLUDE should not be set when inbound exclude is empty")
		}
	}
	if !foundOutbound {
		t.Error("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
	}
}

func TestBuildProxyInitContainer_WithAnnotationPorts(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("11434,4317", "")

	var foundOutbound bool
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
			foundOutbound = true
			if env.Value != "8080,11434,4317" {
				t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080,11434,4317")
			}
		}
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			t.Error("INBOUND_PORTS_EXCLUDE should not be set when inbound exclude is empty")
		}
	}
	if !foundOutbound {
		t.Error("proxy-init container missing OUTBOUND_PORTS_EXCLUDE env var")
	}
}

func TestBuildProxyInitContainer_WithInboundExclude(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("", "8443,18789")

	var foundInbound bool
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" && env.Value != "8080" {
			t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080")
		}
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			foundInbound = true
			if env.Value != "8443,18789" {
				t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8443,18789")
			}
		}
	}
	if !foundInbound {
		t.Error("proxy-init container missing INBOUND_PORTS_EXCLUDE env var")
	}
}

func TestBuildProxyInitContainer_WithBothExcludes(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildProxyInitContainer("11434", "8443")

	var foundOutbound, foundInbound bool
	for _, env := range container.Env {
		if env.Name == "OUTBOUND_PORTS_EXCLUDE" {
			foundOutbound = true
			if env.Value != "8080,11434" {
				t.Errorf("OUTBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8080,11434")
			}
		}
		if env.Name == "INBOUND_PORTS_EXCLUDE" {
			foundInbound = true
			if env.Value != "8443" {
				t.Errorf("INBOUND_PORTS_EXCLUDE = %q, want %q", env.Value, "8443")
			}
		}
	}
	if !foundOutbound {
		t.Error("missing OUTBOUND_PORTS_EXCLUDE")
	}
	if !foundInbound {
		t.Error("missing INBOUND_PORTS_EXCLUDE")
	}
}

func TestBuildPortExcludeValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"single port", "8443", "8443"},
		{"multiple ports", "8443,18789", "8443,18789"},
		{"whitespace", " 8443 , 18789 ", "8443,18789"},
		{"duplicates", "8443,8443,18789", "8443,18789"},
		{"invalid tokens", "8443,abc,18789", "8443,18789"},
		{"out of range", "0,8443,99999", "8443"},
		{"all invalid", "abc,0,99999", ""},
		{"empty segments", "8443,,18789", "8443,18789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPortExcludeValue(tt.input, "test-annotation")
			if got != tt.want {
				t.Errorf("buildPortExcludeValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildEnvoyProxyContainer_HasExpectedAudienceFromConfigMap(t *testing.T) {
	builder := NewContainerBuilder(config.CompiledDefaults())
	container := builder.BuildEnvoyProxyContainerWithSpireOption(true)

	found := false
	for _, env := range container.Env {
		if env.Name == "EXPECTED_AUDIENCE" {
			found = true
			if env.ValueFrom == nil || env.ValueFrom.ConfigMapKeyRef == nil {
				t.Error("EXPECTED_AUDIENCE must use ConfigMapKeyRef")
				break
			}
			if env.ValueFrom.ConfigMapKeyRef.Name != "authbridge-config" {
				t.Errorf("EXPECTED_AUDIENCE ConfigMapKeyRef.Name = %q, want %q",
					env.ValueFrom.ConfigMapKeyRef.Name, "authbridge-config")
			}
			if env.ValueFrom.ConfigMapKeyRef.Optional == nil || !*env.ValueFrom.ConfigMapKeyRef.Optional {
				t.Error("EXPECTED_AUDIENCE should be optional")
			}
			break
		}
	}
	if !found {
		t.Error("envoy-proxy container missing EXPECTED_AUDIENCE env var from ConfigMap")
	}
}
