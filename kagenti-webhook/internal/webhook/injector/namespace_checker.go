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

var nsLog = logf.Log.WithName("namespace-checker")

// DEPRECATED, used by Agent CR. Remove CheckNamespaceInjectionEnabled after Agent CR is deleted and use IsNamespaceInjectionEnabled instead.

// checks if a namespace has injection enabled via labels or annotations
func CheckNamespaceInjectionEnabled(ctx context.Context, k8sClient client.Client, namespaceName, labelKey, annotationKey string) (bool, error) {
	nsLog.Info("Checking namespace injection settings", "namespace", namespaceName, "labelKey", labelKey, "annotationKey", annotationKey)

	namespace := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: namespaceName}, namespace); err != nil {
		nsLog.Error(err, "Failed to fetch namespace", "namespace", namespaceName)
		return false, err
	}

	nsLog.Info("Namespace fetched", "namespace", namespaceName, "labels", namespace.Labels, "annotations", namespace.Annotations)

	// Check NS label (e.g., kagenti-enabled: "true")
	if namespace.Labels != nil {
		if namespace.Labels[labelKey] == "true" {
			nsLog.Info("Namespace injection enabled via label", "namespace", namespaceName, "labelKey", labelKey, "labelValue", "true")
			return true, nil
		}
	}

	// Check annotation (e.g., kagenti.dev/inject: "true")
	if namespace.Annotations != nil {
		if namespace.Annotations[annotationKey] == "true" {
			nsLog.Info("Namespace injection enabled via annotation", "namespace", namespaceName, "annotationKey", annotationKey, "annotationValue", "true")
			return true, nil
		}
	}

	nsLog.Info("Namespace injection not enabled", "namespace", namespaceName)
	return false, nil
}

// checks if a namespace has injection enabled via labels or annotations
func IsNamespaceInjectionEnabled(ctx context.Context, k8sClient client.Client, namespaceName, labelKey string) (bool, error) {
	nsLog.Info("Checking namespace injection settings", "namespace", namespaceName, "labelKey", labelKey)

	namespace := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: namespaceName}, namespace); err != nil {
		nsLog.Error(err, "Failed to fetch namespace", "namespace", namespaceName)
		return false, err
	}

	nsLog.Info("Namespace fetched", "namespace", namespaceName, "labels", namespace.Labels, "annotations", namespace.Annotations)

	// Check NS label (e.g., kagenti-enabled: "true")
	if namespace.Labels != nil {
		if namespace.Labels[labelKey] == "true" {
			nsLog.Info("Namespace injection enabled via label", "namespace", namespaceName, "labelKey", labelKey, "labelValue", "true")
			return true, nil
		}
	}

	nsLog.Info("Namespace injection not enabled", "namespace", namespaceName)
	return false, nil
}
