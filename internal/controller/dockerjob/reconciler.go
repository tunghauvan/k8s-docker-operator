package dockerjob

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	dockerclient "github.com/docker/docker/client"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appv1alpha1 "github.com/tunghauvan/k8s-docker-operator/api/v1alpha1"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/common"
)

const finalizerName = "dockerjob.app.example.com/finalizer"

// DockerJobReconciler reconciles a DockerJob object
type DockerJobReconciler struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	DockerClient  dockerclient.APIClient // for testing
	dockerClients sync.Map               // cache: DockerHost name → *cachedClient
}

type cachedClient struct {
	client    dockerclient.APIClient
	createdAt time.Time
}

//+kubebuilder:rbac:groups=app.example.com,resources=dockerjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=app.example.com,resources=dockerjobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=app.example.com,resources=dockerjobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=app.example.com,resources=dockerhosts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile handles the reconciliation loop for DockerJob resources.
func (r *DockerJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// 1. Fetch the DockerJob instance
	job := &appv1alpha1.DockerJob{}
	if err := r.Get(ctx, req.NamespacedName, job); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Resolve Docker Client
	cli, err := r.getDockerClient(ctx, job)
	if err != nil {
		l.Error(err, "Failed to create docker client")
		return ctrl.Result{}, err
	}

	// 3. Finalizer Logic (cleanup container on deletion)
	if job.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(job, finalizerName) {
			controllerutil.AddFinalizer(job, finalizerName)
			if err := r.Update(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(job, finalizerName) {
			containerName := r.containerName(job)
			if err := cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true}); err != nil && !dockerclient.IsErrNotFound(err) {
				l.Error(err, "Failed to remove job container on deletion", "Container", containerName)
			}
			controllerutil.RemoveFinalizer(job, finalizerName)
			if err := r.Update(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 4. If job already completed, handle TTL cleanup only
	if job.Status.Phase == appv1alpha1.JobPhaseSucceeded || job.Status.Phase == appv1alpha1.JobPhaseFailed {
		return r.handleTTLCleanup(ctx, cli, job)
	}

	// 5. Sync the job container
	return r.syncJob(ctx, cli, job)
}

// containerName determines the Docker container name for this job.
func (r *DockerJobReconciler) containerName(job *appv1alpha1.DockerJob) string {
	if job.Spec.ContainerName != "" {
		return job.Spec.ContainerName
	}
	return "job-" + job.Name
}

// syncJob manages the lifecycle of the job's Docker container.
func (r *DockerJobReconciler) syncJob(ctx context.Context, cli dockerclient.APIClient, job *appv1alpha1.DockerJob) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	containerName := r.containerName(job)

	// Check if container exists
	inspected, err := cli.ContainerInspect(ctx, containerName)
	if err != nil {
		if !dockerclient.IsErrNotFound(err) {
			return ctrl.Result{}, err
		}
		// Container not found → create and start
		l.Info("Job container not found, creating...", "Container", containerName)
		if err := r.createAndStartContainer(ctx, cli, job); err != nil {
			l.Error(err, "Failed to create job container")
			r.setStatus(ctx, job, appv1alpha1.JobPhaseFailed, nil, fmt.Sprintf("Failed to create container: %v", err))
			return ctrl.Result{}, err
		}
		now := metav1.Now()
		job.Status.Phase = appv1alpha1.JobPhaseRunning
		job.Status.StartTime = &now
		job.Status.Attempts++
		job.Status.Message = "Container started"
		if err := r.Status().Update(ctx, job); err != nil {
			l.Error(err, "Failed to update status to Running")
		}
		// Requeue to check completion
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Container exists — check state
	if inspected.State == nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	switch {
	case inspected.State.Running:
		// Still running — check for timeout
		if job.Spec.ActiveDeadlineSeconds != nil && job.Status.StartTime != nil {
			deadline := job.Status.StartTime.Add(time.Duration(*job.Spec.ActiveDeadlineSeconds) * time.Second)
			if time.Now().After(deadline) {
				l.Info("Job exceeded active deadline, terminating", "Container", containerName)
				timeout := 10
				_ = cli.ContainerStop(ctx, inspected.ID, container.StopOptions{Timeout: &timeout})
				_ = cli.ContainerRemove(ctx, inspected.ID, container.RemoveOptions{Force: true})
				r.setStatus(ctx, job, appv1alpha1.JobPhaseFailed, nil, "Job exceeded ActiveDeadlineSeconds")
				return ctrl.Result{}, nil
			}
		}
		// Update status to running
		if job.Status.Phase != appv1alpha1.JobPhaseRunning {
			job.Status.Phase = appv1alpha1.JobPhaseRunning
			job.Status.ContainerID = inspected.ID
			job.Status.Message = "Container running"
			_ = r.Status().Update(ctx, job)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil

	case inspected.State.Status == "exited":
		exitCode := int32(inspected.State.ExitCode)
		now := metav1.Now()
		job.Status.ContainerID = inspected.ID
		job.Status.ExitCode = &exitCode
		job.Status.CompletionTime = &now

		if exitCode == 0 {
			// Success
			l.Info("Job completed successfully", "Container", containerName)
			job.Status.Phase = appv1alpha1.JobPhaseSucceeded
			job.Status.Message = "Job completed successfully"
			_ = r.Status().Update(ctx, job)
			return r.handleTTLCleanup(ctx, cli, job)
		}

		// Non-zero exit
		l.Info("Job container exited with error", "Container", containerName, "ExitCode", exitCode)

		// Check backoff/retry
		if job.Status.Attempts <= job.Spec.BackoffLimit {
			l.Info("Retrying job", "Attempt", job.Status.Attempts+1, "BackoffLimit", job.Spec.BackoffLimit)
			// Remove old container and retry
			_ = cli.ContainerRemove(ctx, inspected.ID, container.RemoveOptions{Force: true})
			if err := r.createAndStartContainer(ctx, cli, job); err != nil {
				r.setStatus(ctx, job, appv1alpha1.JobPhaseFailed, &exitCode, fmt.Sprintf("Retry failed: %v", err))
				return ctrl.Result{}, err
			}
			job.Status.Attempts++
			job.Status.Phase = appv1alpha1.JobPhaseRunning
			job.Status.Message = fmt.Sprintf("Retrying (attempt %d/%d)", job.Status.Attempts, job.Spec.BackoffLimit+1)
			job.Status.ExitCode = nil
			job.Status.CompletionTime = nil
			_ = r.Status().Update(ctx, job)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// No more retries
		job.Status.Phase = appv1alpha1.JobPhaseFailed
		job.Status.Message = fmt.Sprintf("Job failed with exit code %d after %d attempt(s)", exitCode, job.Status.Attempts)
		_ = r.Status().Update(ctx, job)
		return r.handleTTLCleanup(ctx, cli, job)

	default:
		// Other states (created, paused, etc.) — requeue
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

// handleTTLCleanup removes the Docker container after TTL expires.
func (r *DockerJobReconciler) handleTTLCleanup(ctx context.Context, cli dockerclient.APIClient, job *appv1alpha1.DockerJob) (ctrl.Result, error) {
	if job.Spec.TTLSecondsAfterFinished == nil || job.Status.CompletionTime == nil {
		return ctrl.Result{}, nil
	}

	ttl := time.Duration(*job.Spec.TTLSecondsAfterFinished) * time.Second
	elapsed := time.Since(job.Status.CompletionTime.Time)
	if elapsed < ttl {
		return ctrl.Result{RequeueAfter: ttl - elapsed}, nil
	}

	// TTL expired — remove container
	l := log.FromContext(ctx)
	containerName := r.containerName(job)
	if err := cli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true}); err != nil && !dockerclient.IsErrNotFound(err) {
		l.Error(err, "Failed to remove job container after TTL", "Container", containerName)
	} else {
		l.Info("Removed job container after TTL", "Container", containerName)
	}

	return ctrl.Result{}, nil
}

// setStatus is a helper to update job phase/status fields.
func (r *DockerJobReconciler) setStatus(ctx context.Context, job *appv1alpha1.DockerJob, phase appv1alpha1.DockerJobPhase, exitCode *int32, message string) {
	now := metav1.Now()
	job.Status.Phase = phase
	job.Status.Message = message
	if exitCode != nil {
		job.Status.ExitCode = exitCode
	}
	if phase == appv1alpha1.JobPhaseFailed || phase == appv1alpha1.JobPhaseSucceeded {
		job.Status.CompletionTime = &now
	}
	_ = r.Status().Update(ctx, job)
}

// createAndStartContainer pulls the image, creates, and starts the job container.
func (r *DockerJobReconciler) createAndStartContainer(ctx context.Context, cli dockerclient.APIClient, job *appv1alpha1.DockerJob) error {
	l := log.FromContext(ctx)
	containerName := r.containerName(job)

	// Pull options
	pullOpts := image.PullOptions{}
	if job.Spec.ImagePullSecret != "" {
		authConfig, err := r.getAuthConfig(ctx, job.Namespace, job.Spec.ImagePullSecret)
		if err != nil {
			l.Error(err, "Failed to get image pull secret")
			return err
		}
		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			return err
		}
		pullOpts.RegistryAuth = base64.URLEncoding.EncodeToString(encodedJSON)
	}

	// Pull image
	reader, err := cli.ImagePull(ctx, job.Spec.Image, pullOpts)
	if err != nil {
		l.Error(err, "Failed to pull image")
		return err
	}
	defer reader.Close()
	io.Copy(io.Discard, reader)

	// Resolve environment variables
	resolvedEnv := append([]string{}, job.Spec.Env...)
	if len(job.Spec.EnvVars) > 0 {
		for _, ev := range job.Spec.EnvVars {
			val := ev.Value
			if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
				secretVal, err := r.getSecretValue(ctx, job.Namespace, ev.ValueFrom.SecretKeyRef.Name, ev.ValueFrom.SecretKeyRef.Key)
				if err != nil {
					l.Error(err, "Failed to resolve secret env var", "Name", ev.Name)
					return err
				}
				val = secretVal
			}
			resolvedEnv = append(resolvedEnv, fmt.Sprintf("%s=%s", ev.Name, val))
		}
	}

	// Build container config
	cmd := job.Spec.Command
	if len(job.Spec.Args) > 0 {
		cmd = append(cmd, job.Spec.Args...)
	}

	config := &container.Config{
		Image: job.Spec.Image,
		Cmd:   cmd,
		Env:   resolvedEnv,
	}

	// Build host config
	restartPolicy := job.Spec.RestartPolicy
	if restartPolicy == "" || restartPolicy == "Never" {
		restartPolicy = "no"
	} else if restartPolicy == "OnFailure" {
		restartPolicy = "on-failure"
	}

	hostConfig := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(restartPolicy)},
		Binds:         []string{},
	}

	// Handle Resources
	if job.Spec.Resources != nil {
		hostConfig.Resources = buildDockerResources(job.Spec.Resources)
	}

	// Handle Volumes
	for _, v := range job.Spec.VolumeMounts {
		bind := fmt.Sprintf("%s:%s", v.HostPath, v.ContainerPath)
		if v.ReadOnly {
			bind += ":ro"
		}
		hostConfig.Binds = append(hostConfig.Binds, bind)
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, containerName)
	if err != nil {
		l.Error(err, "Failed to create job container")
		return err
	}

	// Handle Secret Volumes
	for _, sv := range job.Spec.SecretVolumes {
		if err := r.uploadSecretToContainer(ctx, cli, job.Namespace, sv, resp.ID); err != nil {
			l.Error(err, "Failed to upload secret volume", "Secret", sv.SecretName, "Path", sv.MountPath)
			return err
		}
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		l.Error(err, "Failed to start job container")
		return err
	}

	l.Info("Job container created and started", "ID", resp.ID, "Container", containerName)
	return nil
}

