package dockerhost

import (
	"context"
	"fmt"
	"net/http"
	"time"

	dockerclient "github.com/docker/docker/client"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appv1alpha1 "github.com/tunghauvan/k8s-docker-operator/api/v1alpha1"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/common"
)

// ClientBuilderFunc defines a function to create a Docker client
type ClientBuilderFunc func(opts ...dockerclient.Opt) (dockerclient.APIClient, error)

//+kubebuilder:rbac:groups=kdop.io.vn,resources=dockerhosts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kdop.io.vn,resources=dockerhosts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kdop.io.vn,resources=dockerhosts/finalizers,verbs=update

// DockerHostReconciler reconciles a DockerHost object
type DockerHostReconciler struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	ClientBuilder ClientBuilderFunc
}

// Reconcile is part of the main kubernetes reconciliation loop
func (r *DockerHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// Ensure ClientBuilder is set
	if r.ClientBuilder == nil {
		r.ClientBuilder = func(opts ...dockerclient.Opt) (dockerclient.APIClient, error) {
			return dockerclient.NewClientWithOpts(opts...)
		}
	}

	// Fetch DockerHost
	host := &appv1alpha1.DockerHost{}
	err := r.Get(ctx, req.NamespacedName, host)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Ping Docker Daemon
	err = r.pingDockerHost(ctx, host)

	newPhase := "Connected"
	newMessage := "Successfully connected to Docker Daemon"
	if err != nil {
		newPhase = "Error"
		newMessage = err.Error()
		l.Error(err, "Failed to connect to Docker Host", "HostURL", host.Spec.HostURL)
	}

	// Update Status if changed
	if host.Status.Phase != newPhase || host.Status.Message != newMessage {
		host.Status.Phase = newPhase
		host.Status.Message = newMessage
		if err := r.Status().Update(ctx, host); err != nil {
			l.Error(err, "Failed to update DockerHost status")
			return ctrl.Result{}, err
		}
	}

	// Requeue periodically to check connection
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *DockerHostReconciler) pingDockerHost(ctx context.Context, host *appv1alpha1.DockerHost) error {
	opts := []dockerclient.Opt{
		dockerclient.WithHost(host.Spec.HostURL),
		dockerclient.WithAPIVersionNegotiation(),
	}

	if host.Spec.TLSSecretName != "" {
		secret := &corev1.Secret{}
		secretKey := k8sclient.ObjectKey{
			Namespace: host.Namespace,
			Name:      host.Spec.TLSSecretName,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return fmt.Errorf("failed to get TLS Secret: %w", err)
		}

		tlsConfig, err := common.GetTLSConfig(secret)
		// Note: getTLSConfig is available since it's in the same package
		if err != nil {
			return fmt.Errorf("failed to load TLS config: %w", err)
		}

		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
			Timeout: 5 * time.Second,
		}
		opts = append(opts, dockerclient.WithHTTPClient(httpClient))
	}

	cli, err := r.ClientBuilder(opts...)
	if err != nil {
		return err
	}
	defer cli.Close()

	_, err = cli.Ping(ctx)
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *DockerHostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1alpha1.DockerHost{}).
		Complete(r)
}
