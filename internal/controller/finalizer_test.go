// Finalizer behaviour for both policy reconcilers.
//
// The invariant under test is the one ADR-0003 §2 states as "time is a property
// of the edge, never deleted": a CR may only be released once the control plane
// has CONFIRMED its graph edges are closed. Releasing it on an unconfirmed or
// failed close orphans edges that go on asserting a policy governs workloads
// after the policy is gone — a wrong security answer that nothing else in the
// system can detect, because the CR that would have explained it no longer
// exists.
//
// These run against a real API server specifically so that "the finalizer is
// held" means the object is genuinely stuck in Terminating, not that a fake
// client kept a field set.
package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"k8boss.io/operator/api/v1alpha1"
)

// reconcileThenDelete drives a CR to Ready (so it carries the finalizer), then
// issues a delete and returns without asserting — the delete blocks on the
// finalizer, so the object is still readable.
func reconcileSegToReady(t *testing.T, stub *backendStub, pc, ns, name string) {
	t.Helper()
	stub.on(pathSegReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 1, "edges_closed": 0,
		"edge_keys": []string{"k"}, "matched_workloads": 1, "unverified_edge_keys": []string{},
	}))
	newSegPolicy(t, ns, name, v1alpha1.SegmentationPolicySpec{
		Selector: v1alpha1.WorkloadSelector{MatchLabels: map[string]string{"app": name}},
	})
	if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, name)); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
}

func TestSegmentation_Finalizer_ClosedThenReleasedOnSuccess(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	reconcileSegToReady(t, stub, pc, ns, "gone")

	stub.on(pathClose, okBody(map[string]any{
		"status": "ok", "edges_closed": 1, "unverified_edge_keys": []string{},
	}))

	cr := getSeg(t, ns, "gone")
	if err := k8sC.Delete(testCtx, cr); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	// Still there, held by the finalizer.
	held := getSeg(t, ns, "gone")
	if held.DeletionTimestamp.IsZero() {
		t.Fatal("deletionTimestamp is unset after a delete — the finalizer is not holding the object")
	}

	if _, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "gone")); err != nil {
		t.Fatalf("finalize reconcile: %v", err)
	}
	if !stub.called(pathClose) {
		t.Fatal("close-edges was never called on deletion — the CR's graph edges would stay open forever")
	}

	var out v1alpha1.SegmentationPolicy
	err := k8sC.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "gone"}, &out)
	if err == nil {
		t.Fatalf("object survived a confirmed close, finalizers=%v", out.Finalizers)
	}
}

