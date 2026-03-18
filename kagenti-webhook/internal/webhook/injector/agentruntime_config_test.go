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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// registerAgentRuntimeScheme adds the unstructured AgentRuntime types to a scheme
// so the fake client can handle them.
func registerAgentRuntimeScheme(scheme *runtime.Scheme) {
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: AgentRuntimeGroup, Version: AgentRuntimeVersion, Kind: AgentRuntimeKind},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: AgentRuntimeGroup, Version: AgentRuntimeVersion, Kind: AgentRuntimeKind + "List"},
		&unstructured.UnstructuredList{},
	)
}

func TestReadAgentRuntimeOverrides_NotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	registerAgentRuntimeScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	overrides, err := ReadAgentRuntimeOverrides(context.Background(), fakeClient, "ns1", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides != nil {
		t.Fatalf("expected nil overrides, got %+v", overrides)
	}
}

func TestReadAgentRuntimeOverrides_MatchesByTargetRef(t *testing.T) {
	scheme := runtime.NewScheme()
	registerAgentRuntimeScheme(scheme)

	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": AgentRuntimeGroup + "/" + AgentRuntimeVersion,
			"kind":       AgentRuntimeKind,
			"metadata": map[string]interface{}{
				"name":      "my-agent-runtime", // CR name differs from workload name
				"namespace": "ns1",
			},
			"spec": map[string]interface{}{
				"type": "agent",
				"targetRef": map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"name":       "my-agent", // binds to workload
				},
				"identity": map[string]interface{}{
					"spiffe": map[string]interface{}{
						"trustDomain": "override.local",
					},
					"clientRegistration": map[string]interface{}{
						"provider": "keycloak",
						"realm":    "override-realm",
						"adminCredentialsSecret": map[string]interface{}{
							"name":      "my-secret",
							"namespace": "ns1",
						},
					},
				},
				"trace": map[string]interface{}{
					"endpoint": "http://otel-collector:4317",
					"protocol": "grpc",
					"sampling": map[string]interface{}{
						"rate": 0.5,
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	overrides, err := ReadAgentRuntimeOverrides(context.Background(), fakeClient, "ns1", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil {
		t.Fatal("expected non-nil overrides")
	}

	// Identity — SPIFFE
	if overrides.SpiffeTrustDomain == nil || *overrides.SpiffeTrustDomain != "override.local" {
		t.Errorf("SpiffeTrustDomain = %v", overrides.SpiffeTrustDomain)
	}

	// Identity — ClientRegistration
	if overrides.ClientRegistrationProvider == nil || *overrides.ClientRegistrationProvider != "keycloak" {
		t.Errorf("ClientRegistrationProvider = %v", overrides.ClientRegistrationProvider)
	}
	if overrides.ClientRegistrationRealm == nil || *overrides.ClientRegistrationRealm != "override-realm" {
		t.Errorf("ClientRegistrationRealm = %v", overrides.ClientRegistrationRealm)
	}
	if overrides.AdminCredentialsSecretName == nil || *overrides.AdminCredentialsSecretName != "my-secret" {
		t.Errorf("AdminCredentialsSecretName = %v", overrides.AdminCredentialsSecretName)
	}
	if overrides.AdminCredentialsSecretNamespace == nil || *overrides.AdminCredentialsSecretNamespace != "ns1" {
		t.Errorf("AdminCredentialsSecretNamespace = %v", overrides.AdminCredentialsSecretNamespace)
	}

	// Trace
	if overrides.TraceEndpoint == nil || *overrides.TraceEndpoint != "http://otel-collector:4317" {
		t.Errorf("TraceEndpoint = %v", overrides.TraceEndpoint)
	}
	if overrides.TraceProtocol == nil || *overrides.TraceProtocol != "grpc" {
		t.Errorf("TraceProtocol = %v", overrides.TraceProtocol)
	}
	if overrides.TraceSamplingRate == nil || *overrides.TraceSamplingRate != 0.5 {
		t.Errorf("TraceSamplingRate = %v", overrides.TraceSamplingRate)
	}
}

func TestReadAgentRuntimeOverrides_PartialOverrides(t *testing.T) {
	scheme := runtime.NewScheme()
	registerAgentRuntimeScheme(scheme)

	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": AgentRuntimeGroup + "/" + AgentRuntimeVersion,
			"kind":       AgentRuntimeKind,
			"metadata": map[string]interface{}{
				"name":      "my-agent-rt",
				"namespace": "ns1",
			},
			"spec": map[string]interface{}{
				"type": "agent",
				"targetRef": map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"name":       "my-agent",
				},
				"identity": map[string]interface{}{
					"spiffe": map[string]interface{}{
						"trustDomain": "custom.domain",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	overrides, err := ReadAgentRuntimeOverrides(context.Background(), fakeClient, "ns1", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides == nil {
		t.Fatal("expected non-nil overrides")
	}
	if overrides.SpiffeTrustDomain == nil || *overrides.SpiffeTrustDomain != "custom.domain" {
		t.Errorf("SpiffeTrustDomain = %v", overrides.SpiffeTrustDomain)
	}
	// Other fields should be nil
	if overrides.ClientRegistrationProvider != nil {
		t.Errorf("expected nil ClientRegistrationProvider, got %v", overrides.ClientRegistrationProvider)
	}
	if overrides.TraceEndpoint != nil {
		t.Errorf("expected nil TraceEndpoint, got %v", overrides.TraceEndpoint)
	}
}

func TestReadAgentRuntimeOverrides_NoTargetRefMatch(t *testing.T) {
	scheme := runtime.NewScheme()
	registerAgentRuntimeScheme(scheme)

	// CR targets a different workload
	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": AgentRuntimeGroup + "/" + AgentRuntimeVersion,
			"kind":       AgentRuntimeKind,
			"metadata": map[string]interface{}{
				"name":      "other-runtime",
				"namespace": "ns1",
			},
			"spec": map[string]interface{}{
				"type": "agent",
				"targetRef": map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"name":       "other-agent",
				},
				"identity": map[string]interface{}{
					"spiffe": map[string]interface{}{
						"trustDomain": "should-not-match",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	overrides, err := ReadAgentRuntimeOverrides(context.Background(), fakeClient, "ns1", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides != nil {
		t.Fatalf("expected nil overrides for non-matching targetRef, got %+v", overrides)
	}
}

func TestReadAgentRuntimeOverrides_CRDNotInstalled(t *testing.T) {
	// Empty scheme — no AgentRuntime types registered
	scheme := runtime.NewScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	overrides, err := ReadAgentRuntimeOverrides(context.Background(), fakeClient, "ns1", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides != nil {
		t.Fatalf("expected nil overrides when CRD not installed, got %+v", overrides)
	}
}
