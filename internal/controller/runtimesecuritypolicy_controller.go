package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8boss.io/operator/api/v1alpha1"
	"k8boss.io/operator/internal/backendclient"
)

// crKindRuntimeSecurityPolicy is the cr_kind the close endpoint validates against.
const crKindRuntimeSecurityPolicy = "RuntimeSecurityPolicy"

// RuntimeSecurityPolicyReconciler turns RuntimeSecurityPolicy intent into
// MONITORED_BY edges in the Knowledge Graph, via the control-plane API.
//
// This reconciler lives strictly on the RuntimeSecurityProvider side of the
// repo's Flow/Runtime split (root CLAUDE.md). It imports nothing on a
// FlowProvider path, has no notion of flow providers or enforcement
// providers, and shares no reconcile logic with SegmentationPolicyReconciler
// beyond the generic helpers in common.go. The similarity of the two files is
// the point: they are parallel, not shared.
//
// The reason they cannot be merged even if they look alike: this one has a
// provider-availability precondition the other does not. A 503 from the
// control plane means Tetragon was not confirmed installed and NOTHING was
// written — that is "we could not look", not "we looked and found nothing to
// monitor". Collapsing it into a zero-count success would be exactly the
// no_flow_observed / no_flow_provider_available conflation the repo forbids.
type RuntimeSecurityPolicyReconciler struct {
	client.Client

	Backend *backendclient.Client

	// ClusterID identifies this cluster to the control-plane API. Resolved
	// once at manager startup (see cmd/main.go).
	ClusterID int

	// PlatformConfigName is the singleton cluster-scoped PlatformConfig
	// consulted for the pause gate.
	PlatformConfigName string
}

// +kubebuilder:rbac:groups=k8boss.io,resources=runtimesecuritypolicies,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=k8boss.io,resources=runtimesecuritypolicies/status,verbs=get;update
// +kubebuilder:rbac:groups=k8boss.io,resources=runtimesecuritypolicies/finalizers,verbs=update

func (r *RuntimeSecurityPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cr v1alpha1.RuntimeSecurityPolicy
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cr)
	}

	// Pause gate FIRST, finalizer after — see the long note in
	// segmentationpolicy_controller.go. Attaching the finalizer above the gate
	// deadlocked deletion for any CR reconciled while paused: it had no edges
	// (the backend was never called) but finalize() holds the finalizer when
	// CloseCREdges reports paused, leaving the object in Terminating forever.
	// The finalizer still precedes the first mutating call, because the gate
	// returns without one.
	if gate := checkPauseGate(ctx, r.Client, r.PlatformConfigName); gate.Paused {
		logger.Info("skipping reconcile: paused", "reason", gate.Reason)
		return r.notReady(ctx, &cr, gate.Reason, gate.Message, PausedRequeue)
	}

	if !controllerutil.ContainsFinalizer(&cr, CREdgesFinalizer) {
		controllerutil.AddFinalizer(&cr, CREdgesFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	result, err := r.Backend.ReconcileRuntimeSecurityPolicy(ctx, r.ClusterID,
		cr.Namespace, cr.Name, toRuntimeWireSpec(cr.Spec))
	if err != nil {
		return r.reconcileError(ctx, &cr, err)
	}

	if why := inconsistentRuntimeResult(result); why != "" {
		return r.notReady(ctx, &cr, ReasonWriteNotVerified,
			fmt.Sprintf("runtime security policy reconcile returned 200 but the response does not "+
				"confirm the write: %s. Not marking Ready.", why),
			0)
	}

	// CorrelatedEvidenceCount is how much evidence the backend actually
	// attached, taken from the confirmed response. matched_workloads and
	// edges_upserted are equal on a converged reconcile (checked above), so
	// either is the count; edges_upserted is used because it is the number
	// of edges that verifiably landed, not the number of candidates found.
	cr.Status.CorrelatedEvidenceCount = int64(result.EdgesUpserted)
	cr.Status.ObservedGeneration = cr.Generation
	setCondition(&cr.Status.Conditions, cr.Generation, ConditionReady,
		metav1.ConditionTrue, ReasonReconciled,
		runtimeReadyMessage(result))

	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after successful reconcile: %w", err)
	}
	return ctrl.Result{RequeueAfter: ResyncInterval}, nil
}

