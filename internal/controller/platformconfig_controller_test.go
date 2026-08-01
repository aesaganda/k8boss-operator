package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8boss.io/operator/api/v1alpha1"
)

func pcReconciler(stub *backendStub) *PlatformConfigReconciler {
	return &PlatformConfigReconciler{Client: k8sC, Backend: stub.client(), ClusterID: 1}
}

func getPC(t *testing.T, name string) *v1alpha1.PlatformConfig {
	t.Helper()
	var out v1alpha1.PlatformConfig
	if err := k8sC.Get(testCtx, client.ObjectKey{Name: name}, &out); err != nil {
		t.Fatalf("re-reading PlatformConfig: %v", err)
	}
	return &out
}

func pausedBody(paused bool) stubResponse {
	return okBody(map[string]any{
		"paused": paused, "updated_at": "2026-08-01T00:00:00Z",
		"updated_by": "k8boss-operator", "ever_set": true,
	})
}

func TestPlatformConfig_HappyPath(t *testing.T) {
	name := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	stub.on(pathPaused, pausedBody(false))
	stub.on(pathProviderHealth, okBody(map[string]any{
		"cluster_id": 1,
		"providers": []map[string]any{
			{"name": "agent", "checked": true, "healthy": true, "message": "ok", "last_query_error": nil},
			{"name": "tetragon", "checked": true, "healthy": true, "message": "HEALTHY",
				"last_query_error": nil, "blockers": []string{}},
		},
	}))

	res, err := pcReconciler(stub).Reconcile(testCtx, req("", name))
	if err != nil {
		t.Fatalf("happy path returned an error: %v", err)
	}
	if res.RequeueAfter != HealthPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, HealthPollInterval)
	}

	got := getPC(t, name)
	assertCondition(t, got.Status.Conditions, ConditionReady, metav1.ConditionTrue, ReasonReconciled)
	assertCondition(t, got.Status.Conditions, ConditionProviderHealthKnown,
		metav1.ConditionTrue, ReasonHealthKnown)
	if len(got.Status.ProviderHealth) != 2 {
		t.Fatalf("providerHealth has %d entries, want 2", len(got.Status.ProviderHealth))
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
}

