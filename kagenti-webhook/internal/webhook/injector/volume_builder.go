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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// BuildRequiredVolumes creates all volumes required for sidecar containers (with SPIRE)
func BuildRequiredVolumes() []corev1.Volume {
	// Helper for pointer to bool
	isReadOnly := true

	return []corev1.Volume{
		{
			Name: "shared-data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			// Updated from HostPath to CSI
			Name: "spire-agent-socket",
			VolumeSource: corev1.VolumeSource{
				CSI: &corev1.CSIVolumeSource{
					Driver:   "csi.spiffe.io",
					ReadOnly: &isReadOnly,
				},
			},
		},
		{
			Name: "spiffe-helper-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "spiffe-helper-config",
					},
				},
			},
		},
		{
			Name: "svid-output",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "envoy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "envoy-config",
					},
				},
			},
		},
		{
			Name: "authproxy-routes",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "authproxy-routes",
					},
					Optional: ptr.To(true),
				},
			},
		},
	}
}

// BuildRequiredVolumesNoSpire creates volumes required for sidecar containers without SPIRE
// This excludes spire-agent-socket, spiffe-helper-config, and svid-output volumes
func BuildRequiredVolumesNoSpire() []corev1.Volume {
	return []corev1.Volume{
		{
			Name: "shared-data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "envoy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "envoy-config",
					},
				},
			},
		},
		{
			Name: "authproxy-routes",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "authproxy-routes",
					},
					Optional: ptr.To(true),
				},
			},
		},
	}
}

// BuildResolvedVolumes creates volumes using resolved config values.
// When a resolved envoy config name is provided, the envoy-config volume
// references that ConfigMap instead of the default "envoy-config" one.
// This enables per-workload envoy configs created at admission time.
func BuildResolvedVolumes(spireEnabled bool, envoyConfigMapName string) []corev1.Volume {
	if envoyConfigMapName == "" {
		envoyConfigMapName = EnvoyConfigMapName
	}

	volumes := []corev1.Volume{
		{
			Name: "shared-data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	if spireEnabled {
		isReadOnly := true
		volumes = append(volumes,
			corev1.Volume{
				Name: "spire-agent-socket",
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver:   "csi.spiffe.io",
						ReadOnly: &isReadOnly,
					},
				},
			},
			corev1.Volume{
				Name: "spiffe-helper-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: SpiffeHelperConfigMapName,
						},
					},
				},
			},
			corev1.Volume{
				Name: "svid-output",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		)
	}

	volumes = append(volumes,
		corev1.Volume{
			Name: "envoy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: envoyConfigMapName,
					},
				},
			},
		},
		corev1.Volume{
			Name: "authproxy-routes",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: AuthproxyRoutesConfigMapName,
					},
					Optional: ptr.To(true),
				},
			},
		},
	)

	return volumes
}
