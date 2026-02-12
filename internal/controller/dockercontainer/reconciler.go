package dockercontainer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	appv1alpha1 "github.com/tunghauvan/k8s-docker-operator/api/v1alpha1"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/common"
)

// DockerContainerReconciler reconciles a DockerContainer object
type DockerContainerReconciler struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	DockerClient  dockerclient.APIClient
	dockerClients sync.Map // cache: DockerHost name → *cachedClient
}

type cachedClient struct {
	client    dockerclient.APIClient
	createdAt time.Time
}

//+kubebuilder:rbac:groups=app.example.com,resources=dockercontainers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=app.example.com,resources=dockercontainers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=app.example.com,resources=dockercontainers/finalizers,verbs=update
//+kubebuilder:rbac:groups=app.example.com,resources=dockerhosts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
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

	// 5. Tunnel Logic --> Moved to DockerService controller
	// wsURL, err := r.reconcileTunnelServer(ctx, dockerContainer) ...
	// err = r.reconcileTunnelClient(ctx, cli, dockerContainer, wsURL) ...

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

	// Update Status (state + ID)
	statusChanged := false
	if cr.Status.ID != targetContainer.ID || cr.Status.State != targetContainer.State {
		cr.Status.ID = targetContainer.ID
		cr.Status.State = targetContainer.State
		statusChanged = true
	}

	// Health check status via inspect
	inspectResp, inspectErr := cli.ContainerInspect(ctx, targetContainer.ID)
	if inspectErr == nil && inspectResp.State != nil {
		health := "none"
		if inspectResp.State.Health != nil {
			health = string(inspectResp.State.Health.Status)
		}
		if cr.Status.Health != health {
			cr.Status.Health = health
			statusChanged = true
		}
	}

	if statusChanged {
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

	// Drift Detection: check if container config matches desired spec
	if inspectErr == nil {
		if needsRecreate(inspectResp, &cr.Spec) {
			l.Info("Container config drift detected, recreating...",
				"CurrentImage", targetContainer.Image,
				"ExpectedImage", cr.Spec.Image,
			)
			// Stop and remove
			timeout := 10
			stopTimeout := int(timeout)
			if err := cli.ContainerStop(ctx, targetContainer.ID, container.StopOptions{Timeout: &stopTimeout}); err != nil {
				l.Error(err, "Failed to stop container for update")
			}
			if err := cli.ContainerRemove(ctx, targetContainer.ID, container.RemoveOptions{Force: true}); err != nil {
				return err
			}
			return r.createAndStartContainer(ctx, cli, cr)
		}
	}

	return nil
}

// needsRecreate checks if the running container's config differs from the desired spec.
func needsRecreate(inspected types.ContainerJSON, spec *appv1alpha1.DockerContainerSpec) bool {
	if inspected.Config == nil {
		return false
	}

	// 1. Image mismatch
	if inspected.Config.Image != spec.Image {
		return true
	}

	// 2. Command mismatch
	if len(spec.Command) > 0 && !slices.Equal(inspected.Config.Cmd, spec.Command) {
		return true
	}

	// 3. Env mismatch (compare sorted, only spec-defined vars)
	if len(spec.Env) > 0 {
		specEnvSorted := make([]string, len(spec.Env))
		copy(specEnvSorted, spec.Env)
		sort.Strings(specEnvSorted)

		for _, expected := range specEnvSorted {
			found := false
			for _, actual := range inspected.Config.Env {
				if actual == expected {
					found = true
					break
				}
			}
			if !found {
				return true
			}
		}
	}

	// 4. RestartPolicy mismatch
	if spec.RestartPolicy != "" && inspected.HostConfig != nil {
		if string(inspected.HostConfig.RestartPolicy.Name) != spec.RestartPolicy {
			return true
		}
	}

	// 5. Resource limits mismatch
	if spec.Resources != nil && inspected.HostConfig != nil {
		desired := buildDockerResources(spec.Resources)
		if desired.NanoCPUs != inspected.HostConfig.Resources.NanoCPUs {
			return true
		}
		if desired.Memory != inspected.HostConfig.Resources.Memory {
			return true
		}
	}

	return false
}

