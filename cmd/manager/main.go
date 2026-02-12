package main

import (
	"flag"
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

	appv1alpha1 "k8s-docker-operator/api/v1alpha1"
	"k8s-docker-operator/internal/controller/dockercontainer"
	"k8s-docker-operator/internal/controller/dockerhost"
	"k8s-docker-operator/internal/tunnel"
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

		// Client Args
		serverURL := tunnelCmd.String("server-url", "ws://localhost:8081/ws", "Client: Tunnel Server URL")
		targetAddr := tunnelCmd.String("target-addr", "localhost:6379", "Client: Target Address")

		// Gateway Args
		gatewayListenAddr := tunnelCmd.String("gateway-addr", ":8080", "Gateway: Listen Address")

		tunnelCmd.Parse(os.Args[2:])

		setupLog.Info("Starting Tunnel", "mode", *mode)

		if *mode == "server" {
			srv := tunnel.NewServer(*listenAddr, *wsAddr)
			if err := srv.Start(); err != nil {
				setupLog.Error(err, "Tunnel Server failed")
				os.Exit(1)
			}
		} else if *mode == "client" {
			cli := tunnel.NewClient(*serverURL, *targetAddr)
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
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
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

	if err = (&dockercontainer.DockerContainerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
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

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
