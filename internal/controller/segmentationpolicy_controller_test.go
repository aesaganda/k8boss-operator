package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"k8boss.io/operator/api/v1alpha1"
)

func segReconciler(stub *backendStub, pcName string) *SegmentationPolicyReconciler {
	return &SegmentationPolicyReconciler{
		Client:             k8sC,
		Backend:            stub.client(),
		ClusterID:          1,
		PlatformConfigName: pcName,
	}
}

func getSeg(t *testing.T, ns, name string) *v1alpha1.SegmentationPolicy {
	t.Helper()
	var out v1alpha1.SegmentationPolicy
	if err := k8sC.Get(testCtx, client.ObjectKey{Namespace: ns, Name: name}, &out); err != nil {
		t.Fatalf("re-reading SegmentationPolicy: %v", err)
	}
	return &out
}

// ── happy path ──────────────────────────────────────────────────────────

func TestSegmentation_HappyPath_ReadyOnlyAfterConfirmedWrite(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	stub.on(pathSegReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 2, "edges_closed": 1,
		"edge_keys": []string{"a", "b"}, "matched_workloads": 2,
		"unverified_edge_keys": []string{},
	}))

	newSegPolicy(t, ns, "checkout", v1alpha1.SegmentationPolicySpec{
		Selector: v1alpha1.WorkloadSelector{MatchLabels: map[string]string{"app": "checkout"}},
	})

	r := segReconciler(stub, pc)
	res, err := r.Reconcile(testCtx, req(ns, "checkout"))
	if err != nil {
		t.Fatalf("reconcile returned an error on the happy path: %v", err)
	}
	if res.RequeueAfter != ResyncInterval {
		t.Errorf("RequeueAfter = %v, want the resync interval %v", res.RequeueAfter, ResyncInterval)
	}

	got := getSeg(t, ns, "checkout")
	assertCondition(t, got.Status.Conditions, ConditionReady, metav1.ConditionTrue, ReasonReconciled)

	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d (a confirmed reconcile must advance it)",
			got.Status.ObservedGeneration, got.Generation)
	}
	// Resolved from PlatformConfig, since spec.providerRef is empty.
	if got.Status.ResolvedFlowProvider != string(v1alpha1.FlowProviderHubble) {
		t.Errorf("resolvedFlowProvider = %q, want %q",
			got.Status.ResolvedFlowProvider, v1alpha1.FlowProviderHubble)
	}
	if !controllerutil.ContainsFinalizer(got, CREdgesFinalizer) {
		t.Error("finalizer was not added before the first mutating call — a CR could hold graph " +
			"edges with nothing guaranteeing they get closed")
	}
}

func TestSegmentation_SpecProviderRefWinsOverClusterDefault(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	stub.on(pathSegReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 0, "edges_closed": 0,
		"edge_keys": []string{}, "matched_workloads": 0, "unverified_edge_keys": []string{},
	}))

	newSegPolicy(t, ns, "override", v1alpha1.SegmentationPolicySpec{
		Selector:    v1alpha1.WorkloadSelector{MatchLabels: map[string]string{"app": "x"}},
		ProviderRef: "Calico",
	})

	if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "override")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := getSeg(t, ns, "override"); got.Status.ResolvedFlowProvider != "Calico" {
		t.Errorf("resolvedFlowProvider = %q, want the spec override %q",
			got.Status.ResolvedFlowProvider, "Calico")
	}
}

// A 200 whose counts contradict the request must not produce Ready=True. The
// server is supposed to have raised 500 here; the reconciler not depending on
// that is the point.
func TestSegmentation_InconsistentOKBodyIsNotReady(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"edges_upserted != matched_workloads", map[string]any{
			"status": "ok", "edges_upserted": 1, "matched_workloads": 3,
			"edges_closed": 0, "edge_keys": []string{}, "unverified_edge_keys": []string{}}},
		{"unverified edge keys present", map[string]any{
			"status": "ok", "edges_upserted": 1, "matched_workloads": 1,
			"edges_closed": 0, "edge_keys": []string{"a"}, "unverified_edge_keys": []string{"a"}}},
		{"status is not ok", map[string]any{
			"status": "partial", "edges_upserted": 1, "matched_workloads": 1,
			"edges_closed": 0, "edge_keys": []string{}, "unverified_edge_keys": []string{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
			stub := newBackendStub(t)
			stub.on(pathSegReconcile, okBody(tc.body))
			newSegPolicy(t, ns, "p", v1alpha1.SegmentationPolicySpec{})

			if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "p")); err == nil {
				t.Fatal("reconcile returned nil error on an unconfirmed write; the controller " +
					"would never retry")
			}
			got := getSeg(t, ns, "p")
			assertCondition(t, got.Status.Conditions, ConditionReady,
				metav1.ConditionFalse, ReasonWriteNotVerified)
			if got.Status.ObservedGeneration != 0 {
				t.Errorf("observedGeneration advanced to %d on an unconfirmed write",
					got.Status.ObservedGeneration)
			}
			if got.Status.ResolvedFlowProvider != "" {
				t.Errorf("resolvedFlowProvider = %q was set from an unconfirmed response",
					got.Status.ResolvedFlowProvider)
			}
		})
	}
}

