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

// DockerHostSpec defines the desired state of DockerHost
type DockerHostSpec struct {
	// HostURL is the URL of the Docker Daemon (e.g. tcp://1.2.3.4:2376 or unix:///var/run/docker.sock)
	HostURL string `json:"hostURL,omitempty"`

	// TLSSecretName is the name of the Secret containing ca.pem, cert.pem, key.pem
	// +optional
	TLSSecretName string `json:"tlsSecretName,omitempty"`
}

// DockerHostStatus defines the observed state of DockerHost
type DockerHostStatus struct {
	// Phase indicates if we can connect to the host (Connected, Error)
	Phase string `json:"phase,omitempty"`

	// Message info about the connection status
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// DockerHost is the Schema for the dockerhosts API
type DockerHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DockerHostSpec   `json:"spec,omitempty"`
	Status DockerHostStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// DockerHostList contains a list of DockerHost
type DockerHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockerHost `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DockerHost{}, &DockerHostList{})
}
