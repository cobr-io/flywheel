// Package main is the entry point for the image-builder-controller, which
// reconciles Flux GitRepository resources by dispatching build Jobs whenever
// the observed revision changes.
//
// All client-specific configuration comes from the `flywheel-config` ConfigMap
// (per design § The `flywheel-config` ConfigMap), injected as environment
// variables via valueFrom: configMapKeyRef on the controller Deployment. Flag
// overrides exist only for unit tests and one-shot debugging — production
// pods rely entirely on the ConfigMap.
package main

import (
	"flag"
	"fmt"
	"os"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/cobr-io/flywheel/internal/controller"
	"github.com/cobr-io/flywheel/internal/naming"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sourcev1.AddToScheme(scheme))
}

func envOrFlag(flagVal, envKey string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envKey)
}

func main() {
	var (
		metricsAddr      string
		probeAddr        string
		nsFlag           string
		builderNsFlag    string
		registryFlag     string
		portFlag         string
		clusterFlag      string
		clientFlag       string
		buildKitAddrFlag string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.StringVar(&nsFlag, "namespace", "", "controller's own namespace (override env FLYWHEEL_NAMESPACE)")
	flag.StringVar(&builderNsFlag, "builder-namespace", "", "namespace to watch for GitRepository / build-config (override env BUILDER_NAMESPACE)")
	flag.StringVar(&registryFlag, "registry", "", "k3d registry name (override env CLUSTER_REGISTRY)")
	flag.StringVar(&portFlag, "registry-port", "", "k3d registry port (override env CLUSTER_REGISTRY_PORT)")
	flag.StringVar(&clusterFlag, "cluster-name", "", "k3d cluster name (override env CLUSTER_NAME)")
	flag.StringVar(&clientFlag, "client-name", "", "client name; used as label prefix (override env CLIENT_NAME)")
	flag.StringVar(&buildKitAddrFlag, "buildkit-addr", "", fmt.Sprintf("buildkitd gRPC address (override env BUILDKIT_ADDR; default tcp://buildkitd.%s:1234)", naming.FlywheelNamespace))

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := controller.Config{
		Namespace:        envOrFlag(nsFlag, "FLYWHEEL_NAMESPACE"),
		BuilderNamespace: envOrFlag(builderNsFlag, "BUILDER_NAMESPACE"),
		Registry:         envOrFlag(registryFlag, "CLUSTER_REGISTRY"),
		RegistryPort:     envOrFlag(portFlag, "CLUSTER_REGISTRY_PORT"),
		ClusterName:      envOrFlag(clusterFlag, "CLUSTER_NAME"),
		ClientName:       envOrFlag(clientFlag, "CLIENT_NAME"),
		BuildKitAddr:     envOrFlag(buildKitAddrFlag, "BUILDKIT_ADDR"),
		BuildKitClientImage: os.Getenv("BUILDKIT_CLIENT_IMAGE"),
	}

	if err := cfg.Validate(); err != nil {
		ctrl.Log.Error(err, "incomplete flywheel-config: ensure the ConfigMap is mounted and contains every required key")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.GitRepositoryBuildReconciler{
		Client: mgr.GetClient(),
		Config: cfg,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if err := (&controller.BuildJobReconciler{
		Client: mgr.GetClient(),
		Config: cfg,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create build-job image-scan controller")
		os.Exit(1)
	}

	if err := (&controller.ImagePolicyIUAReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create imagepolicy-iua controller")
		os.Exit(1)
	}

	if err := (&controller.IUASourcePokeReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create iua-source-poke controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up readiness check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting manager",
		"namespace", cfg.Namespace,
		"builderNamespace", cfg.BuilderNamespace,
		"registry", cfg.RegistryURL(),
		"client", cfg.ClientName)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
