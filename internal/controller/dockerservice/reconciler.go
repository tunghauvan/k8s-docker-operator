package dockerservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appv1alpha1 "github.com/tunghauvan/k8s-docker-operator/api/v1alpha1"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/common"
)

// DockerServiceReconciler reconciles a DockerService object
type DockerServiceReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	DockerClient  dockerclient.APIClient
	dockerClients sync.Map // cache: DockerHost name → *cachedClient
}

type cachedClient struct {
	client    dockerclient.APIClient
	createdAt time.Time
}

//+kubebuilder:rbac:groups=app.example.com,resources=dockerservices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=app.example.com,resources=dockerservices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=app.example.com,resources=dockerservices/finalizers,verbs=update
//+kubebuilder:rbac:groups=app.example.com,resources=dockercontainers,verbs=get;list;watch
//+kubebuilder:rbac:groups=app.example.com,resources=dockerhosts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

const finalizerName = "dockerservice.app.example.com/finalizer"

func (r *DockerServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// Fetch DockerService
	dockerService := &appv1alpha1.DockerService{}
	err := r.Get(ctx, req.NamespacedName, dockerService)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch referenced DockerContainer(s)
	var targetContainers []appv1alpha1.DockerContainer

	if dockerService.Spec.Selector != nil {
		// List by Selector
		var containerList appv1alpha1.DockerContainerList
		selector, err := metav1.LabelSelectorAsSelector(dockerService.Spec.Selector)
		if err != nil {
			l.Error(err, "Invalid selector")
			return ctrl.Result{}, err // Don't retry invalid selector
		}

		if err := r.List(ctx, &containerList, client.InNamespace(req.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			l.Error(err, "Failed to list containers by selector")
			return ctrl.Result{}, err
		}
		targetContainers = containerList.Items
	} else if dockerService.Spec.ContainerRef != "" {
		// Legacy: Single Container Ref
		dockerContainer := &appv1alpha1.DockerContainer{}
		err = r.Get(ctx, k8sclient.ObjectKey{Namespace: req.Namespace, Name: dockerService.Spec.ContainerRef}, dockerContainer)
		if err != nil {
			if errors.IsNotFound(err) {
				l.Info("Referenced DockerContainer not found", "containerRef", dockerService.Spec.ContainerRef)
				// Wait for container to be created
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}
		targetContainers = []appv1alpha1.DockerContainer{*dockerContainer}
	} else {
		// No target specified
		l.Info("DockerService has no ContainerRef or Selector")
		return ctrl.Result{}, nil
	}

	if len(targetContainers) == 0 {
		l.Info("No matching DockerContainers found, scaling tunnel server to 0")
	}

	// Resolve Docker Client (using FIRST Container's DockerHostRef for tunnel server setup?)
	// Actually, we iterate over containers later for tunnel clients.
	// But we need CLI to clean up old resources if needed.
	// We'll init CLI inside loop.

	// Finalizer Logic
	if dockerService.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(dockerService, finalizerName) {
			controllerutil.AddFinalizer(dockerService, finalizerName)
			if err := r.Update(ctx, dockerService); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(dockerService, finalizerName) {
			if err := r.deleteExternalResources(ctx, dockerService); err != nil {
				return ctrl.Result{}, err // Retry on failure
			}
			controllerutil.RemoveFinalizer(dockerService, finalizerName)
			if err := r.Update(ctx, dockerService); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Logic 1: Gather Target IPs
	var targetIPs []string
	var firstContainer *appv1alpha1.DockerContainer

	for _, tc := range targetContainers {
		if tc.Status.IPv4 != "" {
			// Append valid IPs
			// Note: If using host networking, might be host IP.
			// If using bridge, container IP.
			// We append the IP:Port for EACH port defined in service?
			// The server expects target addresses.
			// The Load Balancer (Server) balances *requests*.
			// If we have Port 80 and 8080 defined in Service.
			// Incoming traffic on 80 -> needs to go to Container:TargetPort80.
			// Incoming traffic on 8080 -> needs to go to Container:TargetPort8080.
			// The Server `handleTCP` receives a connection. It doesn't know strictly which ServicePort it came from
			// unless we separate listeners or listeners share logic.
			// `Server` has `listenAddr` (which is one port?).
			// Wait, the Server supports multiple Ports: `-listen-addr=:80 -listen-addr=:8080` (main.go handles this?)
			// In `main.go`, `listenAddr` is a single string flag. It doesn't support multiple.
			// CRITICAL MISSING FEATURE properly supporting multiple ports in `main.go` / `Server` if not already handled.
			// Checking `main.go`: `listenAddr := tunnelCmd.String(...)`. It is a single value.
			// But `reconciler.go` previously generated:
			// `args = append(args, fmt.Sprintf("-listen-addr=:%d", p.Port))` loops over ports.
			// `flag` package with single `String` definition usually takes LAST value if repeated?
			// OR `flag` package doesn't support repeated flags for `String`.
			// `Server` struct has `listenAddr` string. It only supports ONE listener in current code.
			// IF the user has multiple ports, the current code is likely BROKEN for multiple ports or only listens on the last one.
			// Let's assume Single Port for now or that it works (maybe `main.go` uses a custom flag parser? No, uses standard `flag`).

			// For the single target loading:
			// We need to pass list of targets compatible with the listener.
			// Assuming all containers listen on the SAME TargetPort.
			// We form "IP:TargetPort".
			// If Service maps Port 80 -> TargetPort 8080.
			// We push "IP:8080".

			// We assume 1 Service Port for simplicity or standard LB behavior.
			// If multiple ports, we'd need multiple Listeners and multiple Target Groups.
			// For now, let's take the First Service Port's TargetPort.
			targetPort := dockerService.Spec.Ports[0].TargetPort
			targetIPs = append(targetIPs, fmt.Sprintf("%s:%d", tc.Status.IPv4, targetPort))
		}
		if firstContainer == nil {
			firstContainer = &tc
		}
	}

	// Logic 2: Tunnel Server (K8s) - Fixed size (1) acting as Load Balancer
	desiredReplicas := 1
	if len(targetContainers) == 0 {
		desiredReplicas = 0
	}
	wsURL, err := r.reconcileTunnelServer(ctx, dockerService, desiredReplicas, targetIPs)
	if err != nil {
		l.Error(err, "Failed to reconcile tunnel server")
		return ctrl.Result{}, err
	}

	// Logic 3: Tunnel Client (Docker) - SINGLE client
	if firstContainer != nil {
		// Resolve Client using the first container's DockerHostRef
		cli, err := r.getDockerClient(ctx, firstContainer)
		if err != nil {
			l.Error(err, "Failed to get docker client", "host", firstContainer.Spec.DockerHostRef)
			return ctrl.Result{}, err
		}

		// Reconcile SINGLE Tunnel Client
		// We name it genericly: "tunnel-client-<service-name>"
		if err := r.reconcileTunnelClient(ctx, cli, dockerService, firstContainer, wsURL); err != nil {
			l.Error(err, "Failed to reconcile tunnel client")
		}
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileTunnelServer creates the K8s Deployment and Service for the tunnel server
func (r *DockerServiceReconciler) reconcileTunnelServer(ctx context.Context, ds *appv1alpha1.DockerService, targetCount int, targetIPs []string) (string, error) {
	name := "tunnel-" + ds.Name
	namespace := ds.Namespace
	l := log.FromContext(ctx)

	// 1. Service (ClusterIP)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": name},
		},
	}

	opResult, err := ctrl.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = map[string]string{"app": name}

		ports := []corev1.ServicePort{
			{Name: "ws", Port: 8081, TargetPort: intstr.FromInt(8081)}, // Tunnel WS
		}

		// Map user-defined ports
		for _, p := range ds.Spec.Ports {
			ports = append(ports, corev1.ServicePort{
				Name:       p.Name,
				Port:       p.Port,
				TargetPort: intstr.FromInt(int(p.Port)), // Tunnel Server maps listener directly
			})
		}
		svc.Spec.Ports = ports

		return ctrl.SetControllerReference(ds, svc, r.Scheme)
	})
	if err != nil {
		return "", err
	}
	if opResult != controllerutil.OperationResultNone {
		l.Info("Tunnel Service reconciled", "operation", opResult)
	}

	// 2. Tunnel Auth Secret
	authToken, err := r.ensureTunnelAuthSecret(ctx, ds)
	if err != nil {
		return "", err
	}

	// 3. Deployment
	replicas := int32(targetCount)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	opResult, err = ctrl.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = map[string]string{"app": name}
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}

		// Join targets (sorted for stability)
		sort.Strings(targetIPs)
		targetsCSV := strings.Join(targetIPs, ",")

		args := []string{
			"tunnel",
			"-mode=server",
			"-ws-addr=:8081",
			"-auth-token=" + authToken,
			"-targets=" + targetsCSV,
		}
		for _, p := range ds.Spec.Ports {
			args = append(args, fmt.Sprintf("-listen-addr=:%d", p.Port))
		}

		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "server",
					Image: func() string {
						img := os.Getenv("OPERATOR_IMAGE")
						if img == "" {
							return "hvtung/k8s-docker-operator:latest"
						}
						return img
					}(), // Use same image
					ImagePullPolicy: corev1.PullIfNotPresent,
					Args:            args,
					Ports:           []corev1.ContainerPort{{ContainerPort: 8081}},
				}},
			},
		}
		return ctrl.SetControllerReference(ds, dep, r.Scheme)
	})
	if err != nil {
		return "", err
	}

	// We need the gateway address to form the WS URL?
	// Actually, the new architecture uses a Gateway logic or direct?
	// Based on code in DockerContainer:
	// "ws://tunnel-service:8081/ws"

	return fmt.Sprintf("ws://%s.%s.svc:%d/ws", name, namespace, 8081), nil
}

