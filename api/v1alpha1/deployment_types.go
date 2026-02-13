/*
Copyright 2024.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DockerDeploymentSpec defines the desired state of DockerDeployment
type DockerDeploymentSpec struct {
	// Replicas is the number of desired containers
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Selector is a label query over containers that should match the replica count.
	// It must match the container template's labels.
	Selector *metav1.LabelSelector `json:"selector"`

	// Template describes the DockerContainers that will be created.
	Template DockerContainerTemplate `json:"template"`
}

// DockerContainerTemplate describes the data a container should have when created from a template
type DockerContainerTemplate struct {
	// Standard object's metadata.
	// +optional
	Metadata ObjectMeta `json:"metadata,omitempty"`

	// Specification of the desired behavior of the container.
	// +optional
	Spec DockerContainerSpec `json:"spec,omitempty"`
}

// ObjectMeta is metadata that all persisted resources must have, which includes all objects
// users must create.
type ObjectMeta struct {
	// Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: http://kubernetes.io/docs/user-guide/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: http://kubernetes.io/docs/user-guide/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// DockerDeploymentStatus defines the observed state of DockerDeployment
type DockerDeploymentStatus struct {
	// AvailableReplicas is the total number of available (running) containers
	AvailableReplicas int32 `json:"availableReplicas"`

	// Conditions represents the latest available observations of an object's current state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas",description="Desired replicas"
//+kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.availableReplicas",description="Available replicas"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// DockerDeployment is the Schema for the dockerdeployments API
type DockerDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DockerDeploymentSpec   `json:"spec,omitempty"`
	Status DockerDeploymentStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// DockerDeploymentList contains a list of DockerDeployment
type DockerDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockerDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DockerDeployment{}, &DockerDeploymentList{})
}
