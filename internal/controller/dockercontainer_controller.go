package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	corev1 "k8s.io/api/core/v1"
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

	// Pull options
	pullOpts := image.PullOptions{}

	// Handle Registry Auth
	if cr.Spec.ImagePullSecret != "" {
		authConfig, err := r.getAuthConfig(ctx, cr.Namespace, cr.Spec.ImagePullSecret)
		if err != nil {
			l.Error(err, "Failed to get image pull secret")
			return err
		}
		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			return err
		}
		authStr := base64.URLEncoding.EncodeToString(encodedJSON)
		pullOpts.RegistryAuth = authStr
	}

	// Pull image
	reader, err := cli.ImagePull(ctx, cr.Spec.Image, pullOpts)
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
		Binds:         []string{},
	}

	// Handle Volumes
	if len(cr.Spec.VolumeMounts) > 0 {
		for _, v := range cr.Spec.VolumeMounts {
			bind := fmt.Sprintf("%s:%s", v.HostPath, v.ContainerPath)
			if v.ReadOnly {
				bind += ":ro"
			}
			hostConfig.Binds = append(hostConfig.Binds, bind)
		}
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

func (r *DockerContainerReconciler) getAuthConfig(ctx context.Context, namespace, secretName string) (registry.AuthConfig, error) {
	secret := &corev1.Secret{}
	key := k8sclient.ObjectKey{Namespace: namespace, Name: secretName}
	if err := r.Get(ctx, key, secret); err != nil {
		return registry.AuthConfig{}, err
	}

	// Docker secrets usually have .dockerconfigjson or separate fields
	// Handling simple username/password/server fields for now or standard docker opaque secret
	// Let's assume standard k8s docker-registry secret type first, which is .dockerconfigjson
	// But parsing that is complex. Let's support simple generic secret with username, password, server address first for simplicity.
	// Or standard keys: username, password, serveraddress, registry-token

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	serverAddress := string(secret.Data["server"])

	if username == "" {
		// Try docker-registry keys?
		// For now, let's Stick to generic secret structure: username, password, server
		return registry.AuthConfig{}, fmt.Errorf("username not found in secret")
	}

	return registry.AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: serverAddress,
	}, nil
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

	opts := []dockerclient.Opt{
		dockerclient.WithHost(host.Spec.HostURL),
		dockerclient.WithAPIVersionNegotiation(),
	}

	// Handle TLS
	if host.Spec.TLSSecretName != "" {
		secret := &corev1.Secret{}
		secretKey := k8sclient.ObjectKey{
			Namespace: cr.Namespace,
			Name:      host.Spec.TLSSecretName,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return nil, fmt.Errorf("failed to get TLS Secret '%s': %w", host.Spec.TLSSecretName, err)
		}

		tlsConfig, err := getTLSConfig(secret)
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS config: %w", err)
		}

		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
		}
		opts = append(opts, dockerclient.WithHTTPClient(httpClient))
	}

	return dockerclient.NewClientWithOpts(opts...)
}

func getTLSConfig(secret *corev1.Secret) (*tls.Config, error) {
	caCert, ok := secret.Data["ca.pem"]
	if !ok {
		return nil, fmt.Errorf("secret missing 'ca.pem'")
	}
	certPem, ok := secret.Data["cert.pem"]
	if !ok {
		return nil, fmt.Errorf("secret missing 'cert.pem'")
	}
	keyPem, ok := secret.Data["key.pem"]
	if !ok {
		return nil, fmt.Errorf("secret missing 'key.pem'")
	}

	// Load CA
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate")
	}

	// Load Client Cert/Key
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
		// InsecureSkipVerify: true, // Optional: make configurable via DockerHostSpec?
	}, nil
}
