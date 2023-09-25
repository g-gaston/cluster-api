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

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// NodeUpgradeSpec defines the desired state of NodeUpgrade
type NodeUpgradeSpec struct {
	Machine   corev1.ObjectReference  `json:"machine"`
	Node       corev1.ObjectReference  `json:"node"`
	NewVersion KubernetesVersionBundle `json:"newVersion"`
}

// NodeUpgradeStatus defines the observed state of NodeUpgrade
type NodeUpgradeStatus struct {
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
	Phase      string               `json:"phase"`
	Completed  bool                 `json:"completed"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Machine",type=string,JSONPath=`.spec.machine.name`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.node.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.newVersion.kubernetesVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Completed",type=boolean,JSONPath=`.status.completed`
// +kubebuilder:printcolumn:name="Age",type="date",priority=1,JSONPath=".metadata.creationTimestamp"

// NodeUpgrade is the Schema for the nodeupgrades API
type NodeUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeUpgradeSpec   `json:"spec,omitempty"`
	Status NodeUpgradeStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// NodeUpgradeList contains a list of NodeUpgrade
type NodeUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeUpgrade{}, &NodeUpgradeList{})
}
