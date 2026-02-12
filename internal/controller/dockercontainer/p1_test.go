package dockercontainer

import (
	"context"
	"fmt"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appv1alpha1 "github.com/tunghauvan/k8s-docker-operator/api/v1alpha1"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/common"
)

// TestP1_ResourceLimits_BuildDockerResources tests the resource limit conversion helper.
func TestP1_ResourceLimits_BuildDockerResources(t *testing.T) {
	tests := []struct {
		name       string
		resources  *appv1alpha1.ResourceRequirements
		wantCPU    int64
		wantMemory int64
	}{
		{
			name:       "nil resources",
			resources:  nil,
			wantCPU:    0,
			wantMemory: 0,
		},
		{
			name:       "half CPU, 256m memory",
			resources:  &appv1alpha1.ResourceRequirements{CPULimit: "0.5", MemoryLimit: "256m"},
			wantCPU:    500000000,
			wantMemory: 256 * 1024 * 1024,
		},
		{
			name:       "2 CPUs, 1g memory",
			resources:  &appv1alpha1.ResourceRequirements{CPULimit: "2", MemoryLimit: "1g"},
			wantCPU:    2000000000,
			wantMemory: 1024 * 1024 * 1024,
		},
		{
			name:       "only CPU",
			resources:  &appv1alpha1.ResourceRequirements{CPULimit: "1"},
			wantCPU:    1000000000,
			wantMemory: 0,
		},
		{
			name:       "only memory in KB",
			resources:  &appv1alpha1.ResourceRequirements{MemoryLimit: "512k"},
			wantCPU:    0,
			wantMemory: 512 * 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := buildDockerResources(tt.resources)
			assert.Equal(t, tt.wantCPU, res.NanoCPUs, "NanoCPUs mismatch")
			assert.Equal(t, tt.wantMemory, res.Memory, "Memory mismatch")
		})
	}
}

// TestP1_ParseMemoryString tests memory string parsing.
func TestP1_ParseMemoryString(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"256m", 256 * 1024 * 1024},
		{"1g", 1024 * 1024 * 1024},
		{"512k", 512 * 1024},
		{"1024", 1024},
		{"", 0},
		{"invalid", 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := parseMemoryString(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestP1_ResourceLimits_DriftDetection tests that resource changes trigger recreation.
func TestP1_ResourceLimits_DriftDetection(t *testing.T) {
	spec := &appv1alpha1.DockerContainerSpec{
		Image: "nginx:latest",
		Resources: &appv1alpha1.ResourceRequirements{
			CPULimit:    "0.5",
			MemoryLimit: "256m",
		},
	}

	// Matching resources — no drift
	inspected := types.ContainerJSON{
		Config: &container.Config{
			Image: "nginx:latest",
		},
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				Resources: container.Resources{
					NanoCPUs: 500000000,
					Memory:   256 * 1024 * 1024,
				},
			},
		},
	}
	assert.False(t, needsRecreate(inspected, spec), "should NOT recreate when resources match")

	// Mismatched CPU — should drift
	inspected.HostConfig.Resources.NanoCPUs = 1000000000
	assert.True(t, needsRecreate(inspected, spec), "should recreate when CPU differs")

	// Mismatched Memory — should drift
	inspected.HostConfig.Resources.NanoCPUs = 500000000
	inspected.HostConfig.Resources.Memory = 128 * 1024 * 1024
	assert.True(t, needsRecreate(inspected, spec), "should recreate when memory differs")
}

// TestP1_MultiPortTunnel_Naming tests that tunnel clients get indexed names.
func TestP1_MultiPortTunnel_Naming(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-port-app",
			Namespace: "default",
		},
		Spec: appv1alpha1.DockerContainerSpec{
			ContainerName: "multi-port-app",
			Image:         "nginx:latest",
			Services: []appv1alpha1.ServicePort{
				{Name: "http", Port: 80, TargetPort: 8080},
				{Name: "grpc", Port: 9090, TargetPort: 9090},
				{Name: "metrics", Port: 9100, TargetPort: 9100},
			},
		},
	}

	// The mock needs a main container with network settings for IP discovery
	mockDocker := &common.MockDockerClient{
		Containers: []types.Container{
			{
				ID:    "main-container-id",
				Names: []string{"/multi-port-app"},
				State: "running",
				Image: "nginx:latest",
				NetworkSettings: &types.SummaryNetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"bridge": {IPAddress: "172.17.0.2"},
					},
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	wsURL := "ws://tunnel-multi-port-app.default.svc:8081/ws"

	err := r.reconcileTunnelClient(context.Background(), mockDocker, cr, wsURL)
	assert.NoError(t, err)

	// Verify 3 tunnel clients were created (one per service port)
	expectedNames := []string{
		"multi-port-app-tunnel-0",
		"multi-port-app-tunnel-1",
		"multi-port-app-tunnel-2",
	}

	for _, expected := range expectedNames {
		found := false
		for _, created := range mockDocker.Created {
			if created == expected {
				found = true
				break
			}
		}
		assert.True(t, found, fmt.Sprintf("Expected tunnel client %s to be created", expected))
	}
}

// TestP1_Reconcile_WithResourceLimits tests full reconcile with resource limits set.
func TestP1_Reconcile_WithResourceLimits(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resource-limited-app",
			Namespace: "default",
		},
		Spec: appv1alpha1.DockerContainerSpec{
			ContainerName: "resource-limited-app",
			Image:         "nginx:latest",
			Resources: &appv1alpha1.ResourceRequirements{
				CPULimit:    "0.5",
				MemoryLimit: "256m",
			},
		},
	}

	mockDocker := &common.MockDockerClient{}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()

	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: k8stypes.NamespacedName{
			Name:      cr.Name,
			Namespace: cr.Namespace,
		},
	})
	assert.NoError(t, err)

	// Verify container was created
	assert.Contains(t, mockDocker.Created, "resource-limited-app")
}
