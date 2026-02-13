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

// DockerServiceSpec defines the desired state of DockerService
type DockerServiceSpec struct {
	// ContainerRef references the DockerContainer to expose.
	// Mutually exclusive with Selector.
	// +optional
	ContainerRef string `json:"containerRef,omitempty"`

	// Selector matches multiple DockerContainers to expose (Load Balancing).
	// Mutually exclusive with ContainerRef.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// Ports list of port mappings from container to K8s Service
	Ports []ServicePort `json:"ports"`

	// NetworkMode for tunnel client containers (e.g. "kind", "host")
	// If empty, defaults to "bridge" (or inherited logic)
	// +optional
	NetworkMode string `json:"networkMode,omitempty"`
}

// DockerServiceStatus defines the observed state of DockerService
type DockerServiceStatus struct {
	// Phase indicates the status of the service (Pending, Active, Error)
	Phase string `json:"phase,omitempty"`

	// TunnelClients count of active tunnel client containers
	TunnelClients int `json:"tunnelClients,omitempty"`

	// TunnelServerURL internal K8s URL of the tunnel server
	TunnelServerURL string `json:"tunnelServerURL,omitempty"`

	// Message human readable status
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// DockerService is the Schema for the dockerservices API
type DockerService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DockerServiceSpec   `json:"spec,omitempty"`
	Status DockerServiceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// DockerServiceList contains a list of DockerService
type DockerServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockerService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DockerService{}, &DockerServiceList{})
}
