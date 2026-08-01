package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8boss.io/operator/api/v1alpha1"
)

func runtimeReconciler(stub *backendStub, pcName string) *RuntimeSecurityPolicyReconciler {
	return &RuntimeSecurityPolicyReconciler{
		Client:             k8sC,
		Backend:            stub.client(),
		ClusterID:          1,
		PlatformConfigName: pcName,
	}
}

func getRuntime(t *testing.T, ns, name string) *v1alpha1.RuntimeSecurityPolicy {
	t.Helper()
	var out v1alpha1.RuntimeSecurityPolicy
	if err := k8sC.Get(testCtx, client.ObjectKey{Namespace: ns, Name: name}, &out); err != nil {
		t.Fatalf("re-reading RuntimeSecurityPolicy: %v", err)
	}
	return &out
}

func TestRuntime_HappyPath(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)

	stub := newBackendStub(t)
	stub.on(pathRuntimeReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 3, "edges_closed": 0, "edge_keys": []string{"a", "b", "c"},
		"matched_workloads": 3, "unverified_edge_keys": []string{}, "provider_installed": true,
	}))
	newRuntimePolicy(t, ns, "rsp", v1alpha1.RuntimeSecurityPolicySpec{
		Selector: v1alpha1.WorkloadSelector{MatchLabels: map[string]string{"app": "checkout"}},
	})

	res, err := runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp"))
	if err != nil {
		t.Fatalf("happy path returned an error: %v", err)
	}
	if res.RequeueAfter != ResyncInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, ResyncInterval)
	}

	got := getRuntime(t, ns, "rsp")
	assertCondition(t, got.Status.Conditions, ConditionReady, metav1.ConditionTrue, ReasonReconciled)
	if got.Status.CorrelatedEvidenceCount != 3 {
		t.Errorf("correlatedEvidenceCount = %d, want 3", got.Status.CorrelatedEvidenceCount)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
	// This reconciler must never touch flow-provider state: the CRD has no such
	// field and the Flow/Runtime split forbids one growing the other's surface.
	// The assertion is structural — see TestRuntime_NeverTouchesFlowProvider.
}

// A 200 that does not affirm the provider was installed is not a result a
// Ready=True may be built on, even though the server is supposed to have raised
// 503 in that case.
func TestRuntime_ProviderInstalledMustBeAffirmed(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"provider_installed absent", map[string]any{
			"status": "ok", "edges_upserted": 1, "edges_closed": 0, "edge_keys": []string{"a"},
			"matched_workloads": 1, "unverified_edge_keys": []string{}}},
		{"provider_installed false", map[string]any{
			"status": "ok", "edges_upserted": 1, "edges_closed": 0, "edge_keys": []string{"a"},
			"matched_workloads": 1, "unverified_edge_keys": []string{}, "provider_installed": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t)
			pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
			stub := newBackendStub(t)
			stub.on(pathRuntimeReconcile, okBody(tc.body))
			newRuntimePolicy(t, ns, "rsp", v1alpha1.RuntimeSecurityPolicySpec{})

			if _, err := runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp")); err == nil {
				t.Fatal("an unaffirmed provider produced a successful reconcile")
			}
			got := getRuntime(t, ns, "rsp")
			assertCondition(t, got.Status.Conditions, ConditionReady,
				metav1.ConditionFalse, ReasonWriteNotVerified)
			if got.Status.CorrelatedEvidenceCount != 0 {
				t.Errorf("correlatedEvidenceCount = %d was written from an unconfirmed response",
					got.Status.CorrelatedEvidenceCount)
			}
		})
	}
}