// inconsistentRuntimeResult returns a human-readable reason when a 200
// response does not evidence a converged write, or "" when it does.
//
// Note the provider_installed check: a 200 that does not affirm the provider
// was installed is not a result we may build a Ready=True on, even though the
// server is supposed to have raised 503 in that case. Trusting the absence of
// an error over the presence of a confirmation is the failure mode this whole
// migration guards against.
func inconsistentRuntimeResult(res *backendclient.ReconcileResult) string {
	switch {
	case res.Status != "ok":
		return fmt.Sprintf("status was %q, not \"ok\"", res.Status)
	case res.ProviderInstalled == nil:
		return "provider_installed was absent, so runtime provider availability is unconfirmed"
	case !*res.ProviderInstalled:
		return "provider_installed was false, so no runtime evidence could have been collected"
	case len(res.UnverifiedEdgeKeys) > 0:
		return fmt.Sprintf("%d edge(s) did not verify as open after commit", len(res.UnverifiedEdgeKeys))
	case res.EdgesUpserted != res.MatchedWorkloads:
		return fmt.Sprintf("edges_upserted=%d does not match matched_workloads=%d",
			res.EdgesUpserted, res.MatchedWorkloads)
	}
	return ""
}

// runtimeReadyMessage describes a confirmed reconcile. The zero case is
// spelled out because correlatedEvidenceCount=0 is ambiguous on its face: this
// message pins it to "the runtime provider was confirmed installed and matched
// nothing", which is a different fact from the ProviderUnavailable path where
// the count is unknown.
func runtimeReadyMessage(result *backendclient.ReconcileResult) string {
	if result.MatchedWorkloads == 0 {
		return fmt.Sprintf("Runtime provider confirmed installed, and NO workload in this namespace "+
			"matches spec.selector (a checked zero, not an unchecked one): "+
			"correlatedEvidenceCount=0 means nothing to monitor, not a failed check. "+
			"%d edge(s) closed.", result.EdgesClosed)
	}
	return fmt.Sprintf("Control plane confirmed the policy against a reachable runtime provider: "+
		"%d workload(s) matched, %d edge(s) upserted, %d edge(s) closed.",
		result.MatchedWorkloads, result.EdgesUpserted, result.EdgesClosed)
}

// reconcileError maps a failed control-plane call onto a specific condition.
func (r *RuntimeSecurityPolicyReconciler) reconcileError(ctx context.Context,
	cr *v1alpha1.RuntimeSecurityPolicy, err error) (ctrl.Result, error) {

	if backendclient.IsPaused(err) {
		return r.notReady(ctx, cr, ReasonPaused,
			fmt.Sprintf("Control-plane API is paused server-side; no mutation was applied: %v", err),
			PausedRequeue)
	}

	if backendclient.IsProviderUnavailable(err) {
		// The whole point of this branch. status.correlatedEvidenceCount is
		// deliberately left alone rather than zeroed: writing 0 here would
		// assert "no runtime evidence exists", when the truth is that we
		// could not look at all. The condition says which.
		return r.notReady(ctx, cr, ReasonProviderUnavailable,
			fmt.Sprintf("runtime security policy reconcile failed: provider_unavailable — the "+
				"runtime security provider (Tetragon) is not confirmed installed on this cluster, "+
				"so NO runtime evidence was collected and none could be. This is not "+
				"\"nothing to monitor\": correlatedEvidenceCount is unknown, not zero. "+
				"Underlying error: %v", err),
			PausedRequeue)
	}

	var apiErr *backendclient.APIError
	if errors.As(err, &apiErr) {
		reason := ReasonReconcileFailed
		if backendclient.IsWriteNotVerified(err) {
			reason = ReasonWriteNotVerified
		}
		return r.notReady(ctx, cr, reason,
			fmt.Sprintf("runtime security policy reconcile failed: %s — %s", apiErr.Code, apiErr.Message),
			0)
	}

	if _, uerr := r.notReady(ctx, cr, ReasonBackendUnreachable,
		fmt.Sprintf("runtime security policy reconcile could not reach the control-plane API, so "+
			"this policy's monitored state is unknown: %v", err), 0); uerr != nil {
		return ctrl.Result{}, uerr
	}
	return ctrl.Result{}, err
}

