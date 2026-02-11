package controller

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client" // k8s client
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"
)

// DockerContainerReconciler reconciles a DockerContainer object
type DockerContainerReconciler struct {
	k8sclient.Client
	Scheme       *runtime.Scheme
	DockerClient dockerclient.APIClient
}

const finalizerName = "dockercontainer.app.example.com/finalizer"

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *DockerContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// Fetch the DockerContainer instance
	dockerContainer := &appv1alpha1.DockerContainer{}
	err := r.Get(ctx, req.NamespacedName, dockerContainer)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Initialize Docker Client if not already done (lazy init for simplicity)
	if r.DockerClient == nil {
		cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
		if err != nil {
			l.Error(err, "Failed to create docker client")
			return ctrl.Result{}, err
		}
		r.DockerClient = cli
	}

	// Check if the instance is marked to be deleted
	if dockerContainer.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object.
		if !controllerutil.ContainsFinalizer(dockerContainer, finalizerName) {
			controllerutil.AddFinalizer(dockerContainer, finalizerName)
			if err := r.Update(ctx, dockerContainer); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(dockerContainer, finalizerName) {
			// our finalizer is present, so lets handle any external dependency
			if err := r.deleteExternalResources(ctx, dockerContainer); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(dockerContainer, finalizerName)
			if err := r.Update(ctx, dockerContainer); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// Sync logic
	err = r.syncContainer(ctx, dockerContainer)
	if err != nil {
		l.Error(err, "Failed to sync container")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Minute * 1}, nil
}

func (r *DockerContainerReconciler) syncContainer(ctx context.Context, cr *appv1alpha1.DockerContainer) error {
	l := log.FromContext(ctx)
	cli := r.DockerClient

	// Check if container exists
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}

	var targetContainer *types.Container
	// Match by name
	// Note: Docker API returns names with leading slash usually
	expectedName := "/" + cr.Spec.ContainerName
	for _, c := range containers {
		for _, name := range c.Names {
			if name == expectedName {
				targetContainer = &c
				break
			}
		}
		if targetContainer != nil {
			break
		}
	}

	if targetContainer == nil {
		l.Info("Container not found, creating...", "Name", cr.Spec.ContainerName)
		return r.createAndStartContainer(ctx, cr)
	}

	// Update Status
	if cr.Status.ID != targetContainer.ID || cr.Status.State != targetContainer.State {
		cr.Status.ID = targetContainer.ID
		cr.Status.State = targetContainer.State
		if err := r.Status().Update(ctx, cr); err != nil {
			l.Error(err, "Failed to update status")
		}
	}

	// Check if running
	if targetContainer.State != "running" {
		l.Info("Container exists but not running, starting...", "Name", cr.Spec.ContainerName)
		if err := cli.ContainerStart(ctx, targetContainer.ID, container.StartOptions{}); err != nil {
			return err
		}
	}

	// Determine if update is needed (simple check: Image)
	if targetContainer.Image != cr.Spec.Image {
		l.Info("Container image mismatch, recreating...", "Current", targetContainer.Image, "Expected", cr.Spec.Image)
		// Stop and remove
		timeout := 10 // seconds
		stopTimeout := int(timeout)
		if err := cli.ContainerStop(ctx, targetContainer.ID, container.StopOptions{Timeout: &stopTimeout}); err != nil {
			l.Error(err, "Failed to stop container for update")
		}
		if err := cli.ContainerRemove(ctx, targetContainer.ID, container.RemoveOptions{Force: true}); err != nil {
			return err
		}
		return r.createAndStartContainer(ctx, cr)
	}

	return nil
}

func (r *DockerContainerReconciler) createAndStartContainer(ctx context.Context, cr *appv1alpha1.DockerContainer) error {
	l := log.FromContext(ctx)
	cli := r.DockerClient

	// Pull image
	reader, err := cli.ImagePull(ctx, cr.Spec.Image, image.PullOptions{})
	if err != nil {
		l.Error(err, "Failed to pull image")
		return err
	}
	defer reader.Close()
	io.Copy(io.Discard, reader) // Wait for pull to finish

	// Configure Config
	config := &container.Config{
		Image: cr.Spec.Image,
		Cmd:   cr.Spec.Command,
		Env:   cr.Spec.Env,
	}

	// Configure HostConfig
	hostConfig := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(cr.Spec.RestartPolicy)},
		PortBindings:  nat.PortMap{},
	}

	// Port mapping logic
	if len(cr.Spec.Ports) > 0 {
		for _, p := range cr.Spec.Ports {
			// Format "8080:80" -> Host:Container
			parts := strings.Split(p, ":")
			if len(parts) == 2 {
				hostPort := parts[0]
				containerPort := parts[1]
				portKey, err := nat.NewPort("tcp", containerPort)
				if err != nil {
					l.Error(err, "Invalid port format")
					continue
				}
				hostConfig.PortBindings[portKey] = []nat.PortBinding{
					{HostIP: "0.0.0.0", HostPort: hostPort},
				}
				// Expose port in Config as well
				if config.ExposedPorts == nil {
					config.ExposedPorts = make(nat.PortSet)
				}
				config.ExposedPorts[portKey] = struct{}{}
			}
		}
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, cr.Spec.ContainerName)
	if err != nil {
		l.Error(err, "Failed to create container")
		return err
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		l.Error(err, "Failed to start container")
		return err
	}

	l.Info("Container created and started", "ID", resp.ID)
	return nil
}

func (r *DockerContainerReconciler) deleteExternalResources(ctx context.Context, cr *appv1alpha1.DockerContainer) error {
	l := log.FromContext(ctx)
	// Initialize if needed
	if r.DockerClient == nil {
		cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
		if err != nil {
			return err
		}
		r.DockerClient = cli
	}

	cli := r.DockerClient
	// Find container ID by name if not in status or just try by name
	// But ContainerRemove takes ID or Name.
	// Try by Name
	err := cli.ContainerRemove(ctx, cr.Spec.ContainerName, container.RemoveOptions{Force: true})
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			return nil // Already gone
		}
		l.Error(err, "Failed to remove container")
		return err
	}
	l.Info("Container removed", "Name", cr.Spec.ContainerName)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerContainerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1alpha1.DockerContainer{}).
		Complete(r)
}