// buildDockerHealthCheck converts our CRD HealthCheckConfig to Docker's container.HealthConfig
func buildDockerHealthCheck(hc *appv1alpha1.HealthCheckConfig) *container.HealthConfig {
	if hc == nil {
		return nil
	}

	config := &container.HealthConfig{
		Test: hc.Test,
	}

	if hc.Interval != "" {
		if d, err := time.ParseDuration(hc.Interval); err == nil {
			config.Interval = d
		}
	}
	if hc.Timeout != "" {
		if d, err := time.ParseDuration(hc.Timeout); err == nil {
			config.Timeout = d
		}
	}
	if hc.Retries > 0 {
		config.Retries = hc.Retries
	}

	return config
}

// buildDockerResources converts CRD ResourceRequirements to Docker container.Resources
func buildDockerResources(r *appv1alpha1.ResourceRequirements) container.Resources {
	if r == nil {
		return container.Resources{}
	}
	res := container.Resources{}
	if r.CPULimit != "" {
		cpuFloat, err := strconv.ParseFloat(r.CPULimit, 64)
		if err == nil {
			res.NanoCPUs = int64(cpuFloat * 1e9)
		}
	}
	if r.MemoryLimit != "" {
		res.Memory = parseMemoryString(r.MemoryLimit)
	}
	return res
}

// parseMemoryString parses memory strings like "256m", "1g", "512000" into bytes
func parseMemoryString(s string) int64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}

	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "k")
	}

	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return val * multiplier
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

	// Resolve EnvVars (literals and secrets)
	resolvedEnv := cr.Spec.Env
	if len(cr.Spec.EnvVars) > 0 {
		for _, ev := range cr.Spec.EnvVars {
			val := ev.Value
			if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
				secretVal, err := r.getSecretValue(ctx, cr.Namespace, ev.ValueFrom.SecretKeyRef.Name, ev.ValueFrom.SecretKeyRef.Key)
				if err != nil {
					l.Error(err, "Failed to resolve secret env var", "Name", ev.Name)
					return err
				}
				val = secretVal
			}
			resolvedEnv = append(resolvedEnv, fmt.Sprintf("%s=%s", ev.Name, val))
		}
	}

	// Configure Config
	config := &container.Config{
		Image:       cr.Spec.Image,
		Cmd:         cr.Spec.Command,
		Env:         resolvedEnv,
		Healthcheck: buildDockerHealthCheck(cr.Spec.HealthCheck),
	}

	// Configure HostConfig
	hostConfig := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(cr.Spec.RestartPolicy)},
		PortBindings:  nat.PortMap{},
		Binds:         []string{},
	}

	// Handle Resource Limits
	if cr.Spec.Resources != nil {
		hostConfig.Resources = buildDockerResources(cr.Spec.Resources)
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

	// Handle Secret Volumes via Copy Strategy (Upload files after creation but before start)
	if len(cr.Spec.SecretVolumes) > 0 {
		for _, sv := range cr.Spec.SecretVolumes {
			if err := r.uploadSecretToContainer(ctx, cli, cr.Namespace, sv, resp.ID); err != nil {
				l.Error(err, "Failed to upload secret volume", "Secret", sv.SecretName, "Path", sv.MountPath)
				// Should we proceed? Probably not, security credentials might be missing.
				return err
			}
		}
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		l.Error(err, "Failed to start container")
		return err
	}

	l.Info("Container created and started", "ID", resp.ID)
	return nil
}

func (r *DockerContainerReconciler) getSecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, k8sclient.ObjectKey{Namespace: namespace, Name: name}, secret)
	if err != nil {
		return "", err
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %s not found in secret %s", key, name)
	}
	return string(val), nil
}

