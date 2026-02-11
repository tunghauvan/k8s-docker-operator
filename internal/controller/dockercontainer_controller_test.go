package controller

import (
	"context"
	"testing"
	"time"

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDockerContainerReconciler_Reconcile(t *testing.T) {
	// Scheme
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	// Defines
	crName := "test-container"
	crNamespace := "default"
	dockerImage := "nginx:latest"
	dockerContainerName := "my-test-nginx"

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: crNamespace,
		},
		Spec: appv1alpha1.DockerContainerSpec{
			Image:         dockerImage,
			ContainerName: dockerContainerName,
		},
	}

	// Fake K8s Client
	// Note: We need to register the object in the fake client
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()

	// Mock Docker Client
	mockDocker := &MockDockerClient{}

	// Reconciler
	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	// Request
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      crName,
			Namespace: crNamespace,
		},
	}

	// Test 1: Create Container
	ctx := context.TODO()
	res, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, time.Minute*1, res.RequeueAfter)

	// Test 2: Status Update (Second Pass)
	res, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)

	// Verify Docker Call
	assert.Contains(t, mockDocker.Created, dockerContainerName)
	assert.Contains(t, mockDocker.Started[0], "mock-id-"+dockerContainerName)

	// Verify Status Updated
	updatedCR := &appv1alpha1.DockerContainer{}
	err = k8sClient.Get(ctx, req.NamespacedName, updatedCR)
	assert.NoError(t, err)
	assert.Equal(t, "mock-id-"+dockerContainerName, updatedCR.Status.ID)
}
