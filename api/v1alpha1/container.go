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

	// EnvVars list of environment variables with support for ValueFrom
	// +optional
	EnvVars []EnvVar `json:"envVars,omitempty"`

	// SecretVolumes list of Kubernetes Secrets to mount as files
	// +optional
	SecretVolumes []SecretVolume `json:"secretVolumes,omitempty"`

	// HealthCheck defines a Docker health check for the container
	// +optional
	HealthCheck *HealthCheckConfig `json:"healthCheck,omitempty"`
}

// HealthCheckConfig defines health check parameters for the container
type HealthCheckConfig struct {
	// Test is the command to run (e.g. ["CMD", "curl", "-f", "http://localhost/"])
	Test []string `json:"test"`
	// Interval between checks (e.g. "30s")
	// +optional
	Interval string `json:"interval,omitempty"`
	// Timeout for a single check (e.g. "5s")
	// +optional
	Timeout string `json:"timeout,omitempty"`
	// Retries before reporting unhealthy
	// +optional
	Retries int `json:"retries,omitempty"`
}

// EnvVar defines an environment variable
type EnvVar struct {
	// Name of the environment variable
	Name string `json:"name"`
	// Value of the environment variable (literal)
	// +optional
	Value string `json:"value,omitempty"`
	// ValueFrom source for the environment variable's value
	// +optional
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// EnvVarSource represents a source for the value of an EnvVar
type EnvVarSource struct {
	// SecretKeyRef selects a key of a secret in the target container's namespace
	// +optional
	SecretKeyRef *SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// SecretKeySelector selects a key of a Secret
type SecretKeySelector struct {
	// Name of the referent
	Name string `json:"name"`
	// The key of the secret to select from. Must be a valid secret key.
	Key string `json:"key"`
}

// SecretVolume defines a mapping from a K8s Secret to a directory in the container
type SecretVolume struct {
	// SecretName is the name of the Kubernetes Secret
	SecretName string `json:"secretName"`
	// MountPath is the absolute path in the container where the secret should be mounted
	MountPath string `json:"mountPath"`
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

	// Health is the Docker health status (healthy, unhealthy, starting, none)
	Health string `json:"health,omitempty"`
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
