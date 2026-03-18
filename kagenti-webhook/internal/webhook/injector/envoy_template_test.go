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
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/config"
)

func TestRenderEnvoyConfig_UsesExistingYAML(t *testing.T) {
	cfg := &ResolvedConfig{
		Platform:  config.CompiledDefaults(),
		EnvoyYAML: "existing-envoy-config",
	}

	result, err := RenderEnvoyConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "existing-envoy-config" {
		t.Errorf("expected existing envoy config to be returned as-is, got %q", result)
	}
}

func TestRenderEnvoyConfig_TemplateRendering(t *testing.T) {
	cfg := &ResolvedConfig{
		Platform: config.CompiledDefaults(),
	}

	result, err := RenderEnvoyConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the template was rendered with correct ports
	if !strings.Contains(result, "port_value: 9901") {
		t.Error("expected admin port 9901 in rendered config")
	}
	if !strings.Contains(result, "port_value: 15123") {
		t.Error("expected outbound port 15123 in rendered config")
	}
	if !strings.Contains(result, "port_value: 15124") {
		t.Error("expected inbound port 15124 in rendered config")
	}
	if !strings.Contains(result, "ext_proc_cluster") {
		t.Error("expected ext_proc_cluster in rendered config")
	}
	if !strings.Contains(result, "port_value: 9090") {
		t.Error("expected ext_proc port 9090 in rendered config")
	}
	if !strings.Contains(result, "original_destination") {
		t.Error("expected original_destination cluster in rendered config")
	}
}

func TestRenderEnvoyConfig_CustomPorts(t *testing.T) {
	platform := config.CompiledDefaults()
	platform.Proxy.Port = 20000
	platform.Proxy.InboundProxyPort = 20001
	platform.Proxy.AdminPort = 20002

	cfg := &ResolvedConfig{
		Platform: platform,
	}

	result, err := RenderEnvoyConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "port_value: 20000") {
		t.Error("expected custom outbound port 20000")
	}
	if !strings.Contains(result, "port_value: 20001") {
		t.Error("expected custom inbound port 20001")
	}
	if !strings.Contains(result, "port_value: 20002") {
		t.Error("expected custom admin port 20002")
	}
}
