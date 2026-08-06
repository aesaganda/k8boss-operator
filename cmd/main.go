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
		leaderElectionNS     string
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
	// Empty keeps controller-runtime's in-cluster detection (it reads the
	// ServiceAccount's namespace file), which is what the shipped Deployment
	// relies on. That detection has no out-of-cluster equivalent, so running
	// locally with --leader-elect=true failed outright with "unable to find
	// leader election namespace" — a manager that could not start at all in
	// the one setup where you would test failover by hand.
	flag.StringVar(&leaderElectionNS, "leader-election-namespace", os.Getenv("K8BOSS_LEADER_ELECTION_NAMESPACE"),
		"Namespace holding the leader-election Lease. Empty = detect from the in-cluster "+
			"ServiceAccount; set it to run with --leader-elect outside a cluster "+
			"(env: K8BOSS_LEADER_ELECTION_NAMESPACE).")
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

	// Required, for the same reason as the two above: it is this pod's own
	// config, not someone else's outage. The backend now fails closed on an
	// unset OPERATOR_API_TOKEN (503, code operator_token_not_configured)
	// because that surface writes graph edges and holds the kill switch — so
	// starting without a token buys nothing but a reconciler that 503s on
	// every call while readiness quietly reports the backend at fault.
	// Crash-looping with this message names the actual problem.
	operatorToken := os.Getenv("K8BOSS_OPERATOR_TOKEN")
	if operatorToken == "" {
		setupLog.Error(nil, "K8BOSS_OPERATOR_TOKEN is required; the backend's operator "+
			"control-plane API does not run unauthenticated. Set OPERATOR_API_TOKEN on the "+
			"backend and put the same value in the k8boss-operator Secret (key: token).")
		os.Exit(1)
	}
	backend := backendclient.New(backendURL, operatorToken)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "k8boss-operator.k8boss.io",
		LeaderElectionNamespace: leaderElectionNS,
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
		// Same singleton the other two reconcilers gate on. Without it this
		// controller pushed EVERY PlatformConfig's spec.paused to the
		// cluster-wide kill switch, so a second CR of any name could unpause
		// the control plane behind the operator's own gate.
		PlatformConfigName: platformConfigName,
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
