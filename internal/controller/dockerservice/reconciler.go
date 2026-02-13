package dockerservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
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

	// Logic 1: Tunnel Server (K8s) - Fixed size (1) acting as Load Balancer
	desiredReplicas := 1
	if len(targetContainers) == 0 {
		desiredReplicas = 0
	}
	wsURL, err := r.reconcileTunnelServer(ctx, dockerService, desiredReplicas)
	if err != nil {
		l.Error(err, "Failed to reconcile tunnel server")
		return ctrl.Result{}, err
	}

	// Logic 2: Tunnel Clients (Docker) - One for each target container
	for _, targetContainer := range targetContainers {
		// Resolve Client for THIS container
		cli, err := r.getDockerClient(ctx, &targetContainer)
		if err != nil {
			l.Error(err, "Failed to get docker client for container", "container", targetContainer.Name)
			continue
		}

		if err := r.reconcileTunnelClient(ctx, cli, dockerService, &targetContainer, wsURL); err != nil {
			l.Error(err, "Failed to reconcile tunnel client", "container", targetContainer.Name)
			// Should we return error or continue? Continue for partial success.
		}
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileTunnelServer creates the K8s Deployment and Service for the tunnel server
func (r *DockerServiceReconciler) reconcileTunnelServer(ctx context.Context, ds *appv1alpha1.DockerService, targetCount int) (string, error) {
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

		// Build Args
		args := []string{
			"tunnel",
			"-mode=server",
			"-ws-addr=:8081",
			"-auth-token=" + authToken,
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
					Args:  args,
					Ports: []corev1.ContainerPort{{ContainerPort: 8081}},
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

// reconcileTunnelClient creates tunnel client containers on the Docker host
func (r *DockerServiceReconciler) reconcileTunnelClient(ctx context.Context, cli dockerclient.APIClient, ds *appv1alpha1.DockerService, dc *appv1alpha1.DockerContainer, wsURL string) error {
	l := log.FromContext(ctx)

	// Get Main Container IP
	mainContainer, err := cli.ContainerInspect(ctx, dc.Spec.ContainerName)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			l.Info("Main container not ready yet for tunnel")
			return nil
		}
		return err
	}

	targetIP := ""
	targetNetworkID := ""
	for _, netConf := range mainContainer.NetworkSettings.Networks {
		targetIP = netConf.IPAddress
		targetNetworkID = netConf.NetworkID
		break
	}
	if targetIP == "" {
		l.Info("Main container has no IP yet")
		return nil
	}

	// Gateway Logic for connection
	// Similar to before, we need to connect to the K8s gateway from outside.
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
	// Pulling implementation skipped for brevity/simplicity, assume it works or handled elsewhere

	targetQuery := url.QueryEscape(wsURL)
	clientConnectURL := fmt.Sprintf("ws://%s:%d/ws?target=%s&token=%s", gatewayHost, gatewayPort, targetQuery, url.QueryEscape(authToken))

	// Create tunnel clients
	activeClients := 0
	for i, port := range ds.Spec.Ports {
		targetAddr := fmt.Sprintf("%s:%d", targetIP, port.TargetPort)
		// Use Container Name for uniqueness in Load Balanced mode
		tunnelName := fmt.Sprintf("%s-tunnel-%d", dc.Name, i)

		// Check if running
		_, err := cli.ContainerInspect(ctx, tunnelName)
		if err == nil {
			activeClients++
			continue
		} else if !dockerclient.IsErrNotFound(err) {
			return err
		}

		// Create
		l.Info("Creating tunnel client", "name", tunnelName, "target", targetAddr)

		config := &container.Config{
			Image: img,
			Cmd: []string{
				"tunnel",
				"-mode=client",
				"-server-url=" + clientConnectURL,
				"-target-addr=" + targetAddr,
				"-auth-token=" + authToken,
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
			// Try to connect. Ignore if already connected.
			if err := cli.NetworkConnect(ctx, targetNetworkID, tunnelName, nil); err != nil {
				// Current docker client might return error if already connected.
				// We can check error string or inspect first. Checking string is easier but fragile.
				// Let's inspect first since we might need to know.
				// Actually, just ignore specific error types?
				// "endpoint with name ... already exists in network ..."
				// "already connected to network"
				// For simplicity/robustness, let's log debug and ignore error for now, or check inspect.

				// Better: check inspect of tunnel container.
				cInspect, errInspect := cli.ContainerInspect(ctx, tunnelName)
				alreadyConnected := false
				if errInspect == nil {
					for _, n := range cInspect.NetworkSettings.Networks {
						if n.NetworkID == targetNetworkID {
							alreadyConnected = true
							break
						}
					}
				}

				if !alreadyConnected {
					// Real error
					l.Error(err, "Failed to connect tunnel to target network", "network", targetNetworkID)
					// Don't fail the loop? Warning.
				}
			}
		}

		activeClients++
	}

	// Update status
	ds.Status.Phase = "Active"
	ds.Status.TunnelClients = activeClients
	ds.Status.TunnelServerURL = wsURL
	metricsClient := client.NewNamespacedClient(r.Client, ds.Namespace)
	return metricsClient.Status().Update(ctx, ds)
}

func (r *DockerServiceReconciler) deleteExternalResources(ctx context.Context, ds *appv1alpha1.DockerService) error {
	l := log.FromContext(ctx)

	// We need to clean up tunnels for ALL potential containers.
	// Since we don't have the list here easily without querying, we might need to rely on
	// Docker naming convention or list all containers and filter?
	// OR: For now, we can just try to clean up known patterns if possible.
	// But `deleteExternalResources` is called on finalizer.
	// The best way is to list all DockerContainers that MATCH the selector (if it exists)
	// and clean up.

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

	for _, targetContainer := range targetContainers {
		cli, err := r.getDockerClient(ctx, &targetContainer)
		if err != nil {
			l.Error(err, "Failed to get docker client for cleanup", "container", targetContainer.Name)
			continue
		}

		for i := range ds.Spec.Ports {
			// Naming convention: <container-name>-tunnel-<port-index>
			tunnelName := fmt.Sprintf("%s-tunnel-%d", targetContainer.Name, i)

			// Also try legacy name for backward compatibility? "<service-name>-tunnel-..."
			// Only if containerRef was used?
			// For simplicity, let's just stick to new pattern or try both if needed.

			err := cli.ContainerRemove(ctx, tunnelName, container.RemoveOptions{Force: true})
			if err != nil && !dockerclient.IsErrNotFound(err) {
				l.Error(err, "Failed to remove tunnel container", "name", tunnelName)
			}
		}
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
