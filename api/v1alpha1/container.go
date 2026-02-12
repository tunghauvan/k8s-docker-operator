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

// DockerContainerSpec defines the desired state of DockerContainer
type DockerContainerSpec struct {

	// Image is the Docker image to run
	Image string `json:"image,omitempty"`

	// ContainerName is the name of the container on the docker host
	ContainerName string `json:"containerName,omitempty"`

	// Ports list of port mappings (e.g. "8080:80")
	// +optional
	Ports []string `json:"ports,omitempty"`

	// Command to run in the container
	// +optional
	Command []string `json:"command,omitempty"`

	// Env list of environment variables (e.g. "KEY=VALUE")
	// +optional
	Env []string `json:"env,omitempty"`

	// RestartPolicy for the container (no, on-failure, always, unless-stopped)
	// +optional
	RestartPolicy string `json:"restartPolicy,omitempty"`
	// DockerHostRef is the name of the DockerHost CR to use.
	// If empty, defaults to the local Docker socket.
	// +optional
	DockerHostRef string `json:"dockerHostRef,omitempty"`

	// ImagePullSecret is the name of the Secret containing Docker registry credentials
	// +optional
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// VolumeMounts list of volumes to mount into the container
	// +optional
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// Services defines the ports to expose via Kubernetes Service
	// +optional
	Services []ServicePort `json:"services,omitempty"`

	// NetworkMode for the container (e.g. "host", "bridge", "kind")
	// +optional
	NetworkMode string `json:"networkMode,omitempty"`
}

type ServicePort struct {
	// Port is the port number to expose on the Kubernetes Service
	Port int32 `json:"port"`
	// TargetPort is the port number on the container to forward to
	TargetPort int32 `json:"targetPort"`
	// Name is the name of the port
	// +optional
	Name string `json:"name,omitempty"`
}

// VolumeMount defines a volume mount
type VolumeMount struct {
	// HostPath is the path on the host
	HostPath string `json:"hostPath"`
	// ContainerPath is the path in the container
	ContainerPath string `json:"containerPath"`
	// ReadOnly if true, mounts the volume as read-only
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// DockerContainerStatus defines the observed state of DockerContainer
type DockerContainerStatus struct {

	// ID of the container
	ID string `json:"id,omitempty"`

	// State request state of the container
	State string `json:"state,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// DockerContainer is the Schema for the dockercontainers API
type DockerContainer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DockerContainerSpec   `json:"spec,omitempty"`
	Status DockerContainerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// DockerContainerList contains a list of DockerContainer
type DockerContainerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockerContainer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DockerContainer{}, &DockerContainerList{})
}