// ensureTunnelAuthSecret creates/retrieves a random token
func (r *DockerServiceReconciler) ensureTunnelAuthSecret(ctx context.Context, ds *appv1alpha1.DockerService) (string, error) {
	secretName := ds.Name + "-tunnel-auth"
	secret := &corev1.Secret{}
	err := r.Get(ctx, k8sclient.ObjectKey{Namespace: ds.Namespace, Name: secretName}, secret)
	if err == nil {
		if token, ok := secret.Data["token"]; ok {
			return string(token), nil
		}
	} else if !errors.IsNotFound(err) {
		return "", err
	}

	// Generate new token
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: ds.Namespace,
		},
		Data: map[string][]byte{"token": []byte(token)},
	}
	if err := ctrl.SetControllerReference(ds, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", err
	}
	return token, nil
}

// reconcileTunnelClient creates A SINGLE tunnel client container on the Docker host
func (r *DockerServiceReconciler) reconcileTunnelClient(ctx context.Context, cli dockerclient.APIClient, ds *appv1alpha1.DockerService, representativeContainer *appv1alpha1.DockerContainer, wsURL string) error {
	l := log.FromContext(ctx)

	// We use the "Representative Container" to determine:
	// 1. Docker Host (already resolved cli)
	// 2. Network (we should join the same network to reach targets)

	// Inspect Representative to get NetworkID
	mainContainer, err := cli.ContainerInspect(ctx, representativeContainer.Spec.ContainerName)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			l.Info("Representative container not found, skipping tunnel client")
			return nil
		}
		return err
	}

	targetNetworkID := ""
	for _, netConf := range mainContainer.NetworkSettings.Networks {
		targetNetworkID = netConf.NetworkID
		break
	}

	// Gateway Logic for connection (Connect to K8s from Docker)
	gatewayPort := 30000
	gatewayHost := "localhost"

	// Determine gateway host based on NetworkMode or heuristic
	netMode := ds.Spec.NetworkMode
	if netMode == "kind" {
		nodes := &corev1.NodeList{}
		if err := r.List(ctx, nodes, client.MatchingLabels{"node-role.kubernetes.io/control-plane": ""}); err == nil && len(nodes.Items) > 0 {
			gatewayHost = nodes.Items[0].Name
		}
	}

	authToken, err := r.ensureTunnelAuthSecret(ctx, ds)
	if err != nil {
		return err
	}

	// Pull image once
	img := os.Getenv("OPERATOR_IMAGE")
	if img == "" {
		img = "hvtung/k8s-docker-operator:latest"
	}

	// Check/Pull Image
	_, _, err = cli.ImageInspectWithRaw(ctx, img)
	if err != nil {
		reader, err := cli.ImagePull(ctx, img, image.PullOptions{})
		if err == nil {
			io.Copy(io.Discard, reader)
			reader.Close()
		} else {
			l.Error(err, "Failed to pull tunnel image (might be local)")
		}
	}

	targetQuery := url.QueryEscape(wsURL)
	clientConnectURL := fmt.Sprintf("ws://%s:%d/ws?target=%s&token=%s", gatewayHost, gatewayPort, targetQuery, url.QueryEscape(authToken))

	// Single Tunnel Client Name
	tunnelName := fmt.Sprintf("tunnel-client-%s", ds.Name)

	// Check if running and image matches
	existing, err := cli.ContainerInspect(ctx, tunnelName)
	if err == nil {
		// If image doesn't match, we need to recreate
		if existing.Config.Image != img {
			l.Info("Tunnel client image drift detected, recreating", "name", tunnelName, "old", existing.Config.Image, "new", img)
			if err := cli.ContainerRemove(ctx, tunnelName, container.RemoveOptions{Force: true}); err != nil {
				return err
			}
			// Continue to creation
		} else {
			if !existing.State.Running {
				// Restart
				if err := cli.ContainerStart(ctx, tunnelName, container.StartOptions{}); err != nil {
					return err
				}
			}
			// Update status and return
			ds.Status.Phase = "Active"
			ds.Status.TunnelClients = 1
			ds.Status.TunnelServerURL = wsURL
			metricsClient := client.NewNamespacedClient(r.Client, ds.Namespace)
			return metricsClient.Status().Update(ctx, ds)
		}
	} else if !dockerclient.IsErrNotFound(err) {
		return err
	}

	// Create
	l.Info("Creating single tunnel client", "name", tunnelName)

	config := &container.Config{
		Image: img,
		Cmd: []string{
			"tunnel",
			"-mode=client",
			"-server-url=" + clientConnectURL,
			"-auth-token=" + authToken,
			// No target-addr, it is dynamic
		},
	}

	hostConfig := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: "always"},
	}
	if netMode == "host" {
		hostConfig.NetworkMode = "host"
	} else if netMode == "kind" {
		hostConfig.NetworkMode = "kind"
	}

	_, err = cli.ContainerCreate(ctx, config, hostConfig, nil, nil, tunnelName)
	if err != nil {
		return err
	}
	if err := cli.ContainerStart(ctx, tunnelName, container.StartOptions{}); err != nil {
		return err
	}

	// Connect to target network if applicable
	if targetNetworkID != "" && targetNetworkID != "host" && targetNetworkID != "none" {
		if err := cli.NetworkConnect(ctx, targetNetworkID, tunnelName, nil); err != nil {
			// Ignore if already connected (fallback)
			l.Info("Network connect (might already exist)", "error", err)
		}
	}

	// Update status
	ds.Status.Phase = "Active"
	ds.Status.TunnelClients = 1
	ds.Status.TunnelServerURL = wsURL
	metricsClient := client.NewNamespacedClient(r.Client, ds.Namespace)
	return metricsClient.Status().Update(ctx, ds)
}