// ── error path: the specific reason must survive, not a generic failure ──

func TestSegmentation_StructuredErrorSurfacesSpecificReason(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	stub.on(pathSegReconcile, apiError(500, "write_not_verified", "graph_write_unverified",
		"2 edge(s) did not verify as open after commit"))

	newSegPolicy(t, ns, "p", v1alpha1.SegmentationPolicySpec{})

	if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "p")); err == nil {
		t.Fatal("a 500 from the control plane produced a nil error — the reconcile would not retry")
	}

	got := getSeg(t, ns, "p")
	c := assertCondition(t, got.Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonWriteNotVerified)
	// The API's stable machine key must reach the message; a human reading the
	// CR has to be able to tell WHICH failure this was.
	if !strings.Contains(c.Message, "graph_write_unverified") {
		t.Errorf("condition message lost the API's error code: %q", c.Message)
	}
	if got.Status.ObservedGeneration != 0 {
		t.Errorf("observedGeneration advanced to %d on a failed reconcile", got.Status.ObservedGeneration)
	}
}

func TestSegmentation_TransportFailureIsBackendUnreachable(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	stub.srv.Close() // nothing listening: we never reached the API at all

	newSegPolicy(t, ns, "p", v1alpha1.SegmentationPolicySpec{})

	if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "p")); err == nil {
		t.Fatal("an unreachable backend produced a nil error")
	}
	got := getSeg(t, ns, "p")
	// Not ReconcileFailed: "we could not look" is a different answer from "we
	// looked and it refused".
	assertCondition(t, got.Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonBackendUnreachable)
}

// ── paused paths ────────────────────────────────────────────────────────

// Server-side 423: distinct reason, no error returned (so no error-backoff hot
// loop), and a bounded requeue instead.
func TestSegmentation_ServerPaused423IsDistinctAndBacksOff(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	stub.on(pathSegReconcile, apiError(423, "paused", "operator_paused",
		"Operator control-plane API is paused; no mutation was applied."))

	newSegPolicy(t, ns, "p", v1alpha1.SegmentationPolicySpec{})

	res, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "p"))
	if err != nil {
		t.Fatalf("a 423 was returned as a reconcile error (%v); the controller would retry it on "+
			"the error backoff against an endpoint whose answer cannot change without a human", err)
	}
	if res.RequeueAfter != PausedRequeue {
		t.Errorf("RequeueAfter = %v, want the paused backoff %v", res.RequeueAfter, PausedRequeue)
	}
	assertCondition(t, getSeg(t, ns, "p").Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonPaused)
}

// The CRD-native gate: spec.paused=true must stop the call from being made at
// all, not merely ignore its result.
func TestSegmentation_PlatformConfigPausedMakesNoBackendCall(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, true, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	newSegPolicy(t, ns, "p", v1alpha1.SegmentationPolicySpec{})

	res, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "p"))
	if err != nil {
		t.Fatalf("paused reconcile returned an error: %v", err)
	}
	if res.RequeueAfter != PausedRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, PausedRequeue)
	}
	stub.assertNotCalled(pathSegReconcile)
	assertCondition(t, getSeg(t, ns, "p").Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonPaused)
}

