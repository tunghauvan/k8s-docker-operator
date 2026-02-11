package controller

import (
	"context"
	"fmt"
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

	// 1. Fetch the DockerContainer instance
	dockerContainer := &appv1alpha1.DockerContainer{}
	err := r.Get(ctx, req.NamespacedName, dockerContainer)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Resolve Docker Client
	cli, err := r.getDockerClient(ctx, dockerContainer)
	if err != nil {
		l.Error(err, "Failed to create docker client")
		return ctrl.Result{}, err
	}
	// Close client if it's not the injected/cached one
	// Note: In real world, we might want to cache clients.
	// For now, if it's not the test client, we close it.
	if r.DockerClient == nil || (dockerContainer.Spec.DockerHostRef != "" && cli != r.DockerClient) {
		defer cli.Close()
	}

	// 3. Finalizer Logic
	if dockerContainer.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(dockerContainer, finalizerName) {
			controllerutil.AddFinalizer(dockerContainer, finalizerName)
			if err := r.Update(ctx, dockerContainer); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(dockerContainer, finalizerName) {
			if err := r.deleteExternalResources(ctx, cli, dockerContainer); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(dockerContainer, finalizerName)
			if err := r.Update(ctx, dockerContainer); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 4. Sync Logic
	err = r.syncContainer(ctx, cli, dockerContainer)
	if err != nil {
		l.Error(err, "Failed to sync container")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Minute * 1}, nil
}

func (r *DockerContainerReconciler) syncContainer(ctx context.Context, cli dockerclient.APIClient, cr *appv1alpha1.DockerContainer) error {
	l := log.FromContext(ctx)

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
		return r.createAndStartContainer(ctx, cli, cr)
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
		return r.createAndStartContainer(ctx, cli, cr)
	}

	return nil
}

func (r *DockerContainerReconciler) createAndStartContainer(ctx context.Context, cli dockerclient.APIClient, cr *appv1alpha1.DockerContainer) error {
	l := log.FromContext(ctx)

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

func (r *DockerContainerReconciler) deleteExternalResources(ctx context.Context, cli dockerclient.APIClient, cr *appv1alpha1.DockerContainer) error {
	l := log.FromContext(ctx)
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

// getDockerClient resolves the Docker Client to use.
func (r *DockerContainerReconciler) getDockerClient(ctx context.Context, cr *appv1alpha1.DockerContainer) (dockerclient.APIClient, error) {
	// Case 1: Local (Default)
	if cr.Spec.DockerHostRef == "" {
		if r.DockerClient != nil {
			return r.DockerClient, nil
		}
		// Fallback to Env/Socket if nil
		return dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	}

	// Case 2: Remote DockerHost
	host := &appv1alpha1.DockerHost{}
	hostKey := k8sclient.ObjectKey{
		Namespace: cr.Namespace,
		Name:      cr.Spec.DockerHostRef,
	}
	if err := r.Get(ctx, hostKey, host); err != nil {
		return nil, fmt.Errorf("failed to get DockerHost '%s': %w", cr.Spec.DockerHostRef, err)
	}

	// Create client for host
	opts := []dockerclient.Opt{
		dockerclient.WithHost(host.Spec.HostURL),
		dockerclient.WithAPIVersionNegotiation(),
	}

	// TODO: Handle TLS using host.Spec.TLSSecretName

	return dockerclient.NewClientWithOpts(opts...)
}
