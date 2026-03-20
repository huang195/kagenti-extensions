/*
Copyright 2025-2026.

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

package v1alpha1

import (
	"fmt"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/injector"
	. "github.com/onsi/ginkgo/v2" //nolint:revive // dot import is standard Ginkgo usage
	. "github.com/onsi/gomega"    //nolint:revive // dot import is standard Gomega usage
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var testNsCounter int

var _ = Describe("AuthBridge Pod Webhook", func() {
	var testNamespace string

	BeforeEach(func() {
		// Create a unique namespace with kagenti-enabled=true for each test
		testNsCounter++
		testNamespace = fmt.Sprintf("test-webhook-%d", testNsCounter)

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNamespace,
				Labels: map[string]string{
					"kagenti-enabled": "true",
				},
			},
		}
		err := k8sClient.Create(ctx, ns)
		Expect(err).NotTo(HaveOccurred())
	})

	newTestPod := func(name string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: name + "-",
				Namespace:    testNamespace,
				Labels:       labels,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "app",
						Image: "busybox:latest",
					},
				},
			},
		}
	}

	Context("when a Pod has kagenti.io/type=agent and kagenti.io/inject=enabled", func() {
		It("should inject sidecars", func() {
			pod := newTestPod("agent-pod", map[string]string{
				"kagenti.io/type":   "agent",
				"kagenti.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			// Re-fetch from the API server to get server-side mutations
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			// Verify sidecars were injected
			Expect(containerNames(pod.Spec.Containers)).To(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).To(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod has kagenti.io/type=tool and kagenti.io/inject=enabled", func() {
		It("should not inject sidecars (injectTools feature gate is disabled by default)", func() {
			pod := newTestPod("tool-pod", map[string]string{
				"kagenti.io/type":   "tool",
				"kagenti.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			Expect(containerNames(pod.Spec.Containers)).NotTo(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).NotTo(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod does not have kagenti.io/type label", func() {
		It("should not inject sidecars", func() {
			pod := newTestPod("no-type-pod", map[string]string{
				"kagenti.io/inject": "enabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			Expect(containerNames(pod.Spec.Containers)).NotTo(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).NotTo(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod has kagenti.io/inject=disabled", func() {
		It("should not inject sidecars", func() {
			pod := newTestPod("disabled-pod", map[string]string{
				"kagenti.io/type":   "agent",
				"kagenti.io/inject": "disabled",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			Expect(containerNames(pod.Spec.Containers)).NotTo(ContainElement(injector.EnvoyProxyContainerName))
			Expect(initContainerNames(pod.Spec.InitContainers)).NotTo(ContainElement(injector.ProxyInitContainerName))
		})
	})

	Context("when a Pod already has injected containers (idempotency)", func() {
		It("should not double-inject", func() {
			pod := newTestPod("already-injected-pod", map[string]string{
				"kagenti.io/type":   "agent",
				"kagenti.io/inject": "enabled",
			})
			// Pre-add the envoy-proxy container to simulate prior injection
			pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
				Name:  injector.EnvoyProxyContainerName,
				Image: "envoy:test",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			// Count envoy-proxy containers — should be exactly 1 (the pre-existing one)
			count := 0
			for _, c := range pod.Spec.Containers {
				if c.Name == injector.EnvoyProxyContainerName {
					count++
				}
			}
			Expect(count).To(Equal(1))
		})
	})

	Context("when a Pod already has the combined authbridge container (idempotency)", func() {
		It("should not double-inject", func() {
			pod := newTestPod("already-combined-pod", map[string]string{
				"kagenti.io/type":   "agent",
				"kagenti.io/inject": "enabled",
			})
			// Pre-add the authbridge container to simulate prior combined injection
			pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
				Name:  injector.AuthBridgeContainerName,
				Image: "authbridge:test",
			})

			err := k8sClient.Create(ctx, pod)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)
			Expect(err).NotTo(HaveOccurred())

			// Should not have added any additional sidecar containers
			Expect(containerNames(pod.Spec.Containers)).NotTo(ContainElement(injector.EnvoyProxyContainerName))
			Expect(containerNames(pod.Spec.Containers)).To(ContainElement(injector.AuthBridgeContainerName))
		})
	})
})

var _ = Describe("deriveWorkloadName", func() {
	DescribeTable("should extract the correct workload name",
		func(generateName, name, uid, expected string) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: generateName,
					Name:         name,
					UID:          types.UID(uid),
				},
			}
			Expect(deriveWorkloadName(pod)).To(Equal(expected))
		},
		Entry("Deployment Pod (GenerateName with ReplicaSet hash)",
			"myapp-7d4f8b9c5-", "", "", "myapp-7d4f8b9c5"),
		Entry("StatefulSet Pod (GenerateName without hash)",
			"myapp-", "", "", "myapp"),
		Entry("Bare Pod with Name only",
			"", "my-bare-pod", "", "my-bare-pod"),
		Entry("Pod with both GenerateName and Name (GenerateName wins)",
			"myapp-abc12-", "myapp-abc12-xyz", "", "myapp-abc12"),
		Entry("GenerateName with no trailing hyphen",
			"myapp", "", "", "myapp"),
		Entry("Both empty — falls back to UID",
			"", "", "abc-123-uid", "abc-123-uid"),
	)
})

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for i, c := range containers {
		names[i] = c.Name
	}
	return names
}

func initContainerNames(containers []corev1.Container) []string {
	return containerNames(containers)
}
