/*
Copyright 2023.

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ControlPlaneUpgradeSpec defines the desired state of ControlPlaneUpgrade
type ControlPlaneUpgradeSpec struct {
	Cluster                corev1.ObjectReference   `json:"cluster"`
	ControlPlane           corev1.ObjectReference   `json:"controlPlane"`
	MachinesRequireUpgrade []corev1.ObjectReference `json:"machinesRequireUpgrade"`
	NewVersion             KubernetesVersionBundle  `json:"newVersion"`
}

type KubernetesVersionBundle struct {
	KubernetesVersion string `json:"kubernetesVersion"`
	KubeletVersion    string `json:"kubeletVersion"`
	UpgraderImage     string `json:"upgraderImage"`
}

// ControlPlaneUpgradeStatus defines the observed state of ControlPlaneUpgrade
type ControlPlaneUpgradeStatus struct {
	RequireUpgrade int64 `json:"requireUpgrade"`
	Upgraded       int64 `json:"upgraded"`
	Ready          bool  `json:"ready"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cluster.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.newVersion.kubernetesVersion`
// +kubebuilder:printcolumn:name="Completed",type=integer,JSONPath=`.status.upgraded`
// +kubebuilder:printcolumn:name="Required",type=integer,JSONPath=`.status.requireUpgrade`
// +kubebuilder:printcolumn:name="Age",type="date",priority=1,JSONPath=".metadata.creationTimestamp"

// ControlPlaneUpgrade is the Schema for the controlplaneupgrades API
type ControlPlaneUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControlPlaneUpgradeSpec   `json:"spec,omitempty"`
	Status ControlPlaneUpgradeStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ControlPlaneUpgradeList contains a list of ControlPlaneUpgrade
type ControlPlaneUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlaneUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ControlPlaneUpgrade{}, &ControlPlaneUpgradeList{})
}
