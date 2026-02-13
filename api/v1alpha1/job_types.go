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

// DockerJobPhase represents the current phase of a DockerJob
type DockerJobPhase string

const (
	JobPhasePending   DockerJobPhase = "Pending"
	JobPhaseRunning   DockerJobPhase = "Running"
	JobPhaseSucceeded DockerJobPhase = "Succeeded"
	JobPhaseFailed    DockerJobPhase = "Failed"
)

// DockerJobSpec defines the desired state of DockerJob
type DockerJobSpec struct {

	// Image is the Docker image to run
	Image string `json:"image"`

	// ContainerName is the name of the container on the docker host.
	// Defaults to "job-<metadata.name>" if empty.
	// +optional
	ContainerName string `json:"containerName,omitempty"`

	// Command to run in the container (Docker ENTRYPOINT)
	// +optional
	Command []string `json:"command,omitempty"`

	// Args are arguments to the command (Docker CMD)
	// +optional
	Args []string `json:"args,omitempty"`

	// Env list of environment variables (e.g. "KEY=VALUE")
	// +optional
	Env []string `json:"env,omitempty"`

	// EnvVars list of environment variables with support for ValueFrom (K8s Secrets)
	// +optional
	EnvVars []EnvVar `json:"envVars,omitempty"`

	// VolumeMounts list of volumes to mount into the container
	// +optional
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`

	// SecretVolumes list of Kubernetes Secrets to mount as files
	// +optional
	SecretVolumes []SecretVolume `json:"secretVolumes,omitempty"`

	// DockerHostRef is the name of the DockerHost CR to use.
	// If empty, defaults to the local Docker socket.
	// +optional
	DockerHostRef string `json:"dockerHostRef,omitempty"`

	// ImagePullSecret is the name of the Secret containing Docker registry credentials
	// +optional
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// RestartPolicy for the job container. Only "Never" or "OnFailure" are valid.
	// Default: "Never"
	// +optional
	RestartPolicy string `json:"restartPolicy,omitempty"`

	// BackoffLimit specifies the number of retries before marking this job as failed.
	// Default: 0 (no retries)
	// +optional
	BackoffLimit int32 `json:"backoffLimit,omitempty"`

	// ActiveDeadlineSeconds specifies a timeout in seconds. If the job does not
	// complete within this duration, it is terminated and marked as Failed.
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// TTLSecondsAfterFinished specifies the number of seconds after job completion
	// (Succeeded or Failed) before the Docker container is cleaned up.
	// If unset, the container is not automatically removed.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// Resources defines CPU and Memory limits for the container
	// +optional
	Resources *ResourceRequirements `json:"resources,omitempty"`
}

// DockerJobStatus defines the observed state of DockerJob
type DockerJobStatus struct {

	// Phase is the current phase of the job (Pending, Running, Succeeded, Failed)
	Phase DockerJobPhase `json:"phase,omitempty"`

	// ContainerID is the Docker container ID
	ContainerID string `json:"containerID,omitempty"`

	// StartTime is the time the job container was started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is the time the job container finished
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// ExitCode is the exit code of the container process
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// Attempts is the number of times the container has been run
	Attempts int32 `json:"attempts,omitempty"`

	// Message is a human-readable message indicating details about the job status
	// +optional
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Exit Code",type=integer,JSONPath=`.status.exitCode`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DockerJob is the Schema for the dockerjobs API.
// It runs a one-off container on a Docker host and tracks its completion.
type DockerJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DockerJobSpec   `json:"spec,omitempty"`
	Status DockerJobStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// DockerJobList contains a list of DockerJob
type DockerJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockerJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DockerJob{}, &DockerJobList{})
}