func (r *DockerContainerReconciler) uploadSecretToContainer(ctx context.Context, cli dockerclient.APIClient, namespace string, sv appv1alpha1.SecretVolume, containerID string) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: namespace, Name: sv.SecretName}, secret); err != nil {
		return err
	}

	// Create tar stream
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Clean mount path and make it relative to root for tar entries
	relPath := strings.TrimPrefix(sv.MountPath, "/")

	for name, data := range secret.Data {
		hdr := &tar.Header{
			Name: filepath.Join(relPath, name),
			Mode: 0444,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}

	// Upload to container root
	return cli.CopyToContainer(ctx, containerID, "/", &buf, container.CopyToContainerOptions{})
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
	// Remove main container
	err := cli.ContainerRemove(ctx, cr.Spec.ContainerName, container.RemoveOptions{Force: true})
	if err != nil && !dockerclient.IsErrNotFound(err) {
		l.Error(err, "Failed to remove container")
		return err
	}
	l.Info("Container removed", "Name", cr.Spec.ContainerName)

	// Remove indexed tunnel client containers
	for i := range cr.Spec.Services {
		tunnelName := fmt.Sprintf("%s-tunnel-%d", cr.Spec.ContainerName, i)
		if err := cli.ContainerRemove(ctx, tunnelName, container.RemoveOptions{Force: true}); err != nil && !dockerclient.IsErrNotFound(err) {
			l.Error(err, "Failed to remove tunnel container", "Name", tunnelName)
		}
	}

	// Also try legacy single tunnel name for backward compatibility
	legacyTunnel := cr.Spec.ContainerName + "-tunnel"
	if err := cli.ContainerRemove(ctx, legacyTunnel, container.RemoveOptions{Force: true}); err != nil && !dockerclient.IsErrNotFound(err) {
		l.Error(err, "Failed to remove legacy tunnel", "Name", legacyTunnel)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerContainerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1alpha1.DockerContainer{}).
		Complete(r)
}

// getDockerClient resolves the Docker Client to use, with caching for remote hosts.
func (r *DockerContainerReconciler) getDockerClient(ctx context.Context, cr *appv1alpha1.DockerContainer) (dockerclient.APIClient, error) {
	// Case 1: Local (Default)
	if cr.Spec.DockerHostRef == "" {
		if r.DockerClient != nil {
			return r.DockerClient, nil
		}
		// Fallback to Env/Socket if nil
		return dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	}

	// Case 2: Remote DockerHost — check cache first
	cacheKey := cr.Namespace + "/" + cr.Spec.DockerHostRef
	if cached, ok := r.dockerClients.Load(cacheKey); ok {
		cc := cached.(*cachedClient)
		if time.Since(cc.createdAt) < 5*time.Minute {
			return cc.client, nil
		}
		// Expired — close and recreate
		cc.client.Close()
		r.dockerClients.Delete(cacheKey)
	}

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

	newClient, err := dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}

	// Store in cache
	r.dockerClients.Store(cacheKey, &cachedClient{
		client:    newClient,
		createdAt: time.Now(),
	})

	return newClient, nil
}