// buildDockerResources converts CRD ResourceRequirements to Docker container.Resources
func buildDockerResources(r *appv1alpha1.ResourceRequirements) container.Resources {
	if r == nil {
		return container.Resources{}
	}
	res := container.Resources{}
	if r.CPULimit != "" {
		var cpuFloat float64
		fmt.Sscanf(r.CPULimit, "%f", &cpuFloat)
		res.NanoCPUs = int64(cpuFloat * 1e9)
	}
	if r.MemoryLimit != "" {
		res.Memory = parseMemoryString(r.MemoryLimit)
	}
	return res
}

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
	var val int64
	fmt.Sscanf(s, "%d", &val)
	return val * multiplier
}

func (r *DockerJobReconciler) getSecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return "", err
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %s not found in secret %s", key, name)
	}
	return string(val), nil
}

func (r *DockerJobReconciler) uploadSecretToContainer(ctx context.Context, cli dockerclient.APIClient, namespace string, sv appv1alpha1.SecretVolume, containerID string) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: namespace, Name: sv.SecretName}, secret); err != nil {
		return err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
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

	return cli.CopyToContainer(ctx, containerID, "/", &buf, container.CopyToContainerOptions{})
}

func (r *DockerJobReconciler) getAuthConfig(ctx context.Context, namespace, secretName string) (registry.AuthConfig, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Namespace: namespace, Name: secretName}, secret); err != nil {
		return registry.AuthConfig{}, err
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	serverAddress := string(secret.Data["server"])
	if username == "" {
		return registry.AuthConfig{}, fmt.Errorf("username not found in secret")
	}
	return registry.AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: serverAddress,
	}, nil
}

