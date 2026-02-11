package controller

import (
	"context"
	"testing"
	"time"

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestIntegration_Reconcile_RealDocker runs with a REAL Docker socket.
// Requires Docker to be running locally.
func TestIntegration_Reconcile_RealDocker(t *testing.T) {
	// Skip if short mode (optional, but good practice)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// 1. Setup Scheme
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	// 2. Define CR
	crName := "integration-test-nginx"
	crNamespace := "default"
	dockerImage := "nginx:alpine" // Use alpine for speed
	dockerContainerName := "integration-test-nginx-container"

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: crNamespace,
		},
		Spec: appv1alpha1.DockerContainerSpec{
			Image:         dockerImage,
			ContainerName: dockerContainerName,
			Ports:         []string{"8888:80"}, // Map 8888 to 80
		},
	}

	// 3. Fake K8s Client
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()

	// 4. Reconciler with NIL DockerClient (forces real connection)
	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: nil, // This triggers NewClientWithOpts(FromEnv)
	}

	// 5. Cleanup function to remove container if exists
	ctx := context.Background()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer cli.Close()

	cleanup := func() {
		cli.ContainerRemove(ctx, dockerContainerName, container.RemoveOptions{Force: true})
	}
	cleanup()       // Pre-cleanup
	defer cleanup() // Post-cleanup

	// 6. Pull image first to ensure test doesn't timeout on pull
	// (Optional, but makes Reconcile faster and more deterministic)
	// reader, err := cli.ImagePull(ctx, dockerImage, types.ImagePullOptions{})
	// if err == nil {
	// 	io.Copy(io.Discard, reader)
	// 	reader.Close()
	// }

	// 7. Run Reconcile
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      crName,
			Namespace: crNamespace,
		},
	}

	// Act
	res, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, time.Minute*1, res.RequeueAfter)

	// 8. Verify Container Exists on Host
	inspect, err := cli.ContainerInspect(ctx, dockerContainerName)
	require.NoError(t, err)
	require.NotNil(t, inspect.State)
	assert.Equal(t, "running", inspect.State.Status)
	assert.Equal(t, "/"+dockerContainerName, inspect.Name)

	// 9. Verify CR Status Update (Run Reconcile again to update status)
	_, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)

	updatedCR := &appv1alpha1.DockerContainer{}
	err = k8sClient.Get(ctx, req.NamespacedName, updatedCR)
	assert.NoError(t, err)
	assert.NotEmpty(t, updatedCR.Status.ID)
	assert.Equal(t, inspect.ID, updatedCR.Status.ID)

	t.Logf("Successfully created and verified container %s with ID %s", dockerContainerName, updatedCR.Status.ID)
}
