// Command manager runs the K8Boss operator: three reconcilers (ADR-0003 §3)
// over one controller-runtime Manager.
//
// Deployment model is deliberately single-cluster-per-operator-instance. One
// operator process serves exactly one K8Boss cluster registration, so cluster
// identity is resolved ONCE here at startup from K8BOSS_CLUSTER_ID and handed
// to each reconciler as a plain field — never re-derived per-CR, and no
// multi-cluster routing is built (YAGNI; nothing needs it yet). See NOTES.md
// for why the env var rather than a CRD field.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8boss.io/operator/api/v1alpha1"
	"k8boss.io/operator/internal/backendclient"
	"k8boss.io/operator/internal/controller"
	"k8boss.io/operator/internal/preflight"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilRuntimeMust(clientgoscheme.AddToScheme(scheme))
	utilRuntimeMust(v1alpha1.AddToScheme(scheme))
}

func utilRuntimeMust(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		backendURL           string
		clusterIDFlag        string
		platformConfigName   string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	// Off by default because leader election needs coordination.k8s.io Lease
	// RBAC beyond the marker-generated role. The shipped install turns it ON
	// (--leader-elect=true in config/manager/manager.yaml, with
	// config/rbac/leader_election_role.yaml): replicas:1 does not make a
	// second manager impossible — a rolling update overlaps two pods by
	// construction — and two managers double-writing to the control-plane API
	// fails silently.
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election, ensuring only one active manager. Requires Lease RBAC.")
	flag.StringVar(&backendURL, "backend-url", os.Getenv("K8BOSS_BACKEND_URL"),
		"Base URL of the K8Boss backend control-plane API (env: K8BOSS_BACKEND_URL).")
	flag.StringVar(&clusterIDFlag, "cluster-id", os.Getenv("K8BOSS_CLUSTER_ID"),
		"K8Boss cluster registration id this operator instance serves (env: K8BOSS_CLUSTER_ID).")
	flag.StringVar(&platformConfigName, "platformconfig-name", envOr("K8BOSS_PLATFORMCONFIG_NAME", "default"),
		"Name of the cluster-scoped singleton PlatformConfig (env: K8BOSS_PLATFORMCONFIG_NAME).")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Fail fast on missing identity/connection config. A manager that starts
	// without knowing which cluster it serves would happily reconcile CRs
	// against cluster 0 — a confidently wrong answer, which is worse than
	// not starting.
	if backendURL == "" {
		setupLog.Error(nil, "K8BOSS_BACKEND_URL (or --backend-url) is required; "+
			"the operator has no other path into the Knowledge Graph")
		os.Exit(1)
	}
	clusterID, err := parseClusterID(clusterIDFlag)
	if err != nil {
		setupLog.Error(err, "K8BOSS_CLUSTER_ID (or --cluster-id) is required and must be a "+
			"positive integer matching this cluster's K8Boss registration")
		os.Exit(1)
	}

	// The token is optional by design — the backend treats an empty
	// OPERATOR_API_TOKEN as "no check", relying on cluster-internal Service
	// networking (same trade-off as OTLP_INGEST_TOKEN). But this surface
	// mutates the Knowledge Graph and carries the kill switch, unlike the
	// ingest-only OTLP receivers, so running without one is worth saying out
	// loud rather than leaving to whoever reads the contract doc.
	operatorToken := os.Getenv("K8BOSS_OPERATOR_TOKEN")
	if operatorToken == "" {
		setupLog.Info("WARNING: no K8BOSS_OPERATOR_TOKEN set — control-plane calls " +
			"are unauthenticated. Anyone who can reach the backend Service can write " +
			"graph edges and flip the kill switch. Set OPERATOR_API_TOKEN on the " +
			"backend and K8BOSS_OPERATOR_TOKEN here before running unattended.")
	}
	backend := backendclient.New(backendURL, operatorToken)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "k8boss-operator.k8boss.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.SegmentationPolicyReconciler{
		Client:             mgr.GetClient(),
		Backend:            backend,
		ClusterID:          clusterID,
		PlatformConfigName: platformConfigName,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SegmentationPolicy")
		os.Exit(1)
	}

	if err := (&controller.RuntimeSecurityPolicyReconciler{
		Client:             mgr.GetClient(),
		Backend:            backend,
		ClusterID:          clusterID,
		PlatformConfigName: platformConfigName,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RuntimeSecurityPolicy")
		os.Exit(1)
	}

	if err := (&controller.PlatformConfigReconciler{
		Client:    mgr.GetClient(),
		Backend:   backend,
		ClusterID: clusterID,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PlatformConfig")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// Liveness stays Ping deliberately: an unreachable backend must fail
	// readiness, not restart a healthy operator into a crash-loop over
	// someone else's outage.
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	// Ping alone answers "the process is up". Readiness has to mean "the CRDs
	// are served and the control plane answers", or the Deployment reports
	// Ready while every reconcile is failing.
	if err := mgr.AddReadyzCheck("preflight",
		preflight.New(mgr.GetConfig(), backend).Check); err != nil {
		setupLog.Error(err, "unable to set up preflight ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "clusterID", clusterID,
		"backendURL", backendURL, "platformConfigName", platformConfigName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func parseClusterID(raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("not set")
	}
	id, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not an integer: %w", raw, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("%d is not a positive cluster id", id)
	}
	return id, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