// The critical case. A failed close must leave the finalizer in place: the
// alternative is a deleted CR whose ENFORCED_BY edges are still open in the
// graph with nothing left that knows they should not be.
func TestSegmentation_Finalizer_HeldWhenCloseFails(t *testing.T) {
	cases := []struct {
		name       string
		resp       stubResponse
		wantReason string
		wantErr    bool
	}{
		{
			name:       "structured 500",
			resp:       apiError(500, "write_not_verified", "graph_write_unverified", "close did not verify"),
			wantReason: ReasonCloseFailed,
			wantErr:    true,
		},
		{
			// A 200 that does not actually confirm the close. The reconciler
			// must not treat "the request succeeded" as "the edges are closed".
			name: "200 with unverified edge keys",
			resp: okBody(map[string]any{
				"status": "ok", "edges_closed": 0, "unverified_edge_keys": []string{"k1", "k2"},
			}),
			wantReason: ReasonCloseFailed,
			wantErr:    true,
		},
		{
			name: "200 with a non-ok status",
			resp: okBody(map[string]any{
				"status": "partial", "edges_closed": 0, "unverified_edge_keys": []string{},
			}),
			wantReason: ReasonCloseFailed,
			wantErr:    true,
		},
		{
			// Paused blocks cleanup too (contract §3): hold the object, back
			// off, and say so — do not delete it with its edges still open.
			name:       "423 paused",
			resp:       apiError(423, "paused", "operator_paused", "no mutation was applied"),
			wantReason: ReasonPaused,
			wantErr:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
			stub := newBackendStub(t)
			reconcileSegToReady(t, stub, pc, ns, "stuck")
			stub.on(pathClose, tc.resp)

			cr := getSeg(t, ns, "stuck")
			if err := k8sC.Delete(testCtx, cr); err != nil {
				t.Fatalf("deleting: %v", err)
			}

			res, err := segReconciler(stub, pc).Reconcile(testCtx, req(ns, "stuck"))
			if tc.wantErr && err == nil {
				t.Fatal("a failed close returned nil error — the finalize would never be retried")
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("paused close returned an error (%v); it would hot-loop on the "+
						"error backoff against a state only a human can change", err)
				}
				if res.RequeueAfter != PausedRequeue {
					t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, PausedRequeue)
				}
			}

			// The whole point: the object must still exist, still finalized.
			var out v1alpha1.SegmentationPolicy
			if err := k8sC.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "stuck"}, &out); err != nil {
				t.Fatalf("the CR was released despite a failed edge close (%v) — its graph edges "+
					"are now orphaned open with nothing left to close them", err)
			}
			if !controllerutil.ContainsFinalizer(&out, CREdgesFinalizer) {
				t.Fatal("the finalizer was removed on a failed close — graph edges orphaned")
			}
			assertCondition(t, out.Status.Conditions, ConditionReady,
				metav1.ConditionFalse, tc.wantReason)

			// Let the test clean up rather than leaving a wedged object behind.
			stub.on(pathClose, okBody(map[string]any{
				"status": "ok", "edges_closed": 0, "unverified_edge_keys": []string{}}))
			_, _ = segReconciler(stub, pc).Reconcile(testCtx, req(ns, "stuck"))
		})
	}
}

func TestRuntime_Finalizer_HeldWhenCloseFails(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)

	stub.on(pathRuntimeReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 1, "edges_closed": 0, "edge_keys": []string{"k"},
		"matched_workloads": 1, "unverified_edge_keys": []string{}, "provider_installed": true,
	}))
	newRuntimePolicy(t, ns, "rsp", v1alpha1.RuntimeSecurityPolicySpec{
		Selector:         v1alpha1.WorkloadSelector{MatchLabels: map[string]string{"app": "x"}},
		TracingPolicyRef: "deny-shells",
	})
	if _, err := runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp")); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	stub.on(pathClose, apiError(502, "cluster", "cluster_unreachable", "cluster unreachable"))

	cr := getRuntime(t, ns, "rsp")
	if err := k8sC.Delete(testCtx, cr); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp")); err == nil {
		t.Fatal("a failed close returned nil error")
	}

	var out v1alpha1.RuntimeSecurityPolicy
	if err := k8sC.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "rsp"}, &out); err != nil {
		t.Fatalf("the CR was released despite a failed edge close (%v) — MONITORED_BY edges orphaned", err)
	}
	if !controllerutil.ContainsFinalizer(&out, CREdgesFinalizer) {
		t.Fatal("finalizer removed on a failed close")
	}
	assertCondition(t, out.Status.Conditions, ConditionReady, metav1.ConditionFalse, ReasonCloseFailed)

	stub.on(pathClose, okBody(map[string]any{
		"status": "ok", "edges_closed": 1, "unverified_edge_keys": []string{}}))
	_, _ = runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp"))
}

// A CR deleted while the operator was down never got a finalizer, so there is
// nothing to close and nothing to hold. It must not call the backend and must
// not error.
func TestSegmentation_Finalizer_AbsentIsANoOp(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)

	cr := newSegPolicy(t, ns, "nofin", v1alpha1.SegmentationPolicySpec{})
	// Simulate "already terminating with no finalizer" by driving finalize()
	// directly on an object whose deletionTimestamp is set.
	cr.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	if _, err := segReconciler(stub, pc).finalize(testCtx, cr); err != nil {
		t.Fatalf("finalize on a CR with no finalizer returned an error: %v", err)
	}
	stub.assertNotCalled(pathClose)
}
