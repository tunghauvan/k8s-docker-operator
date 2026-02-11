package dockercontainer

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"
	"k8s-docker-operator/internal/controller/common"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	// Mock Docker Client
	mockDocker := &common.MockDockerClient{}

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

func TestDockerContainerReconciler_Reconcile_Tunnel(t *testing.T) {
	// Scheme
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	// Defines
	crName := "test-app-tunnel"
	crNamespace := "default"

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: crNamespace,
		},
		Spec: appv1alpha1.DockerContainerSpec{
			Image:         "nginx:latest",
			ContainerName: "my-web-app",
			Services: []appv1alpha1.ServicePort{
				{Port: 80, TargetPort: 80},
			},
		},
	}

	// Mock K8s Client
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(cr).Build()

	// Mock Docker Client
	mockDocker := &common.MockDockerClient{
		Containers: []dockertypes.Container{
			// Main container running
			{
				ID:    "main-id",
				Names: []string{"/my-web-app"},
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

	// Reconciler
	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	// Reconcile
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: crNamespace}}

	// Issue: reconcileTunnelServer checks for Service LB IP. Mock client won't update Status automatically.
	// So 1st Reconcile will create resources and return (wsURL empty).
	// We need to manually update Service Status in mock client for 2nd Reconcile.

	// 1st Run
	_, err := r.Reconcile(context.Background(), req)
	assert.NoError(t, err)

	// Verify Deployment Created
	dep := &appsv1.Deployment{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{Name: "tunnel-" + crName, Namespace: crNamespace}, dep)
	assert.NoError(t, err)

	// Verify Service Created
	svc := &corev1.Service{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{Name: "tunnel-" + crName, Namespace: crNamespace}, svc)
	assert.NoError(t, err)

	// Update Service Status to simulate LoadBalancer IP
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "1.2.3.4"}}
	err = k8sClient.Status().Update(context.Background(), svc)
	assert.NoError(t, err)

	// 2nd Run
	_, err = r.Reconcile(context.Background(), req)
	assert.NoError(t, err)

	// Verify Tunnel Container Created
	found := false
	for _, created := range mockDocker.Created {
		if created == "my-web-app-tunnel" {
			found = true
			break
		}
	}
	assert.True(t, found, "Tunnel client container should be created")
}

func TestDockerContainerReconciler_Reconcile_AuthAndVolumes(t *testing.T) {
	// Scheme
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))

	// Defines
	crName := "test-app"
	crNamespace := "default"
	secretName := "my-registry-secret"

	cr := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: crNamespace,
		},
		Spec: appv1alpha1.DockerContainerSpec{
			Image:           "private/image:latest",
			ContainerName:   "my-private-app",
			ImagePullSecret: secretName,
			VolumeMounts: []appv1alpha1.VolumeMount{
				{HostPath: "/tmp/host", ContainerPath: "/tmp/container", ReadOnly: true},
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: crNamespace,
		},
		Data: map[string][]byte{
			"username": []byte("user"),
			"password": []byte("pass"),
			"server":   []byte("https://index.docker.io/v1/"),
		},
	}

	// Fake K8s Client
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, secret).WithStatusSubresource(cr).Build()

	// Mock Docker Client
	mockDocker := &common.MockDockerClient{}

	r := &DockerContainerReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      crName,
			Namespace: crNamespace,
		},
	}

	// Run Reconcile
	ctx := context.TODO()
	_, err := r.Reconcile(ctx, req)
	assert.NoError(t, err)

	// Verify Auth
	assert.NotEmpty(t, mockDocker.LastPullOptions.RegistryAuth)
	// Base64 decode to verify content
	decoded, _ := base64.URLEncoding.DecodeString(mockDocker.LastPullOptions.RegistryAuth)
	assert.Contains(t, string(decoded), `"username":"user"`)

	// Verify Volumes
	require.NotNil(t, mockDocker.LastHostConfig)
	assert.Contains(t, mockDocker.LastHostConfig.Binds, "/tmp/host:/tmp/container:ro")
}
