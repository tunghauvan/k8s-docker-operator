package dockercontainer

import (
	"context"
	"testing"

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
)

// TestIntegration_MultiHost verifying DockerHost support using Fake K8s Client + Real Docker Client
func TestIntegration_MultiHost(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// 1. Setup Scheme
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	// 2. Define Objects
	hostName := "local-docker-host"
	namespace := "default"
	containerName := "multihost-test-nginx"
	hostURL := "unix:///var/run/docker.sock" // Assumes local unix socket

	// DockerHost CR
	dockerHost := &appv1alpha1.DockerHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hostName,
			Namespace: namespace,
		},
		Spec: appv1alpha1.DockerHostSpec{
			HostURL: hostURL,
		},
	}

	// DockerContainer CR
	dockerCR := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nginx-multihost",
			Namespace: namespace,
		},
		Spec: appv1alpha1.DockerContainerSpec{
			Image:         "nginx:latest",
			ContainerName: containerName,
			DockerHostRef: hostName,
			Ports:         []string{"8083:80"},
		},
	}

	// 3. Fake Client with Objects
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dockerHost, dockerCR).WithStatusSubresource(dockerHost, dockerCR).Build()

	// 4. Reconciler (Client=nil to force lookup)
	reconciler := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: nil,
	}

	// 5. Cleanup Setup (Real Docker)
	ctx := context.Background()
	dc, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer dc.Close()

	// Ensure cleanup
	cleanup := func() {
		dc.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})
	}
	cleanup()
	defer cleanup()

	// 6. Run Reconcile
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nginx-multihost",
			Namespace: namespace,
		},
	}

	// Loop a few times to simulate controller manager (create, status update, check)
	// First Pass: Create
	_, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	// Verify Created
	inspect, err := dc.ContainerInspect(ctx, containerName)
	require.NoError(t, err)
	require.Equal(t, "running", inspect.State.Status)

	// Second Pass: Update Status
	_, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	// Verify Status
	currentCR := &appv1alpha1.DockerContainer{}
	err = k8sClient.Get(ctx, req.NamespacedName, currentCR)
	require.NoError(t, err)
	require.Equal(t, inspect.ID, currentCR.Status.ID)

	t.Logf("Multi-Host Test Passed: Container %s created via DockerHostRef %s", containerName, hostName)
}
