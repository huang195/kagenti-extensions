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
	"bytes"
	_ "embed"
	"fmt"
	"text/template" // text/template (not html/template) — YAML output, no HTML escaping needed
)

//go:embed envoy.yaml.tmpl
var envoyTemplateSrc string

var envoyTemplate = template.Must(template.New("envoy.yaml").Parse(envoyTemplateSrc))

// envoyTemplateData holds the values substituted into the envoy.yaml template.
type envoyTemplateData struct {
	AdminPort    int32
	OutboundPort int32
	InboundPort  int32
	ExtProcPort  int32
}

// Default ext-proc gRPC port (go-processor).
const defaultExtProcPort int32 = 9090

// RenderEnvoyConfig generates an envoy.yaml from the resolved config.
// If the resolved config already contains an EnvoyYAML string (from the
// namespace ConfigMap), it is returned as-is for backward compatibility.
//
// TODO: wire RenderEnvoyConfig into the admission pipeline when per-workload
// ConfigMap creation is implemented. Currently this function is only used in
// unit tests.
func RenderEnvoyConfig(cfg *ResolvedConfig) (string, error) {
	if cfg == nil || cfg.Platform == nil {
		return "", fmt.Errorf("resolved config or platform config is nil")
	}
	if cfg.EnvoyYAML != "" {
		return cfg.EnvoyYAML, nil
	}

	data := envoyTemplateData{
		AdminPort:    cfg.Platform.Proxy.AdminPort,
		OutboundPort: cfg.Platform.Proxy.Port,
		InboundPort:  cfg.Platform.Proxy.InboundProxyPort,
		ExtProcPort:  defaultExtProcPort,
	}

	var buf bytes.Buffer
	if err := envoyTemplate.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing envoy template: %w", err)
	}
	return buf.String(), nil
}
