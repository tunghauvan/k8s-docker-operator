package dockercontainer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client" // k8s client
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"
	"k8s-docker-operator/internal/controller/common"
)

// DockerContainerReconciler reconciles a DockerContainer object
type DockerContainerReconciler struct {
	k8sclient.Client
	Scheme       *runtime.Scheme
	DockerClient dockerclient.APIClient
}

//+kubebuilder:rbac:groups=app.example.com,resources=dockercontainers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=app.example.com,resources=dockercontainers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=app.example.com,resources=dockercontainers/finalizers,verbs=update
//+kubebuilder:rbac:groups=app.example.com,resources=dockerhosts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

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

	// 5. Tunnel Logic
	wsURL, err := r.reconcileTunnelServer(ctx, dockerContainer)
	if err != nil {
		l.Error(err, "Failed to reconcile tunnel server")
		return ctrl.Result{}, err
	}

	err = r.reconcileTunnelClient(ctx, cli, dockerContainer, wsURL)
	if err != nil {
		l.Error(err, "Failed to reconcile tunnel client")
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

		tlsConfig, err := common.GetTLSConfig(secret)
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

func (r *DockerContainerReconciler) reconcileTunnelClient(ctx context.Context, cli dockerclient.APIClient, cr *appv1alpha1.DockerContainer, wsURL string) error {
	l := log.FromContext(ctx)
	if len(cr.Spec.Services) == 0 {
		return nil
	}
	if wsURL == "" {
		l.Info("Tunnel Server URL not ready yet")
		return nil // Wait for next reconcile
	}

	// Support single port for MVP
	svc := cr.Spec.Services[0]
	targetPort := svc.TargetPort

	// Get Main Container IP
	mainContainer, err := cli.ContainerInspect(ctx, cr.Spec.ContainerName)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			l.Info("Main container not ready yet for tunnel")
			return nil
		}
		return err
	}

	// Determine Target Address
	// If using default bridge, we can use IP.
	// We assume main container is running.
	targetIP := ""
	// Iterate networks (usually "bridge")
	for _, netConf := range mainContainer.NetworkSettings.Networks {
		targetIP = netConf.IPAddress
		break
	}
	if targetIP == "" {
		l.Info("Main container has no IP yet")
		return nil
	}
	targetAddr := fmt.Sprintf("%s:%d", targetIP, targetPort)

	// Tunnel Client Name
	tunnelName := cr.Spec.ContainerName + "-tunnel"

	// Check if already running
	_, err = cli.ContainerInspect(ctx, tunnelName)
	if err == nil {
		// Already exists. Should check config? For now assume OK if running.
		return nil
	} else if !dockerclient.IsErrNotFound(err) {
		return err
	}

	// Pull Image (Operator Image)
	// We assume the Docker Host can pull this image.
	img := os.Getenv("OPERATOR_IMAGE")
	if img == "" {
		img = "controller:latest"
	}
	// Pulling might fail if local image and not in registry.
	// Try pull, ignore error (might be local).
	reader, err := cli.ImagePull(ctx, img, image.PullOptions{})
	if err == nil {
		io.Copy(io.Discard, reader)
		reader.Close()
	} else {
		l.Info("Failed to pull tunnel image (might be local)", "error", err)
	}

	// Gateway Logic:
	gatewayPort := 30000
	gatewayHost := "localhost"

	netMode := cr.Spec.NetworkMode
	if netMode == "kind" {
		// Discover Kind Control Plane Node Name
		nodes := &corev1.NodeList{}
		if err := r.List(ctx, nodes, client.MatchingLabels{"node-role.kubernetes.io/control-plane": ""}); err == nil && len(nodes.Items) > 0 {
			gatewayHost = nodes.Items[0].Name
		}
	}

	// Target URL: ws://tunnel-nginx-basic.default.svc:8081/ws
	targetQuery := url.QueryEscape(wsURL)

	// Gateway Connection URL: ws://gateway:30000/ws?target=...
	clientConnectURL := fmt.Sprintf("ws://%s:%d/ws?target=%s", gatewayHost, gatewayPort, targetQuery)

	// Create Tunnel Client Container
	config := &container.Config{
		Image: img,
		Cmd: []string{
			"tunnel",
			"-mode", "client",
			"-server-url", clientConnectURL,
			"-target-addr", targetAddr,
		},
	}

	// Network Mode
	// netMode already defined above
	if netMode == "" {
		// Default behavior: empty is Bridge (Docker default).
		// Previously we hardcoded "host" for failed Kind fix.
		// Now we rely on user specifying "kind" or "host".
		// For backward compatibility with previous attempt, maybe we should stick with empty?
		// Empty is safer for standard Docker.
	}

	hostConfig := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: "always"},
		NetworkMode:   container.NetworkMode(netMode),
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, tunnelName)
	if err != nil {
		return err
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return err
	}

	l.Info("Tunnel Client started", "ID", resp.ID)
	return nil
}

func (r *DockerContainerReconciler) reconcileTunnelServer(ctx context.Context, cr *appv1alpha1.DockerContainer) (string, error) {
	if len(cr.Spec.Services) == 0 {
		return "", nil
	}

	name := "tunnel-" + cr.Name
	labels := map[string]string{"app": name}

	// 1. Deployment
	replicas := int32(1)
	img := os.Getenv("OPERATOR_IMAGE")
	if img == "" {
		img = "controller:latest"
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "tunnel",
							Image: img,
							Command: []string{
								"/manager", "tunnel",
								"-mode", "server",
								"-listen-addr", ":8080",
								"-ws-addr", ":8081",
							},
							Ports: []corev1.ContainerPort{
								{ContainerPort: 8080},
								{ContainerPort: 8081},
							},
						},
					},
				},
			},
		},
	}
	// Set owner ref
	if err := ctrl.SetControllerReference(cr, dep, r.Scheme); err != nil {
		return "", err
	}

	// Create or Update Deployment
	foundDep := &appsv1.Deployment{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: cr.Namespace, Name: name}, foundDep); err != nil {
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, dep); err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	} else {
		// Update logic
		// foundDep.Spec = dep.Spec // Simple update
		// r.Update(ctx, foundDep)
	}

	// 2. Service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Type:     corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{Name: "traffic", Port: 80, TargetPort: intstr.FromInt(8080)},
				{Name: "ws", Port: 8081, TargetPort: intstr.FromInt(8081)},
			},
		},
	}
	if err := ctrl.SetControllerReference(cr, svc, r.Scheme); err != nil {
		return "", err
	}

	foundSvc := &corev1.Service{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: cr.Namespace, Name: name}, foundSvc); err != nil {
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, svc); err != nil {
				return "", err
			}
			// Just created, might not have IP yet.
			return "", nil
		} else {
			return "", err
		}
	}

	// 3. Construct Tunnel Server URL (Internal ClusterDNS)
	// ws://<service-name>.<namespace>.svc:8081/ws
	tunnelServerURL := fmt.Sprintf("ws://%s.%s.svc:8081/ws", foundSvc.Name, foundSvc.Namespace)
	return tunnelServerURL, nil
}