// getDockerClient resolves the Docker Client to use, with caching for remote hosts.
func (r *DockerJobReconciler) getDockerClient(ctx context.Context, job *appv1alpha1.DockerJob) (dockerclient.APIClient, error) {
	// Case 1: Local (Default)
	if job.Spec.DockerHostRef == "" {
		if r.DockerClient != nil {
			return r.DockerClient, nil
		}
		return dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	}

	// Case 2: Remote DockerHost — check cache
	cacheKey := job.Namespace + "/" + job.Spec.DockerHostRef
	if cached, ok := r.dockerClients.Load(cacheKey); ok {
		cc := cached.(*cachedClient)
		if time.Since(cc.createdAt) < 5*time.Minute {
			return cc.client, nil
		}
		cc.client.Close()
		r.dockerClients.Delete(cacheKey)
	}

	host := &appv1alpha1.DockerHost{}
	hostKey := k8sclient.ObjectKey{
		Namespace: job.Namespace,
		Name:      job.Spec.DockerHostRef,
	}
	if err := r.Get(ctx, hostKey, host); err != nil {
		return nil, fmt.Errorf("failed to get DockerHost '%s': %w", job.Spec.DockerHostRef, err)
	}

	opts := []dockerclient.Opt{
		dockerclient.WithHost(host.Spec.HostURL),
		dockerclient.WithAPIVersionNegotiation(),
	}

	// Handle TLS
	if host.Spec.TLSSecretName != "" {
		secret := &corev1.Secret{}
		secretKey := k8sclient.ObjectKey{
			Namespace: job.Namespace,
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

	r.dockerClients.Store(cacheKey, &cachedClient{
		client:    newClient,
		createdAt: time.Now(),
	})

	return newClient, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1alpha1.DockerJob{}).
		Complete(r)
}
