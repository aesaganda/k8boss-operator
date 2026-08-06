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

// crKindSegmentationPolicy is the cr_kind the close endpoint validates against.
const crKindSegmentationPolicy = "SegmentationPolicy"

// SegmentationPolicyReconciler turns SegmentationPolicy intent into
// ENFORCED_BY edges in the Knowledge Graph, via the control-plane API.
//
// It shares no policy-shaped logic with RuntimeSecurityPolicyReconciler —
// only the generic helpers in common.go and the HTTP client. The two CRDs sit
// on opposite sides of the FlowProvider / RuntimeSecurityProvider split and
// their reconcile semantics differ (this one has no provider-availability
// precondition; that one does).
type SegmentationPolicyReconciler struct {
	client.Client

	Backend *backendclient.Client

	// ClusterID identifies this cluster to the control-plane API. Resolved
	// once at manager startup (see cmd/main.go) — one operator instance
	// serves exactly one cluster.
	ClusterID int

	// PlatformConfigName is the singleton cluster-scoped PlatformConfig
	// consulted for the pause gate.
	PlatformConfigName string
}

// +kubebuilder:rbac:groups=k8boss.io,resources=segmentationpolicies,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=k8boss.io,resources=segmentationpolicies/status,verbs=get;update
// +kubebuilder:rbac:groups=k8boss.io,resources=segmentationpolicies/finalizers,verbs=update

func (r *SegmentationPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cr v1alpha1.SegmentationPolicy
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		// The only silent exit in this reconciler, and only because the
		// object is gone: there is no status left to write a condition to.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cr)
	}

	// The pause gate comes FIRST, and the finalizer only after it.
	//
	// The finalizer still lands before the first mutating call — the gate below
	// returns without calling the backend — so the invariant it exists for ("a
	// CR can never have graph edges without something guaranteeing they get
	// closed") holds exactly as before.
	//
	// Attaching it above the gate instead was a deadlock: a CR reconciled while
	// paused got the finalizer but never reached ReconcileSegmentationPolicy, so
	// it provably had NO edges. Deleting it then ran finalize(), which calls
	// CloseCREdges, gets a paused error back, and deliberately holds the
	// finalizer ("Deletion is blocked ... until it resumes"). The object sat in
	// Terminating forever with nothing to clean up, and a namespace containing
	// one could not be deleted either. PlatformConfig.spec.paused DEFAULTS TO
	// TRUE, so that was the out-of-the-box path, not an edge case.
	gate := checkPauseGate(ctx, r.Client, r.PlatformConfigName)
	if gate.Paused {
		logger.Info("skipping reconcile: paused", "reason", gate.Reason)
		return r.notReady(ctx, &cr, gate.Reason, gate.Message, PausedRequeue)
	}

	if !controllerutil.ContainsFinalizer(&cr, CREdgesFinalizer) {
		controllerutil.AddFinalizer(&cr, CREdgesFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	result, err := r.Backend.ReconcileSegmentationPolicy(ctx, r.ClusterID,
		cr.Namespace, cr.Name, toWireSpec(cr.Spec))
	if err != nil {
		return r.reconcileError(ctx, &cr, err)
	}

	// Only now, with a confirmed 200 in hand, may Ready become true — and
	// only if the response is internally consistent with what we asked for.
	if why := inconsistentSegmentationResult(result); why != "" {
		return r.notReady(ctx, &cr, ReasonWriteNotVerified,
			fmt.Sprintf("segmentation policy reconcile returned 200 but the response does not "+
				"confirm the write: %s. Not marking Ready.", why),
			0)
	}

	// Which FlowProvider this reconcile ran under — spec.providerRef when set,
	// otherwise the cluster default read from PlatformConfig just above. This
	// is resolved intent, and the field name now says so: nothing in the
	// reconcile response confirms enforcement, and a FlowProvider would not be
	// the thing doing it anyway. Set only on this confirmed-success path.
	provider := cr.Spec.ProviderRef
	if provider == "" {
		provider = gate.FlowProvider
	}

	cr.Status.ResolvedFlowProvider = provider
	cr.Status.ObservedGeneration = cr.Generation
	setCondition(&cr.Status.Conditions, cr.Generation, ConditionReady,
		metav1.ConditionTrue, ReasonReconciled,
		segmentationReadyMessage(result))

	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after successful reconcile: %w", err)
	}
	return ctrl.Result{RequeueAfter: ResyncInterval}, nil
}

// inconsistentSegmentationResult returns a human-readable reason when a 200
// response does not actually evidence a converged write, or "" when it does.
//
// The server builds exactly one ENFORCED_BY edge per matched workload
// (backend/app/operator/reconcile.py), so edges_upserted != matched_workloads
// means the response is not describing the reconcile we asked for and Ready
// must not be set from it.
func inconsistentSegmentationResult(res *backendclient.ReconcileResult) string {
	switch {
	case res.Status != "ok":
		return fmt.Sprintf("status was %q, not \"ok\"", res.Status)
	case len(res.UnverifiedEdgeKeys) > 0:
		return fmt.Sprintf("%d edge(s) did not verify as open after commit", len(res.UnverifiedEdgeKeys))
	case res.EdgesUpserted != res.MatchedWorkloads:
		return fmt.Sprintf("edges_upserted=%d does not match matched_workloads=%d",
			res.EdgesUpserted, res.MatchedWorkloads)
	}
	return ""
}