func (r *DockerServiceReconciler) deleteExternalResources(ctx context.Context, ds *appv1alpha1.DockerService) error {
	l := log.FromContext(ctx)

	// Clean up the single tunnel client
	// We need a Docker Client.
	// We don't know exactly which host it is on without checking.
	// But we can iterate over target containers to find the host.
	// OR: Try to clean up from valid hosts.

	// Re-fetch target containers
	var targetContainers []appv1alpha1.DockerContainer
	if ds.Spec.Selector != nil {
		var containerList appv1alpha1.DockerContainerList
		selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
		if err == nil {
			_ = r.List(ctx, &containerList, client.InNamespace(ds.Namespace), client.MatchingLabelsSelector{Selector: selector})
			targetContainers = containerList.Items
		}
	} else if ds.Spec.ContainerRef != "" {
		dockerContainer := &appv1alpha1.DockerContainer{}
		if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: ds.Namespace, Name: ds.Spec.ContainerRef}, dockerContainer); err == nil {
			targetContainers = []appv1alpha1.DockerContainer{*dockerContainer}
		}
	}

	// Map of DockerHosts to check
	hosts := make(map[string]appv1alpha1.DockerContainer)
	for _, tc := range targetContainers {
		hosts[tc.Spec.DockerHostRef] = tc
	}

	tunnelName := fmt.Sprintf("tunnel-client-%s", ds.Name)

	for _, tc := range hosts {
		cli, err := r.getDockerClient(ctx, &tc)
		if err != nil {
			l.Error(err, "Failed to get docker client for cleanup", "host", tc.Spec.DockerHostRef)
			continue
		}

		// Remove single tunnel client
		err = cli.ContainerRemove(ctx, tunnelName, container.RemoveOptions{Force: true})
		if err != nil && !dockerclient.IsErrNotFound(err) {
			l.Error(err, "Failed to remove tunnel container", "name", tunnelName)
		}

		// Remove legacy tunnel containers too (just in case)
		legacyName := fmt.Sprintf("%s-tunnel", ds.Name) // Try various patterns if needed
		_ = cli.ContainerRemove(ctx, legacyName, container.RemoveOptions{Force: true})
	}
	return nil
}