// generateToken creates a random hex token for tunnel authentication
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensureTunnelAuthSecret creates or retrieves the auth secret for tunnel authentication
func (r *DockerContainerReconciler) ensureTunnelAuthSecret(ctx context.Context, cr *appv1alpha1.DockerContainer) (string, error) {
	secretName := "tunnel-auth-" + cr.Name

	// Try to get existing secret
	existingSecret := &corev1.Secret{}
	err := r.Get(ctx, k8sclient.ObjectKey{Namespace: cr.Namespace, Name: secretName}, existingSecret)
	if err == nil {
		// Secret exists, return the token
		return string(existingSecret.Data["token"]), nil
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	// Generate new token
	token, err := generateToken()
	if err != nil {
		return "", fmt.Errorf("failed to generate tunnel token: %w", err)
	}

	// Create new secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cr.Namespace,
		},
		Data: map[string][]byte{
			"token": []byte(token),
		},
	}

	// Set owner reference so it's cleaned up with the CR
	if err := ctrl.SetControllerReference(cr, secret, r.Scheme); err != nil {
		return "", err
	}

	if err := r.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("failed to create tunnel auth secret: %w", err)
	}

	return token, nil
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

	// Get Main Container IP
	mainContainer, err := cli.ContainerInspect(ctx, cr.Spec.ContainerName)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			l.Info("Main container not ready yet for tunnel")
			return nil
		}
		return err
	}

	// Determine Target IP
	targetIP := ""
	for _, netConf := range mainContainer.NetworkSettings.Networks {
		targetIP = netConf.IPAddress
		break
	}
	if targetIP == "" {
		l.Info("Main container has no IP yet")
		return nil
	}

	// Pull Image (Operator Image) — only once for all ports
	img := os.Getenv("OPERATOR_IMAGE")
	if img == "" {
		img = "controller:latest"
	}
	reader, err := cli.ImagePull(ctx, img, image.PullOptions{})
	if err == nil {
		io.Copy(io.Discard, reader)
		reader.Close()
	} else {
		l.Info("Failed to pull tunnel image (might be local)", "error", err)
	}

	// Gateway Logic
	gatewayPort := 30000
	gatewayHost := "localhost"

	netMode := cr.Spec.NetworkMode
	if netMode == "kind" {
		nodes := &corev1.NodeList{}
		if err := r.List(ctx, nodes, client.MatchingLabels{"node-role.kubernetes.io/control-plane": ""}); err == nil && len(nodes.Items) > 0 {
			gatewayHost = nodes.Items[0].Name
		}
	}

	// Get auth token
	authToken, err := r.ensureTunnelAuthSecret(ctx, cr)
	if err != nil {
		l.Error(err, "Failed to ensure tunnel auth secret")
		return err
	}

	targetQuery := url.QueryEscape(wsURL)
	clientConnectURL := fmt.Sprintf("ws://%s:%d/ws?target=%s&token=%s", gatewayHost, gatewayPort, targetQuery, url.QueryEscape(authToken))

	// Create one tunnel client per service port
	for i, svc := range cr.Spec.Services {
		targetAddr := fmt.Sprintf("%s:%d", targetIP, svc.TargetPort)
		tunnelName := fmt.Sprintf("%s-tunnel-%d", cr.Spec.ContainerName, i)

		// Check if already running
		_, err = cli.ContainerInspect(ctx, tunnelName)
		if err == nil {
			continue // Already exists
		} else if !dockerclient.IsErrNotFound(err) {
			return err
		}

		config := &container.Config{
			Image: img,
			Cmd: []string{
				"tunnel",
				"-mode", "client",
				"-server-url", clientConnectURL,
				"-target-addr", targetAddr,
			},
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

		l.Info("Tunnel Client started", "ID", resp.ID, "Port", svc.TargetPort, "Index", i)
	}

	return nil
}

func (r *DockerContainerReconciler) reconcileTunnelServer(ctx context.Context, cr *appv1alpha1.DockerContainer) (string, error) {
	if len(cr.Spec.Services) == 0 {
		return "", nil
	}

	name := "tunnel-" + cr.Name
	labels := map[string]string{"app": name}

	// 1. Ensure auth token secret exists
	authToken, err := r.ensureTunnelAuthSecret(ctx, cr)
	if err != nil {
		return "", fmt.Errorf("failed to ensure tunnel auth: %w", err)
	}

	// 2. Deployment
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
								"-auth-token", authToken,
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
		// Update Deployment spec to ensure auth token is current
		foundDep.Spec.Template.Spec.Containers = dep.Spec.Template.Spec.Containers
		if err := r.Update(ctx, foundDep); err != nil {
			return "", err
		}
	}

	// 3. Service — map all service ports
	svcPorts := []corev1.ServicePort{
		{Name: "ws", Port: 8081, TargetPort: intstr.FromInt(8081)},
	}
	for i, sp := range cr.Spec.Services {
		portName := sp.Name
		if portName == "" {
			portName = fmt.Sprintf("traffic-%d", i)
		}
		svcPorts = append(svcPorts, corev1.ServicePort{
			Name:       portName,
			Port:       sp.Port,
			TargetPort: intstr.FromInt(8080),
		})
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Type:     corev1.ServiceTypeClusterIP,
			Ports:    svcPorts,
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

	// 4. Construct Tunnel Server URL (Internal ClusterDNS)
	// ws://<service-name>.<namespace>.svc:8081/ws
	tunnelServerURL := fmt.Sprintf("ws://%s.%s.svc:8081/ws", foundSvc.Name, foundSvc.Namespace)
	return tunnelServerURL, nil
}
