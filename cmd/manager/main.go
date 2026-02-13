package main

import (
	"flag"
	"net/http"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appv1alpha1 "github.com/tunghauvan/k8s-docker-operator/api/v1alpha1"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/dockercontainer"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/dockerdeployment"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/dockerhost"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/dockerjob"
	"github.com/tunghauvan/k8s-docker-operator/internal/controller/dockerservice"
	"github.com/tunghauvan/k8s-docker-operator/internal/tunnel"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(appv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	// CLI: tunnel [server|client] [args...]
	if len(os.Args) > 1 && os.Args[1] == "tunnel" {
		tunnelCmd := flag.NewFlagSet("tunnel", flag.ExitOnError)
		mode := tunnelCmd.String("mode", "server", "Mode: server or client")

		// Server Args
		listenAddr := tunnelCmd.String("listen-addr", ":8080", "Server: TCP Listen Address")
		wsAddr := tunnelCmd.String("ws-addr", ":8081", "Server: WebSocket Listen Address")
		authToken := tunnelCmd.String("auth-token", "", "Server: Authentication token for tunnel clients")

		// Client Args
		// Client Args
		serverURL := tunnelCmd.String("server-url", "ws://localhost:8081/ws", "Client: Tunnel Server URL")
		targetAddr := tunnelCmd.String("target-addr", "localhost:6379", "Client: Target Address")
		// Reuse authToken for client as well, or define a new one "client-token"
		// To match reconciler's intent, let's allow auth-token to be used by client too.
		// Since authToken is already defined above for server, we can reuse it if we parse flags.
		// But flags are parsed below. The variable `authToken` is already declared.
		// However, the help text says "Server: ...". Let's just use the same flag variable.

		// Gateway Args
		gatewayListenAddr := tunnelCmd.String("gateway-addr", ":8080", "Gateway: Listen Address")

		tunnelCmd.Parse(os.Args[2:])

		setupLog.Info("Starting Tunnel", "mode", *mode)

		if *mode == "server" {
			srv := tunnel.NewServer(*listenAddr, *wsAddr, *authToken)
			if err := srv.Start(); err != nil {
				setupLog.Error(err, "Tunnel Server failed")
				os.Exit(1)
			}
		} else if *mode == "client" {
			// Client needs token
			cli := tunnel.NewClient(*serverURL, *targetAddr, *authToken)
			if err := cli.Start(); err != nil {
				setupLog.Error(err, "Tunnel Client failed")
				os.Exit(1)
			}
		} else if *mode == "gateway" {
			if err := tunnel.RunGateway(*gatewayListenAddr); err != nil {
				setupLog.Error(err, "Tunnel Gateway failed")
				os.Exit(1)
			}
		}
		return
	}

	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var logsAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&logsAddr, "logs-bind-address", ":8082", "The address the logs endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "94c73569.example.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	reconciler := &dockercontainer.DockerContainerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}

	if err = reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DockerContainer")
		os.Exit(1)
	}

	if err = (&dockerhost.DockerHostReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DockerHost")
		os.Exit(1)
	}

	// Register DockerServiceReconciler
	if err = (&dockerservice.DockerServiceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DockerService")
		os.Exit(1)
	}

	// Register DockerJobReconciler
	if err = (&dockerjob.DockerJobReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DockerJob")
		os.Exit(1)
	}

	// Register DockerDeploymentReconciler
	if err = (&dockerdeployment.DockerDeploymentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DockerDeployment")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Start container logs HTTP server
	go func() {
		logsMux := http.NewServeMux()
		logsMux.Handle("/logs", reconciler.LogsHandler())
		setupLog.Info("starting logs server", "addr", logsAddr)
		if err := http.ListenAndServe(logsAddr, logsMux); err != nil {
			setupLog.Error(err, "logs server failed")
		}
	}()

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
