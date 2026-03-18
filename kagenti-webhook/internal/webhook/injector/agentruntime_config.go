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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var arConfigLog = logf.Log.WithName("agentruntime-config")

// AgentRuntimeOverrides holds the per-workload overrides extracted from an
// AgentRuntime CR (agent.kagenti.dev/v1alpha1). Nil pointer fields mean
// "no override". The struct mirrors the CRD spec from kagenti-operator PR #212.
type AgentRuntimeOverrides struct {
	// Identity — from .spec.identity.spiffe
	SpiffeTrustDomain *string

	// Identity — from .spec.identity.clientRegistration
	ClientRegistrationProvider      *string
	ClientRegistrationRealm         *string
	AdminCredentialsSecretName      *string
	AdminCredentialsSecretNamespace *string

	// Observability — from .spec.trace
	TraceEndpoint     *string
	TraceProtocol     *string  // "grpc" or "http"
	TraceSamplingRate *float64 // 0.0–1.0
}

// ReadAgentRuntimeOverrides reads the AgentRuntime CR for a given workload
// using an unstructured client. It lists AgentRuntimes in the namespace and
// finds the one whose spec.targetRef.name matches workloadName.
// Returns (nil, nil) if no matching AgentRuntime CR is found or if the CRD
// is not installed in the cluster.
func ReadAgentRuntimeOverrides(ctx context.Context, c client.Reader, namespace, workloadName string) (*AgentRuntimeOverrides, error) {
	gvk := schema.GroupVersionKind{
		Group:   AgentRuntimeGroup,
		Version: AgentRuntimeVersion,
		Kind:    AgentRuntimeKind + "List",
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)

	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		// CRD not installed or API error — expected during graceful degradation
		arConfigLog.V(1).Info("AgentRuntime CRD not available or list failed",
			"namespace", namespace, "error", err)
		return nil, nil
	}

	// Find the AgentRuntime whose spec.targetRef.name matches the workload
	for i := range list.Items {
		obj := &list.Items[i]
		targetName, found, _ := unstructured.NestedString(obj.Object, "spec", "targetRef", "name")
		if !found || targetName != workloadName {
			continue
		}

		arConfigLog.Info("Found matching AgentRuntime CR",
			"namespace", namespace, "crName", obj.GetName(), "targetRef.name", workloadName)
		return extractOverrides(obj), nil
	}

	arConfigLog.V(1).Info("No AgentRuntime CR targets this workload",
		"namespace", namespace, "workloadName", workloadName)
	return nil, nil
}

// extractOverrides reads the overridable fields from an AgentRuntime CR.
func extractOverrides(obj *unstructured.Unstructured) *AgentRuntimeOverrides {
	overrides := &AgentRuntimeOverrides{}

	// .spec.identity.spiffe.trustDomain
	if v, found, _ := unstructured.NestedString(obj.Object, "spec", "identity", "spiffe", "trustDomain"); found && v != "" {
		overrides.SpiffeTrustDomain = &v
	}

	// .spec.identity.clientRegistration.provider
	if v, found, _ := unstructured.NestedString(obj.Object, "spec", "identity", "clientRegistration", "provider"); found && v != "" {
		overrides.ClientRegistrationProvider = &v
	}

	// .spec.identity.clientRegistration.realm
	if v, found, _ := unstructured.NestedString(obj.Object, "spec", "identity", "clientRegistration", "realm"); found && v != "" {
		overrides.ClientRegistrationRealm = &v
	}

	// .spec.identity.clientRegistration.adminCredentialsSecret.name
	if v, found, _ := unstructured.NestedString(obj.Object, "spec", "identity", "clientRegistration", "adminCredentialsSecret", "name"); found && v != "" {
		overrides.AdminCredentialsSecretName = &v
	}

	// .spec.identity.clientRegistration.adminCredentialsSecret.namespace
	if v, found, _ := unstructured.NestedString(obj.Object, "spec", "identity", "clientRegistration", "adminCredentialsSecret", "namespace"); found && v != "" {
		overrides.AdminCredentialsSecretNamespace = &v
	}

	// .spec.trace.endpoint
	if v, found, _ := unstructured.NestedString(obj.Object, "spec", "trace", "endpoint"); found && v != "" {
		overrides.TraceEndpoint = &v
	}

	// .spec.trace.protocol
	if v, found, _ := unstructured.NestedString(obj.Object, "spec", "trace", "protocol"); found && v != "" {
		overrides.TraceProtocol = &v
	}

	// .spec.trace.sampling.rate (float64)
	if v, found, _ := unstructured.NestedFloat64(obj.Object, "spec", "trace", "sampling", "rate"); found {
		overrides.TraceSamplingRate = &v
	}

	arConfigLog.Info("AgentRuntime overrides extracted",
		"hasSpiffeTrustDomain", overrides.SpiffeTrustDomain != nil,
		"hasClientRegistration", overrides.ClientRegistrationProvider != nil,
		"hasTrace", overrides.TraceEndpoint != nil)

	return overrides
}
