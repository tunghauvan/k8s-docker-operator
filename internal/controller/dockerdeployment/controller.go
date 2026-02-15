package dockerdeployment

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"time"

	appv1alpha1 "github.com/tunghauvan/k8s-docker-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// DockerDeploymentReconciler reconciles a DockerDeployment object
type DockerDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=kdop.io.vn,resources=dockerdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kdop.io.vn,resources=dockerdeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kdop.io.vn,resources=dockerdeployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=kdop.io.vn,resources=dockercontainers,verbs=get;list;watch;create;update;patch;delete

func (r *DockerDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// 1. Fetch DockerDeployment
	deployment := &appv1alpha1.DockerDeployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Handling Deletion: skip reconciliation if being deleted
	if !deployment.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 3. Compute Desired Replicas
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}

	// 3. List Owned DockerContainers
	var childContainers appv1alpha1.DockerContainerList

	// List all DockerContainers in the namespace
	if err := r.List(ctx, &childContainers, client.InNamespace(deployment.Namespace)); err != nil {
		l.Error(err, "Failed to list child containers")
		return ctrl.Result{}, err
	}

	// Filter by OwnerReference
	var ownedContainers []appv1alpha1.DockerContainer
	for _, child := range childContainers.Items {
		if metav1.IsControlledBy(&child, deployment) {
			ownedContainers = append(ownedContainers, child)
		}
	}

	// Sort by creation timestamp (oldest first)
	sort.Slice(ownedContainers, func(i, j int) bool {
		return ownedContainers[i].CreationTimestamp.Before(&ownedContainers[j].CreationTimestamp)
	})

	currentCount := int32(len(ownedContainers))
	diff := replicas - currentCount

	// 4. Scale Up/Down
	if diff > 0 {
		// Scale Up
		l.Info("Scaling up", "current", currentCount, "desired", replicas, "adding", diff)
		for i := 0; i < int(diff); i++ {
			newContainer := r.constructContainer(deployment)
			if err := r.Create(ctx, newContainer); err != nil {
				l.Error(err, "Failed to create container", "name", newContainer.Name)
				return ctrl.Result{}, err
			}
		}
	} else if diff < 0 {
		// Scale Down (remove newest first)
		removeCount := int(-diff)
		l.Info("Scaling down", "current", currentCount, "desired", replicas, "removing", removeCount)
		for i := 0; i < removeCount; i++ {
			// Get the last one (newest)
			victim := ownedContainers[len(ownedContainers)-1-i]
			if err := r.Delete(ctx, &victim); err != nil {
				l.Error(err, "Failed to delete container", "name", victim.Name)
				return ctrl.Result{}, err
			}
		}
	}

	// 5. Update Status
	// Re-list to be accurate (optional, but good for status correctness after operations)
	// For now, we trust our calculation +/- success
	available := int32(0)
	// count running among specific known owned containers (ignoring the ones we just deleted/created for a simpler logic,
	// ideally should re-list but let's approximate based on what we see + what we did)
	// Actually, best is to just counting what's running in the snapshot providing we assume eventual consistency.
	// We will Requeue anyway.

	for _, c := range ownedContainers {
		if c.Status.State == "running" {
			available++
		}
	}
	// Note: this 'available' count is based on PRE-scaling state.
	// The next reconcile loop will update it correctly.

	if deployment.Status.AvailableReplicas != available {
		deployment.Status.AvailableReplicas = available
		if err := r.Status().Update(ctx, deployment); err != nil {
			l.Error(err, "Failed to update deployment status")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: time.Second * 10}, nil
}

func (r *DockerDeploymentReconciler) constructContainer(deploy *appv1alpha1.DockerDeployment) *appv1alpha1.DockerContainer {
	// Generate unique suffix
	suffix := utilRandString(5)
	name := fmt.Sprintf("%s-%s", deploy.Name, suffix)

	container := &appv1alpha1.DockerContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: deploy.Namespace,
			Labels:    deploy.Spec.Template.Metadata.Labels,
		},
		Spec: deploy.Spec.Template.Spec,
	}

	// Ensure Labels map exists
	if container.Labels == nil {
		container.Labels = make(map[string]string)
	}

	// Set OwnerRef
	if err := controllerutil.SetControllerReference(deploy, container, r.Scheme); err != nil {
		// This should not happen if scheme is correct
	}

	// Ensure containerName in Spec is unique
	if container.Spec.ContainerName == "" {
		container.Spec.ContainerName = name
	} else {
		// Append suffix to user-provided container name to avoid conflict on same host
		container.Spec.ContainerName = fmt.Sprintf("%s-%s", container.Spec.ContainerName, suffix)
	}

	return container
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz0123456789")

func utilRandString(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1alpha1.DockerDeployment{}).
		Owns(&appv1alpha1.DockerContainer{}).
		Complete(r)
}