// Not-yet-applied config and unreadable config both resolve to paused, but with
// their own reasons: the CR must say which question went unanswered.
func TestSegmentation_MissingPlatformConfigPausesWithItsOwnReason(t *testing.T) {
	ns := newNamespace(t)
	stub := newBackendStub(t)
	newSegPolicy(t, ns, "p", v1alpha1.SegmentationPolicySpec{})

	res, err := segReconciler(stub, "no-such-platformconfig").Reconcile(testCtx, req(ns, "p"))
	if err != nil {
		t.Fatalf("missing PlatformConfig returned an error: %v", err)
	}
	if res.RequeueAfter != PausedRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, PausedRequeue)
	}
	stub.assertNotCalled(pathSegReconcile)
	assertCondition(t, getSeg(t, ns, "p").Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonPlatformConfigMissing)
}

// The CRD's own `default: true` on spec.paused — an API-server behaviour, which
// is one of the reasons this suite uses envtest rather than a fake client. A CR
// applied without the field must come back paused, so the fail-safe holds for a
// manifest that simply omits it.
func TestPlatformConfig_PausedDefaultsTrueAtTheAPIServer(t *testing.T) {
	uniqName := applyPlatformConfigOmittingPaused(t)
	var pc v1alpha1.PlatformConfig
	if err := k8sC.Get(testCtx, client.ObjectKey{Name: uniqName}, &pc); err != nil {
		t.Fatalf("reading PlatformConfig: %v", err)
	}
	if !pc.Spec.Paused {
		t.Error("spec.paused defaulted to false when omitted — the kill switch fails open, and an " +
			"operator installed with a minimal PlatformConfig starts mutating the graph immediately")
	}
}

// A CR that only ever reconciled while paused has provably NO graph edges: the
// gate returns before ReconcileSegmentationPolicy is ever called. It must
// therefore be deletable.
//
// It was not. The finalizer was attached ABOVE the pause gate, so the paused CR
// carried one; deleting it ran finalize(), which calls CloseCREdges, is told the
// control plane is paused, and deliberately holds the finalizer ("Deletion is
// blocked ... until it resumes"). The object stayed in Terminating with nothing
// to clean up, and a namespace containing one could not be deleted either.
//
// spec.paused defaults to TRUE (see TestPlatformConfig_PausedDefaultsTrueAtThe-
// APIServer), so this was the out-of-the-box path, not a corner case.
func TestSegmentation_PausedCRHasNoFinalizerAndStaysDeletable(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, true, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	newSegPolicy(t, ns, "p", v1alpha1.SegmentationPolicySpec{})

	if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "p")); err != nil {
		t.Fatalf("paused reconcile returned an error: %v", err)
	}
	stub.assertNotCalled(pathSegReconcile)

	got := getSeg(t, ns, "p")
	if controllerutil.ContainsFinalizer(got, CREdgesFinalizer) {
		t.Fatalf("a CR reconciled only while paused carries %q, but it has no edges to close — "+
			"deleting it deadlocks in Terminating because finalize() holds the finalizer while "+
			"the control plane reports paused", CREdgesFinalizer)
	}

	// And it really does go away, rather than hanging on a held finalizer.
	if err := k8sC.Delete(testCtx, got); err != nil {
		t.Fatalf("deleting the paused CR: %v", err)
	}
	if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "p")); err != nil {
		t.Fatalf("reconcile after delete returned an error: %v", err)
	}
	var after v1alpha1.SegmentationPolicy
	err := k8sC.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "p"}, &after)
	if err == nil && !after.DeletionTimestamp.IsZero() &&
		controllerutil.ContainsFinalizer(&after, CREdgesFinalizer) {
		t.Fatal("paused CR is stuck in Terminating on a held finalizer")
	}
}

// The counterpart: once NOT paused, the finalizer must still be attached before
// the backend is called, or a successful reconcile could create edges with
// nothing guaranteeing they get closed. Moving the gate above the finalizer must
// not have cost that ordering.
func TestSegmentation_UnpausedCRGetsFinalizerBeforeTheBackendCall(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	stub.on(pathSegReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 1, "edges_closed": 0,
		"edge_keys": []string{"a"}, "matched_workloads": 1,
		"unverified_edge_keys": []string{},
	}))
	newSegPolicy(t, ns, "p", v1alpha1.SegmentationPolicySpec{})

	if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "p")); err != nil {
		t.Fatalf("reconcile returned an error: %v", err)
	}
	if !controllerutil.ContainsFinalizer(getSeg(t, ns, "p"), CREdgesFinalizer) {
		t.Fatalf("an unpaused CR must carry %q once the backend has been called, "+
			"or its edges can be orphaned", CREdgesFinalizer)
	}
}