// segmentationReadyMessage describes a confirmed reconcile. What is confirmed
// is the *graph write*, not that anything enforces the policy — "confirmed the
// policy" read as the latter. A zero match is stated as a *confirmed* zero: the
// control plane returned 200, which means it successfully enumerated the
// namespace and found nothing matching — unlike a failure path, where the count
// is unknown rather than zero.
func segmentationReadyMessage(result *backendclient.ReconcileResult) string {
	if result.MatchedWorkloads == 0 {
		return fmt.Sprintf("Control plane recorded this policy's intent in the Knowledge Graph, "+
			"and confirmed that NO workload in this namespace matches spec.selector (a checked "+
			"zero, not an unchecked one): 0 edge(s) upserted, %d edge(s) closed.",
			result.EdgesClosed)
	}
	return fmt.Sprintf("Control plane recorded this policy's intent in the Knowledge Graph: "+
		"%d workload(s) matched, %d edge(s) upserted, %d edge(s) closed.",
		result.MatchedWorkloads, result.EdgesUpserted, result.EdgesClosed)
}

// reconcileError maps a failed control-plane call onto a specific condition.
func (r *SegmentationPolicyReconciler) reconcileError(ctx context.Context,
	cr *v1alpha1.SegmentationPolicy, err error) (ctrl.Result, error) {

	if backendclient.IsPaused(err) {
		// Expected state, not a fault: the server-side kill switch is on even
		// though PlatformConfig said otherwise (e.g. someone paused the API
		// directly). Back off instead of hammering it.
		return r.notReady(ctx, cr, ReasonPaused,
			fmt.Sprintf("Control-plane API is paused server-side; no mutation was applied: %v", err),
			PausedRequeue)
	}

	var apiErr *backendclient.APIError
	if errors.As(err, &apiErr) {
		reason := ReasonReconcileFailed
		if backendclient.IsWriteNotVerified(err) {
			// The write may or may not have landed. Neither Ready=True nor a
			// count may be concluded from it — only a retry.
			reason = ReasonWriteNotVerified
		}
		return r.notReady(ctx, cr, reason,
			fmt.Sprintf("segmentation policy reconcile failed: %s — %s", apiErr.Code, apiErr.Message),
			0)
	}

	// Never reached the API at all — we know nothing about cluster state.
	if _, uerr := r.notReady(ctx, cr, ReasonBackendUnreachable,
		fmt.Sprintf("segmentation policy reconcile could not reach the control-plane API, so the "+
			"policy's applied state is unknown: %v", err), 0); uerr != nil {
		return ctrl.Result{}, uerr
	}
	return ctrl.Result{}, err
}

// notReady records a Ready=False condition and returns. requeueAfter of 0
// means "let the controller's error backoff handle it" via the returned error
// path; a non-zero value means this is an expected state to re-check later.
//
// status.observedGeneration is deliberately NOT advanced here: a caller must
// be able to distinguish "reconciled the latest spec" from "tried and
// failed". The condition itself does carry the generation.
func (r *SegmentationPolicyReconciler) notReady(ctx context.Context, cr *v1alpha1.SegmentationPolicy,
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
func (r *SegmentationPolicyReconciler) finalize(ctx context.Context,
	cr *v1alpha1.SegmentationPolicy) (ctrl.Result, error) {

	if !controllerutil.ContainsFinalizer(cr, CREdgesFinalizer) {
		return ctrl.Result{}, nil
	}

	res, err := r.Backend.CloseCREdges(ctx, crKindSegmentationPolicy, r.ClusterID, cr.Namespace, cr.Name)
	if err != nil {
		if backendclient.IsPaused(err) {
			// The kill switch covers cleanup too (contract §3). Hold the
			// object rather than deleting it with its edges still open.
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
		"kind", crKindSegmentationPolicy, "edgesClosed", res.EdgesClosed)
	return ctrl.Result{}, nil
}

// toWireSpec converts the CRD spec to the API's wire shape. Kept local to
// this file: RuntimeSecurityPolicy has its own, and merging them would be the
// first step toward sharing policy-shaped logic across the split.
func toWireSpec(spec v1alpha1.SegmentationPolicySpec) backendclient.SegmentationPolicySpec {
	out := backendclient.SegmentationPolicySpec{
		Selector:    backendclient.WorkloadSelector{MatchLabels: spec.Selector.MatchLabels},
		Ingress:     make([]backendclient.SegmentationRule, 0, len(spec.Ingress)),
		Egress:      make([]backendclient.SegmentationRule, 0, len(spec.Egress)),
		ProviderRef: spec.ProviderRef,
	}
	if out.Selector.MatchLabels == nil {
		out.Selector.MatchLabels = map[string]string{}
	}
	for _, rule := range spec.Ingress {
		out.Ingress = append(out.Ingress, toWireRule(rule))
	}
	for _, rule := range spec.Egress {
		out.Egress = append(out.Egress, toWireRule(rule))
	}
	return out
}

func toWireRule(rule v1alpha1.SegmentationRule) backendclient.SegmentationRule {
	out := backendclient.SegmentationRule{
		Ports:         rule.Ports,
		PeerSelector:  rule.PeerSelector,
		PeerNamespace: rule.PeerNamespace,
	}
	if out.Ports == nil {
		out.Ports = []int32{}
	}
	if out.PeerSelector == nil {
		out.PeerSelector = map[string]string{}
	}
	return out
}

// SetupWithManager registers this reconciler with the manager.
func (r *SegmentationPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SegmentationPolicy{}).
		Named("segmentationpolicy").
		Complete(r)
}
