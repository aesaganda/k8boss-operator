// Package controller holds the operator's three reconcilers, one per CRD.
//
// Deliberate structure: SegmentationPolicy and RuntimeSecurityPolicy live in
// separate files and share NOTHING policy-shaped. What they do share is in
// this file and is generic infrastructure only — condition setting, the
// finalizer name, the PlatformConfig-backed pause gate. This mirrors the
// repo-wide FlowProvider / RuntimeSecurityProvider separation (root
// CLAUDE.md): the two abstractions answer different questions and meet only
// over a shared evidence vocabulary, never by one growing a method because
// the other has it.
package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8boss.io/operator/api/v1alpha1"
)

// CREdgesFinalizer guards the CR until its Knowledge Graph edges have been
// confirmed closed. It is removed ONLY after a 200 from the close endpoint —
// a CR whose close call fails stays around carrying a condition that says
// why, rather than silently orphaning its edges (ADR-0003: "time is a
// property of the edge, never deleted").
const CREdgesFinalizer = "k8boss.io/cr-edges"

// Condition types set by these reconcilers.
const (
	// ConditionReady is true only once the control-plane API has confirmed,
	// in a 200 response, that the intent was actually applied. Never set
	// speculatively on submission (ADR-0003 §2).
	ConditionReady = "Ready"

	// ConditionProviderHealthKnown (PlatformConfig only) is false when at
	// least one provider's health could not be determined. It exists so that
	// "we could not look" never has to be squeezed into a healthy/unhealthy
	// boolean, which would report it as unhealthy.
	ConditionProviderHealthKnown = "ProviderHealthKnown"
)

// Condition reasons. These are stable machine keys; keep them CamelCase per
// the Kubernetes API conventions the CRD schema validates against.
const (
	ReasonReconciled               = "Reconciled"
	ReasonPaused                   = "Paused"
	ReasonPlatformConfigMissing    = "PlatformConfigMissing"
	ReasonPlatformConfigUnreadable = "PlatformConfigUnreadable"
	ReasonBackendUnreachable       = "BackendUnreachable"
	// ReasonAwaitingConfiguration means no control-plane connection was ever
	// configured, so no call was attempted. Kept strictly separate from
	// ReasonBackendUnreachable: one says "we could not reach the backend", the
	// other says "nobody has told us where it is". Reporting the second as the
	// first would blame an outage on a backend that may be perfectly healthy.
	ReasonAwaitingConfiguration = "AwaitingConfiguration"
	ReasonReconcileFailed          = "ReconcileFailed"
	ReasonWriteNotVerified         = "WriteNotVerified"
	ReasonProviderUnavailable      = "ProviderUnavailable"
	ReasonCloseFailed              = "EdgeCloseFailed"
	ReasonHealthUndetermined       = "ProviderHealthUndetermined"
	ReasonHealthKnown              = "ProviderHealthDetermined"
	// ReasonNotSingleton marks a PlatformConfig whose name is not the one this
	// operator answers to. Such an object governs nothing, and says so on
	// itself rather than being silently ignored.
	ReasonNotSingleton = "NotTheConfiguredSingleton"
)

// Requeue cadences.
const (
	// PausedRequeue backs off a call that will keep failing until a human
	// unpauses. Retrying a 423 on the controller's error backoff would hot-loop
	// against an endpoint whose answer cannot change without human action.
	PausedRequeue = 90 * time.Second

	// ResyncInterval re-pushes intent periodically so drift on the backend
	// side (an edge closed out of band, a workload set that changed without a
	// CR write) converges without waiting for a spec edit. The reconcile call
	// is idempotent server-side, so this is cheap and safe.
	ResyncInterval = 10 * time.Minute

	// HealthPollInterval is how often PlatformConfig re-reads provider health.
	HealthPollInterval = 2 * time.Minute

	// UnconfiguredRequeue paces retries while no control-plane connection is
	// configured. Like PausedRequeue, the answer cannot change without human
	// action, so this goes through the requeue path rather than the error
	// path — returning an error would hot-loop the workqueue and fill the log
	// with backoff noise describing a state that is working as designed.
	UnconfiguredRequeue = 5 * time.Minute
)

// unconfiguredMessage renders the condition message for a control-plane call
// that was never attempted because nothing was configured. Shared so all three
// reconcilers tell the user the same thing about how to fix it.
func unconfiguredMessage(what string, err error) string {
	return fmt.Sprintf("%s was not attempted: %v. Point this operator at a K8Boss control plane by "+
		"setting K8BOSS_BACKEND_URL, K8BOSS_CLUSTER_ID and K8BOSS_OPERATOR_TOKEN on its Deployment "+
		"— under OLM, via the Subscription's spec.config.env and a Secret named k8boss-operator "+
		"(key: token) in the operator's namespace.", what, err)
}

// setCondition stamps a condition, carrying the CR's generation so a reader
// can tell whether the condition describes the spec they are looking at.
func setCondition(conds *[]metav1.Condition, generation int64,
	condType string, status metav1.ConditionStatus, reason, message string) {

	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// pauseGate is the CRD-native half of the kill switch (ADR-0003 §4). The
// server enforces its own persisted flag independently on every mutating
// call; this gate exists so a paused operator does not even make the call.
type pauseGate struct {
	// Paused is true whenever we are not affirmatively sure it is safe to
	// mutate. Note the asymmetry: "could not read PlatformConfig" resolves to
	// paused, never to "assume unpaused" — an operator that cannot confirm the
	// switch is off must not act as if it were off.
	Paused bool

	Reason  string
	Message string

	// FlowProvider is the cluster default from PlatformConfig.spec, only
	// meaningful when Paused is false (i.e. we actually read the config).
	FlowProvider string
}

// checkPauseGate reads the singleton cluster-scoped PlatformConfig.
//
// Three ways to end up paused, each reported with its own reason so the
// condition says which question we failed to answer:
//   - the config exists and says paused           -> ReasonPaused
//   - no config has been applied yet              -> ReasonPlatformConfigMissing
//   - the config could not be read at all         -> ReasonPlatformConfigUnreadable
//
// +kubebuilder:rbac:groups=k8boss.io,resources=platformconfigs,verbs=get;list;watch
func checkPauseGate(ctx context.Context, c client.Client, name string) pauseGate {
	var pc v1alpha1.PlatformConfig
	err := c.Get(ctx, types.NamespacedName{Name: name}, &pc)

	switch {
	case apierrors.IsNotFound(err):
		return pauseGate{
			Paused: true,
			Reason: ReasonPlatformConfigMissing,
			Message: fmt.Sprintf("No PlatformConfig %q exists yet; the kill switch defaults to "+
				"paused, so nothing was reconciled. Apply a PlatformConfig with spec.paused=false "+
				"to enable reconciliation.", name),
		}
	case err != nil:
		return pauseGate{
			Paused: true,
			Reason: ReasonPlatformConfigUnreadable,
			Message: fmt.Sprintf("Could not read PlatformConfig %q, so we cannot confirm the kill "+
				"switch is off; treating as paused and reconciling nothing: %v", name, err),
		}
	case pc.Spec.Paused:
		return pauseGate{
			Paused: true,
			Reason: ReasonPaused,
			Message: fmt.Sprintf("PlatformConfig %q has spec.paused=true; no mutation was attempted. "+
				"Set spec.paused=false to resume.", name),
		}
	}

	return pauseGate{Paused: false, FlowProvider: string(pc.Spec.FlowProvider)}
}