// A 200 from the kill-switch write that reports a value contradicting the spec
// is not a success — the flag is not confirmed to match the CR.
func TestPlatformConfig_KillSwitchWriteMustBeConfirmed(t *testing.T) {
	name := applyPlatformConfig(t, true, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	stub.on(pathPaused, pausedBody(false)) // asked for paused=true, told paused=false

	if _, err := pcReconciler(stub).Reconcile(testCtx, req("", name)); err == nil {
		t.Fatal("a kill-switch write that did not take effect was reported as a success")
	}
	got := getPC(t, name)
	assertCondition(t, got.Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonWriteNotVerified)
	if got.Status.ObservedGeneration != 0 {
		t.Errorf("observedGeneration advanced to %d on an unconfirmed kill-switch write",
			got.Status.ObservedGeneration)
	}
	// Health must not have been read as if the switch were settled.
	stub.assertNotCalled(pathProviderHealth)
}

// checked=false means "we could not determine this provider's health" and must
// never render as health=False. ProviderHealthStatus.Health is tri-state, so
// an undetermined provider gets a real entry carrying Health=Unknown plus a
// Reason — present in the list, distinguishable from both healthy and
// unhealthy, and never inferred from an absence.
func TestPlatformConfig_UndeterminedProviderIsNotReportedUnhealthy(t *testing.T) {
	name := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	stub.on(pathPaused, pausedBody(false))
	stub.on(pathProviderHealth, okBody(map[string]any{
		"cluster_id": 1,
		"providers": []map[string]any{
			{"name": "agent", "checked": true, "healthy": true, "message": "ok", "last_query_error": nil},
			{"name": "tetragon", "checked": false, "healthy": nil,
				"message":          "Could not reach cluster 1 to check the runtime provider",
				"last_query_error": nil},
		},
	}))

	if _, err := pcReconciler(stub).Reconcile(testCtx, req("", name)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPC(t, name)

	var tetragon *v1alpha1.ProviderHealthStatus
	for i := range got.Status.ProviderHealth {
		if got.Status.ProviderHealth[i].Name == "tetragon" {
			tetragon = &got.Status.ProviderHealth[i]
		}
	}
	if tetragon == nil {
		t.Fatal("the undetermined provider is missing from status.providerHealth entirely — " +
			"a consumer would have to infer its existence from an absence")
	}
	if tetragon.Health != v1alpha1.ProviderHealthUnknown {
		t.Fatalf("an unchecked provider was written into status.providerHealth as health=%q "+
			"— that is a health verdict the control plane never gave", tetragon.Health)
	}
	if tetragon.Reason == "" {
		t.Error("health=Unknown with no Reason: a non-True verdict with no evidence")
	}
	c := assertCondition(t, got.Status.Conditions, ConditionProviderHealthKnown,
		metav1.ConditionFalse, ReasonHealthUndetermined)
	if !strings.Contains(c.Message, "tetragon") {
		t.Errorf("the undetermined provider is not named anywhere a consumer can find it: %q", c.Message)
	}

	// Ready=True is still correct here: the reconcile itself succeeded. Ready
	// must not be read as "all providers healthy" — that is what
	// ProviderHealthKnown is for.
	assertCondition(t, got.Status.Conditions, ConditionReady, metav1.ConditionTrue, ReasonReconciled)
}

// A checked=true, healthy=false provider IS a verdict and must appear in the
// list, with a non-empty explanation rather than an empty field that reads like
// "nothing went wrong".
func TestPlatformConfig_UnhealthyProviderCarriesAnExplanation(t *testing.T) {
	name := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	stub.on(pathPaused, pausedBody(false))
	stub.on(pathProviderHealth, okBody(map[string]any{
		"cluster_id": 1,
		"providers": []map[string]any{
			{"name": "tetragon", "checked": true, "healthy": false, "message": "",
				"last_query_error": nil, "blockers": []string{}},
		},
	}))

	if _, err := pcReconciler(stub).Reconcile(testCtx, req("", name)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPC(t, name)
	if len(got.Status.ProviderHealth) != 1 {
		t.Fatalf("providerHealth has %d entries, want 1", len(got.Status.ProviderHealth))
	}
	e := got.Status.ProviderHealth[0]
	if e.Health != v1alpha1.ProviderHealthFalse {
		t.Fatalf("a healthy=false provider was recorded as health=%q", e.Health)
	}
	if e.LastQueryError == "" {
		t.Error("an unhealthy provider has an empty lastQueryError, which reads as 'nothing went wrong'")
	}
}

// When health cannot be read at all, every provider's health is unknown — not
// unhealthy. status.providerHealth must be left alone rather than filled with
// fabricated entries, and the condition must say the entries are stale.
func TestPlatformConfig_HealthReadFailureDoesNotFabricateEntries(t *testing.T) {
	name := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	stub.on(pathPaused, pausedBody(false))
	stub.on(pathProviderHealth, okBody(map[string]any{
		"cluster_id": 1,
		"providers": []map[string]any{
			{"name": "agent", "checked": true, "healthy": true, "message": "ok", "last_query_error": nil},
		},
	}))
	if _, err := pcReconciler(stub).Reconcile(testCtx, req("", name)); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}

	stub.on(pathProviderHealth, apiError(502, "cluster", "cluster_unreachable", "cluster unreachable"))
	if _, err := pcReconciler(stub).Reconcile(testCtx, req("", name)); err == nil {
		t.Fatal("an unreadable provider-health endpoint produced a successful reconcile")
	}

	got := getPC(t, name)
	if len(got.Status.ProviderHealth) != 1 ||
		got.Status.ProviderHealth[0].Health != v1alpha1.ProviderHealthTrue {
		t.Errorf("providerHealth was rewritten on a failed read: %+v", got.Status.ProviderHealth)
	}
	c := assertCondition(t, got.Status.Conditions, ConditionProviderHealthKnown,
		metav1.ConditionFalse, ReasonHealthUndetermined)
	if !strings.Contains(strings.ToLower(c.Message), "stale") {
		t.Errorf("the condition does not warn that the listed entries are stale: %q", c.Message)
	}
	assertCondition(t, got.Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonHealthUndetermined)
}

func TestPlatformConfig_BackendUnreachableOnKillSwitchPush(t *testing.T) {
	name := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	stub.srv.Close()

	if _, err := pcReconciler(stub).Reconcile(testCtx, req("", name)); err == nil {
		t.Fatal("an unreachable backend produced a successful reconcile")
	}
	got := getPC(t, name)
	c := assertCondition(t, got.Status.Conditions, ConditionReady,
		metav1.ConditionFalse, ReasonBackendUnreachable)
	if !strings.Contains(c.Message, "unknown") {
		t.Errorf("the condition does not say the server-side flag's state is unknown: %q", c.Message)
	}
}

// Regression guard for the one behaviour that makes the server-side flag an
// INDEPENDENT control rather than a mirror of the CR: this reconciler pushes
// spec.paused on every pass, including the periodic health-poll requeue. If
// that is ever intended to stop clobbering an out-of-band server-side pause,
// this test is where the change shows up.
func TestPlatformConfig_PushesSpecPausedOnEveryReconcile(t *testing.T) {
	name := applyPlatformConfig(t, false, v1alpha1.FlowProviderHubble)
	stub := newBackendStub(t)
	stub.on(pathPaused, pausedBody(false))
	stub.on(pathProviderHealth, okBody(map[string]any{"cluster_id": 1, "providers": []map[string]any{}}))

	for i := 0; i < 2; i++ {
		if _, err := pcReconciler(stub).Reconcile(testCtx, req("", name)); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	n := 0
	for _, c := range stub.calls {
		if c == pathPaused {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("PUT /paused was made %d time(s) across 2 reconciles, want 2 — the CR is the "+
			"source of truth for the flag on every pass", n)
	}
}
