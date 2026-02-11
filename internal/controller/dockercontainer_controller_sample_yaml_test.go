package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Helper to load CR from YAML
func loadSampleCR(t *testing.T) *appv1alpha1.DockerContainer {
	yamlPath := filepath.Join("..", "..", "config", "samples", "app_v1alpha1_dockercontainer.yaml")
	yamlContent, err := os.ReadFile(yamlPath)
	require.NoError(t, err, "Failed to read sample YAML")

	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(appv1alpha1.AddToScheme(sch))

	decode := serializer.NewCodecFactory(sch).UniversalDeserializer().Decode
	obj, _, err := decode(yamlContent, nil, nil)
	require.NoError(t, err, "Failed to decode YAML")

	cr := obj.(*appv1alpha1.DockerContainer)
	cr.Namespace = "default"
	return cr
}

// TestIntegration_SampleYAML_01_Apply creates the container using the sample YAML.
// It DOES NOT cleanup, allowing manual verification.
func TestIntegration_SampleYAML_01_Apply(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cr := loadSampleCR(t)
	dockerContainerName := cr.Spec.ContainerName

	// Setup Reconciler
	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(appv1alpha1.AddToScheme(sch))

	k8sClient := fake.NewClientBuilder().WithScheme(sch).WithObjects(cr).WithStatusSubresource(cr).Build()

	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       sch,
		DockerClient: nil, // Real client
	}

	// Ensure cleanup of previous run just in case, but intended to leave running
	ctx := context.Background()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer cli.Close()

	// Remove if exists to ensure clean slate for Apply logic
	cli.ContainerRemove(ctx, dockerContainerName, container.RemoveOptions{Force: true})

	// Run Reconcile
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      cr.Name,
			Namespace: cr.Namespace,
		},
	}

	res, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, time.Minute*1, res.RequeueAfter)

	// Verify
	inspect, err := cli.ContainerInspect(ctx, dockerContainerName)
	require.NoError(t, err)
	require.NotNil(t, inspect.State)
	assert.Equal(t, "running", inspect.State.Status)

	t.Logf("Successfully created container: %s. It is left running for manual verification.", dockerContainerName)
}

// TestIntegration_SampleYAML_02_Cleanup removes the container created by Apply.
func TestIntegration_SampleYAML_02_Cleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cr := loadSampleCR(t)
	dockerContainerName := cr.Spec.ContainerName

	ctx := context.Background()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer cli.Close()

	err = cli.ContainerRemove(ctx, dockerContainerName, container.RemoveOptions{Force: true})
	require.NoError(t, err, "Failed to remove container")

	_, err = cli.ContainerInspect(ctx, dockerContainerName)
	assert.Error(t, err)
	assert.True(t, dockerclient.IsErrNotFound(err))

	t.Logf("Successfully cleaned up container: %s", dockerContainerName)
}