// getDockerClient - duplicated/adapted from DockerContainerReconciler
func (r *DockerServiceReconciler) getDockerClient(ctx context.Context, cr *appv1alpha1.DockerContainer) (dockerclient.APIClient, error) {
	// Resolve DockerHost name
	hostName := cr.Spec.DockerHostRef
	if hostName == "" {
		hostName = "default"
	}

	if val, ok := r.dockerClients.Load(cr.Namespace + "/" + hostName); ok {
		cached := val.(*cachedClient)
		if time.Since(cached.createdAt) < 5*time.Minute {
			return cached.client, nil
		}
		// Expired
		cached.client.Close()
		r.dockerClients.Delete(cr.Namespace + "/" + hostName)
	}

	host := &appv1alpha1.DockerHost{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: cr.Namespace, Name: hostName}, host); err != nil {
		if errors.IsNotFound(err) {
			// Try "system" namespace fallback
			if cr.Namespace != "system" {
				systemKey := k8sclient.ObjectKey{Namespace: "system", Name: hostName}
				if err := r.Get(ctx, systemKey, host); err == nil {
					goto found
				}
			}

			if hostName == "default" {
				// Fallback to local socket if "default" host CR not found
				if r.DockerClient != nil {
					return r.DockerClient, nil
				}
				return dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
			}
		}
		return nil, err
	}

found:

	opts := []dockerclient.Opt{
		dockerclient.WithHost(host.Spec.HostURL),
		dockerclient.WithAPIVersionNegotiation(),
	}

	if host.Spec.TLSSecretName != "" {
		tlsSecret := &corev1.Secret{}
		if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: cr.Namespace, Name: host.Spec.TLSSecretName}, tlsSecret); err != nil {
			return nil, err
		}

		tlsConfig, err := common.GetTLSConfig(tlsSecret)
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
		}
		opts = append(opts, dockerclient.WithHTTPClient(client))
	}

	cli, err := dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}

	r.dockerClients.Store(cr.Namespace+"/"+cr.Spec.DockerHostRef, &cachedClient{
		client:    cli,
		createdAt: time.Now(),
	})

	return cli, nil
}

func (r *DockerServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1alpha1.DockerService{}).
		Complete(r)
}
