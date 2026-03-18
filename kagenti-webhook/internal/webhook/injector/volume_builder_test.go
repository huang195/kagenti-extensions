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
)

func TestBuildResolvedVolumes_SpireDisabled(t *testing.T) {
	volumes := BuildResolvedVolumes(false, "")

	// Should have: shared-data, envoy-config, authproxy-routes
	if len(volumes) != 3 {
		t.Fatalf("expected 3 volumes, got %d", len(volumes))
	}

	names := map[string]bool{}
	for _, v := range volumes {
		names[v.Name] = true
	}

	for _, expected := range []string{"shared-data", "envoy-config", "authproxy-routes"} {
		if !names[expected] {
			t.Errorf("missing volume %q", expected)
		}
	}

	// Should NOT have SPIRE volumes
	for _, absent := range []string{"spire-agent-socket", "spiffe-helper-config", "svid-output"} {
		if names[absent] {
			t.Errorf("unexpected SPIRE volume %q when spireEnabled=false", absent)
		}
	}
}

func TestBuildResolvedVolumes_SpireEnabled(t *testing.T) {
	volumes := BuildResolvedVolumes(true, "")

	// Should have: shared-data, spire-agent-socket, spiffe-helper-config, svid-output, envoy-config, authproxy-routes
	if len(volumes) != 6 {
		t.Fatalf("expected 6 volumes, got %d", len(volumes))
	}

	names := map[string]bool{}
	for _, v := range volumes {
		names[v.Name] = true
	}

	for _, expected := range []string{"shared-data", "spire-agent-socket", "spiffe-helper-config", "svid-output", "envoy-config", "authproxy-routes"} {
		if !names[expected] {
			t.Errorf("missing volume %q", expected)
		}
	}
}

func TestBuildResolvedVolumes_CustomEnvoyConfigMapName(t *testing.T) {
	volumes := BuildResolvedVolumes(false, "my-custom-envoy")

	var envoyVolume *string
	for _, v := range volumes {
		if v.Name == "envoy-config" {
			name := v.VolumeSource.ConfigMap.LocalObjectReference.Name
			envoyVolume = &name
		}
	}

	if envoyVolume == nil {
		t.Fatal("envoy-config volume not found")
	}
	if *envoyVolume != "my-custom-envoy" {
		t.Errorf("envoy-config ConfigMap name = %q, want %q", *envoyVolume, "my-custom-envoy")
	}
}

func TestBuildResolvedVolumes_DefaultEnvoyConfigMapName(t *testing.T) {
	volumes := BuildResolvedVolumes(false, "")

	for _, v := range volumes {
		if v.Name == "envoy-config" {
			name := v.VolumeSource.ConfigMap.LocalObjectReference.Name
			if name != EnvoyConfigMapName {
				t.Errorf("envoy-config ConfigMap name = %q, want %q", name, EnvoyConfigMapName)
			}
			return
		}
	}
	t.Fatal("envoy-config volume not found")
}
