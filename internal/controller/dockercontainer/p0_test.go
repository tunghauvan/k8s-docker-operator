package dockercontainer

import (
	"context"
	"testing"

	appv1alpha1 "github.com/tunghauvan/k8s-docker-operator/api/v1alpha1"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/common"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestP0_HealthCheckStatus verifies that health status from Docker is reported in the CR status
func TestP0_HealthCheckStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	crName := "test-healthcheck"
	crNamespace := "default"

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: crNamespace,
		},
		Spec: appv1alpha1.DockerContainerSpec{
			Image:         "nginx:latest",
			ContainerName: "healthy-nginx",
			HealthCheck: &appv1alpha1.HealthCheckConfig{
				Test:     []string{"CMD", "curl", "-f", "http://localhost/"},
				Interval: "10s",
				Timeout:  "5s",
				Retries:  3,
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()

	mockDocker := &common.MockDockerClient{
		Containers: []dockertypes.Container{
			{
				ID:    "healthcheck-id",
				Names: []string{"/healthy-nginx"},
				State: "running",
				Image: "nginx:latest",
				NetworkSettings: &dockertypes.SummaryNetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"bridge": {IPAddress: "172.17.0.5"},
					},
				},
			},
		},
		HealthStatus: "healthy",
	}

	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: crNamespace}}
	ctx := context.Background()

	_, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)

	// Verify health status in CR
	updatedCR := &appv1alpha1.DockerContainer{}
	err = k8sClient.Get(ctx, req.NamespacedName, updatedCR)
	require.NoError(t, err)
	assert.Equal(t, "healthy", updatedCR.Status.Health)
	assert.Equal(t, "running", updatedCR.Status.State)
}

// TestP0_HealthCheckUnhealthy verifies that unhealthy status is correctly reported
func TestP0_HealthCheckUnhealthy(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-unhealthy",
			Namespace: "default",
		},
		Spec: appv1alpha1.DockerContainerSpec{
			Image:         "nginx:latest",
			ContainerName: "unhealthy-nginx",
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()

	mockDocker := &common.MockDockerClient{
		Containers: []dockertypes.Container{
			{
				ID:    "unhealthy-id",
				Names: []string{"/unhealthy-nginx"},
				State: "running",
				Image: "nginx:latest",
				NetworkSettings: &dockertypes.SummaryNetworkSettings{
					Networks: map[string]*network.EndpointSettings{},
				},
			},
		},
		HealthStatus: "unhealthy",
	}

	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-unhealthy", Namespace: "default"}}

	_, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)

	updatedCR := &appv1alpha1.DockerContainer{}
	err = k8sClient.Get(ctx, req.NamespacedName, updatedCR)
	require.NoError(t, err)
	assert.Equal(t, "unhealthy", updatedCR.Status.Health)
}

// TestP0_DriftDetection_EnvChange verifies that env changes trigger container recreation
func TestP0_DriftDetection_EnvChange(t *testing.T) {
	// Build a mock inspected container with old env
	inspected := dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: "always"},
			},
		},
		Config: &container.Config{
			Image: "nginx:latest",
			Cmd:   []string{"nginx", "-g", "daemon off;"},
			Env:   []string{"OLD_VAR=old_value"},
		},
	}

	// Spec with new env
	spec := &appv1alpha1.DockerContainerSpec{
		Image:   "nginx:latest",
		Command: []string{"nginx", "-g", "daemon off;"},
		Env:     []string{"NEW_VAR=new_value"},
	}

	assert.True(t, needsRecreate(inspected, spec),
		"Should detect env drift and trigger recreate")
}

// TestP0_DriftDetection_CommandChange verifies that command changes trigger recreation
func TestP0_DriftDetection_CommandChange(t *testing.T) {
	inspected := dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{
			Image: "nginx:latest",
			Cmd:   []string{"nginx"},
		},
	}

	spec := &appv1alpha1.DockerContainerSpec{
		Image:   "nginx:latest",
		Command: []string{"nginx", "-g", "daemon off;"},
	}

	assert.True(t, needsRecreate(inspected, spec),
		"Should detect command drift and trigger recreate")
}

