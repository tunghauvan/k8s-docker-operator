package dockerservice

import (
	"context"
	"fmt"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

func TestDockerServiceReconciler_Reconcile(t *testing.T) {
	// Scheme
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appv1alpha1.AddToScheme(scheme))
	metav1.AddToGroupVersion(scheme, appv1alpha1.GroupVersion)

	fmt.Printf("Known types in %s: %v\n", appv1alpha1.GroupVersion, scheme.KnownTypes(appv1alpha1.GroupVersion))

	// Defines
	ns := "default"
	svcName := "my-app-svc"
	containerName := "my-app"

	ds := &appv1alpha1.DockerService{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kdop.io.vn/v1alpha1",
			Kind:       "DockerService",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: ns,
		},
		Spec: appv1alpha1.DockerServiceSpec{
			ContainerRef: containerName,
			Ports: []appv1alpha1.ServicePort{
				{Name: "http", Port: 80, TargetPort: 8080},
			},
		},
	}

	dc := &appv1alpha1.DockerContainer{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kdop.io.vn/v1alpha1",
			Kind:       "DockerContainer",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      containerName,
			Namespace: ns,
		},
		Spec: appv1alpha1.DockerContainerSpec{
			ContainerName: "my-app-container",
			Image:         "nginx:latest",
		},
	}

	// Mock Docker Client
	mockDocker := &common.MockDockerClient{
		Containers: []types.Container{
			{
				ID:    "main-container-id",
				Names: []string{"/my-app-container"},
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

	// Fake K8s Client
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	ctx := context.Background()
	_ = k8sClient.Create(ctx, dc)
	_ = k8sClient.Create(ctx, ds)

	r := &DockerServiceReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		DockerClient: mockDocker,
	}

	// 1st Reconcile

	// sanity check
	checkDC := &appv1alpha1.DockerContainer{}
	err := k8sClient.Get(ctx, k8stypes.NamespacedName{Name: containerName, Namespace: ns}, checkDC)
	assert.NoError(t, err, "Should be able to get DockerContainer")

	req := ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: svcName, Namespace: ns}}
	_, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)

	// Verify K8s resources created
	// 1. Service
	svc := &corev1.Service{}
	err = k8sClient.Get(context.Background(), k8stypes.NamespacedName{Name: "tunnel-" + svcName, Namespace: ns}, svc)
	assert.NoError(t, err)
	assert.Equal(t, int32(8081), svc.Spec.Ports[0].Port)
	assert.Equal(t, int32(80), svc.Spec.Ports[1].Port)

	// 2. Secret
	secret := &corev1.Secret{}
	err = k8sClient.Get(context.Background(), k8stypes.NamespacedName{Name: svcName + "-tunnel-auth", Namespace: ns}, secret)
	assert.NoError(t, err)
	assert.Contains(t, secret.Data, "token")

	// 3. Deployment
	dep := &appsv1.Deployment{}
	err = k8sClient.Get(context.Background(), k8stypes.NamespacedName{Name: "tunnel-" + svcName, Namespace: ns}, dep)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), *dep.Spec.Replicas)

	// Verify Tunnel Client created in Docker
	found := false
	tunnelName := fmt.Sprintf("tunnel-client-%s", svcName)
	for _, created := range mockDocker.Created {
		if created == tunnelName {
			found = true
			break
		}
	}
	assert.True(t, found, "Tunnel client container should be created")
}
