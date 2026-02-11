package controller

import (
	"context"
	"testing"
	"time"

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"

	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDockerHostReconciler_Reconcile(t *testing.T) {
	// Scheme
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	// Defines
	hostName := "test-host"
	hostNamespace := "default"
	hostURL := "tcp://1.2.3.4:2376"

	host := &appv1alpha1.DockerHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hostName,
			Namespace: hostNamespace,
		},
		Spec: appv1alpha1.DockerHostSpec{
			HostURL: hostURL,
		},
	}

	// Fake K8s Client
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(host).WithStatusSubresource(host).Build()

	// Mock Docker Client Factory
	mockClientBuilder := func(opts ...client.Opt) (client.APIClient, error) {
		return &MockDockerClient{}, nil
	}

	// Reconciler
	r := &DockerHostReconciler{
		Client:        k8sClient,
		Scheme:        scheme,
		ClientBuilder: mockClientBuilder,
	}

	// Request
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      hostName,
			Namespace: hostNamespace,
		},
	}

	// Test: Connection Success
	ctx := context.TODO()
	res, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, time.Second*30, res.RequeueAfter)

	// Verify Status Updated
	updatedHost := &appv1alpha1.DockerHost{}
	err = k8sClient.Get(ctx, req.NamespacedName, updatedHost)
	assert.NoError(t, err)
	assert.Equal(t, "Connected", updatedHost.Status.Phase)
	assert.Equal(t, "Successfully connected to Docker Daemon", updatedHost.Status.Message)
}