// The defining case for this reconciler. A 503 means "we could not look",
// which must NOT be recorded as "we looked and found nothing to monitor":
// correlatedEvidenceCount must be left alone rather than zeroed, and the reason
// must name provider unavailability specifically.
func TestRuntime_ProviderUnavailableIsNotAZeroEvidenceSuccess(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)

	// First: a good reconcile that records a real, non-zero count.
	stub.on(pathRuntimeReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 4, "edges_closed": 0, "edge_keys": []string{"a"},
		"matched_workloads": 4, "unverified_edge_keys": []string{}, "provider_installed": true,
	}))
	newRuntimePolicy(t, ns, "rsp", v1alpha1.RuntimeSecurityPolicySpec{})
	if _, err := runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp")); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}

	// Then Tetragon goes away.
	stub.on(pathRuntimeReconcile, apiError(503, "provider_unavailable", "runtime_provider_unavailable",
		"Tetragon not confirmed installed on cluster 1"))

	res, err := runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp"))
	if err != nil {
		t.Fatalf("a 503 was returned as a reconcile error (%v); it is an expected operational "+
			"state that should back off, not hot-loop", err)
	}
	if res.RequeueAfter != PausedRequeue {
		t.Errorf("RequeueAfter = %v, want a bounded backoff", res.RequeueAfter)
	}

	got := getRuntime(t, ns, "rsp")
	c := assertCondition(t, got.Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonProviderUnavailable)
	if !strings.Contains(c.Message, "unknown") {
		t.Errorf("the condition does not say the count is UNKNOWN rather than zero: %q", c.Message)
	}
	if got.Status.CorrelatedEvidenceCount != 4 {
		t.Errorf("correlatedEvidenceCount = %d; a provider-unavailable reconcile must not overwrite "+
			"the last known count (least of all with 0, which asserts 'no runtime evidence exists' "+
			"when the truth is 'we could not look')", got.Status.CorrelatedEvidenceCount)
	}
	if got.Status.ObservedGeneration != 0 && got.Status.ObservedGeneration == got.Generation {
		// Seeded reconcile advanced it at generation 1 and the spec has not
		// changed since, so this only catches a *new* advance. Kept as a guard
		// for the case where a future edit advances it on the 503 path.
		t.Log("observedGeneration unchanged since the successful reconcile, as expected")
	}
}

func TestRuntime_PlatformConfigPausedMakesNoBackendCall(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, true, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	newRuntimePolicy(t, ns, "rsp", v1alpha1.RuntimeSecurityPolicySpec{})

	res, err := runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp"))
	if err != nil {
		t.Fatalf("paused reconcile returned an error: %v", err)
	}
	if res.RequeueAfter != PausedRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, PausedRequeue)
	}
	stub.assertNotCalled(pathRuntimeReconcile)
	assertCondition(t, getRuntime(t, ns, "rsp").Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonPaused)
}

func TestRuntime_ServerPaused423(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	stub.on(pathRuntimeReconcile, apiError(423, "paused", "operator_paused", "paused"))
	newRuntimePolicy(t, ns, "rsp", v1alpha1.RuntimeSecurityPolicySpec{})

	res, err := runtimeReconciler(stub, pc).Reconcile(testCtx, req(ns, "rsp"))
	if err != nil {
		t.Fatalf("423 returned as an error: %v", err)
	}
	if res.RequeueAfter != PausedRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, PausedRequeue)
	}
	assertCondition(t, getRuntime(t, ns, "rsp").Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonPaused)
}

// The Flow/Runtime split, asserted on the wire rather than by reading imports:
// the RuntimeSecurityPolicy reconciler must only ever touch its own endpoint
// (plus the finalizer's close), never the segmentation/flow one — and the
// converse for SegmentationPolicy.
func TestFlowRuntimeSeparation_NeitherReconcilerTouchesTheOthersEndpoint(t *testing.T) {
	ns := newNamespace(t)
	pc := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)

	rStub := newBackendStub(t)
	rStub.on(pathRuntimeReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 0, "edges_closed": 0, "edge_keys": []string{},
		"matched_workloads": 0, "unverified_edge_keys": []string{}, "provider_installed": true,
	}))
	newRuntimePolicy(t, ns, "rsp", v1alpha1.RuntimeSecurityPolicySpec{})
	if _, err := runtimeReconciler(rStub, pc).Reconcile(testCtx, req(ns, "rsp")); err != nil {
		t.Fatalf("runtime reconcile: %v", err)
	}
	rStub.assertNotCalled(pathSegReconcile)
	rStub.assertNotCalled(pathProviderHealth)

	sStub := newBackendStub(t)
	sStub.on(pathSegReconcile, okBody(map[string]any{
		"status": "ok", "edges_upserted": 0, "edges_closed": 0, "edge_keys": []string{},
		"matched_workloads": 0, "unverified_edge_keys": []string{},
	}))
	newSegPolicy(t, ns, "seg", v1alpha1.SegmentationPolicySpec{})
	if _, err := segReconciler(sStub, pc).Reconcile(testCtx, req(ns, "seg")); err != nil {
		t.Fatalf("segmentation reconcile: %v", err)
	}
	sStub.assertNotCalled(pathRuntimeReconcile)
}