// notReady records a Ready=False condition. status.observedGeneration is not
// advanced on failure, so a reader can tell a reconciled spec from an
// attempted one.
func (r *RuntimeSecurityPolicyReconciler) notReady(ctx context.Context, cr *v1alpha1.RuntimeSecurityPolicy,
	reason, message string, requeueAfter time.Duration) (ctrl.Result, error) {

	setCondition(&cr.Status.Conditions, cr.Generation, ConditionReady,
		metav1.ConditionFalse, reason, message)
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status (%s): %w", reason, err)
	}
	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{}, fmt.Errorf("%s: %s", reason, message)
}

// finalize closes this CR's graph edges, and only then releases the object.
func (r *RuntimeSecurityPolicyReconciler) finalize(ctx context.Context,
	cr *v1alpha1.RuntimeSecurityPolicy) (ctrl.Result, error) {

	if !controllerutil.ContainsFinalizer(cr, CREdgesFinalizer) {
		return ctrl.Result{}, nil
	}

	res, err := r.Backend.CloseCREdges(ctx, crKindRuntimeSecurityPolicy, r.ClusterID, cr.Namespace, cr.Name)
	if err != nil {
		if backendclient.IsPaused(err) {
			return r.notReady(ctx, cr, ReasonPaused,
				fmt.Sprintf("Deletion is blocked: the control-plane API is paused, so this CR's "+
					"graph edges cannot be closed yet. The finalizer is held until it resumes: %v", err),
				PausedRequeue)
		}
		return r.notReady(ctx, cr, ReasonCloseFailed,
			fmt.Sprintf("Deletion is blocked: closing this CR's graph edges failed, so the "+
				"finalizer is held rather than orphaning them: %v", err), 0)
	}
	if res.Status != "ok" || len(res.UnverifiedEdgeKeys) > 0 {
		return r.notReady(ctx, cr, ReasonCloseFailed,
			fmt.Sprintf("Deletion is blocked: close returned status=%q with %d unverified edge key(s); "+
				"the edges are not confirmed closed, so the finalizer is held.",
				res.Status, len(res.UnverifiedEdgeKeys)), 0)
	}

	controllerutil.RemoveFinalizer(cr, CREdgesFinalizer)
	if err := r.Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer after confirmed edge close: %w", err)
	}
	log.FromContext(ctx).Info("closed CR edges and released finalizer",
		"kind", crKindRuntimeSecurityPolicy, "edgesClosed", res.EdgesClosed)
	return ctrl.Result{}, nil
}

// toRuntimeWireSpec converts the CRD spec to the API's wire shape. Kept
// separate from SegmentationPolicy's converter on purpose — see the type
// comment above.
func toRuntimeWireSpec(spec v1alpha1.RuntimeSecurityPolicySpec) backendclient.RuntimeSecurityPolicySpec {
	out := backendclient.RuntimeSecurityPolicySpec{
		Selector:         backendclient.WorkloadSelector{MatchLabels: spec.Selector.MatchLabels},
		TracingPolicyRef: spec.TracingPolicyRef,
		DeniedBinaries:   spec.DeniedBinaries,
	}
	if out.Selector.MatchLabels == nil {
		out.Selector.MatchLabels = map[string]string{}
	}
	if out.DeniedBinaries == nil {
		out.DeniedBinaries = []string{}
	}
	return out
}

// SetupWithManager registers this reconciler with the manager.
func (r *RuntimeSecurityPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.RuntimeSecurityPolicy{}).
		Named("runtimesecuritypolicy").
		Complete(r)
}