// TestP0_DriftDetection_NoChange verifies no recreation when config matches
func TestP0_DriftDetection_NoChange(t *testing.T) {
	inspected := dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: "always"},
			},
		},
		Config: &container.Config{
			Image: "nginx:latest",
			Cmd:   []string{"nginx", "-g", "daemon off;"},
			Env:   []string{"FOO=bar", "PATH=/usr/bin"},
		},
	}

	spec := &appv1alpha1.DockerContainerSpec{
		Image:         "nginx:latest",
		Command:       []string{"nginx", "-g", "daemon off;"},
		Env:           []string{"FOO=bar"},
		RestartPolicy: "always",
	}

	assert.False(t, needsRecreate(inspected, spec),
		"Should not trigger recreate when config matches")
}

// TestP0_DriftDetection_RestartPolicyChange verifies restart policy drift detection
func TestP0_DriftDetection_RestartPolicyChange(t *testing.T) {
	inspected := dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: "always"},
			},
		},
		Config: &container.Config{
			Image: "nginx:latest",
		},
	}

	spec := &appv1alpha1.DockerContainerSpec{
		Image:         "nginx:latest",
		RestartPolicy: "unless-stopped",
	}

	assert.True(t, needsRecreate(inspected, spec),
		"Should detect restart policy drift")
}

// TestP0_TunnelAuthSecret verifies that tunnel auth secret is created and used
func TestP0_TunnelAuthSecret(t *testing.T) {
	t.Skip("Tunnel logic moved to DockerService controller")
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-tunnel-auth",
			Namespace: "default",
			UID:       "test-uid-123",
		},
		Spec: appv1alpha1.DockerContainerSpec{
			Image:         "nginx:latest",
			ContainerName: "auth-app",
			Services: []appv1alpha1.ServicePort{
				{Port: 80, TargetPort: 80},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()

	mockDocker := &common.MockDockerClient{
		Containers: []dockertypes.Container{
			{
				ID:    "main-id",
				Names: []string{"/auth-app"},
				State: "running",
				Image: "nginx:latest",
				NetworkSettings: &dockertypes.SummaryNetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"bridge": {IPAddress: "172.17.0.2"},
					},
				},
			},
		},
	}

	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-tunnel-auth", Namespace: "default"}}

	// First reconcile: creates tunnel server + auth secret
	_, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)

	// Verify auth secret was created
	authSecret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "tunnel-auth-test-tunnel-auth", Namespace: "default"}, authSecret)
	require.NoError(t, err, "Tunnel auth secret should be created")
	assert.NotEmpty(t, authSecret.Data["token"], "Token should not be empty")

	// Second reconcile: uses existing token, should not create new one
	token1 := string(authSecret.Data["token"])
	_, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)

	authSecret2 := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "tunnel-auth-test-tunnel-auth", Namespace: "default"}, authSecret2)
	require.NoError(t, err)
	token2 := string(authSecret2.Data["token"])
	assert.Equal(t, token1, token2, "Token should be stable across reconciles")
}

// TestP0_BuildDockerHealthCheck verifies conversion of CRD HealthCheckConfig to Docker HealthConfig
func TestP0_BuildDockerHealthCheck(t *testing.T) {
	// Test nil input
	assert.Nil(t, buildDockerHealthCheck(nil))

	// Test valid input
	hc := &appv1alpha1.HealthCheckConfig{
		Test:     []string{"CMD", "curl", "-f", "http://localhost/"},
		Interval: "30s",
		Timeout:  "5s",
		Retries:  3,
	}

	result := buildDockerHealthCheck(hc)
	require.NotNil(t, result)
	assert.Equal(t, []string{"CMD", "curl", "-f", "http://localhost/"}, result.Test)
	assert.Equal(t, 3, result.Retries)

	// Test with invalid duration strings (should not panic)
	hc2 := &appv1alpha1.HealthCheckConfig{
		Test:     []string{"CMD", "true"},
		Interval: "invalid",
	}
	result2 := buildDockerHealthCheck(hc2)
	require.NotNil(t, result2)
	assert.Equal(t, []string{"CMD", "true"}, result2.Test)
}
