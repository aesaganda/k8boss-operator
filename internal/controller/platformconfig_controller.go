package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"k8boss.io/operator/api/v1alpha1"
	"k8boss.io/operator/internal/backendclient"
)

// PlatformConfigReconciler owns the cluster-scoped singleton: it pushes
// spec.paused at the control plane's persisted kill switch and reads provider
// health back into status.
//
// It has no finalizer. Nothing per-instance is written to the Knowledge Graph
// by this CR, so there are no edges to close on delete. (The persisted paused
// flag is deliberately NOT reset on delete: deleting the config must not
// silently unpause the API.)
type PlatformConfigReconciler struct {
	client.Client

	Backend *backendclient.Client

	// ClusterID identifies this cluster to the control-plane API.
	ClusterID int

	// PlatformConfigName is the singleton this operator answers to — the same
	// name the SegmentationPolicy and RuntimeSecurityPolicy reconcilers consult
	// through checkPauseGate. Without it this controller reconciled EVERY
	// PlatformConfig object regardless of name and pushed each one's
	// spec.paused to the cluster-wide server-side kill switch, while the pause
	// gate those other controllers read only ever looks at the singleton. So a
	// second CR named anything at all could unpause the control-plane API for
	// the whole cluster, and the operator's own gate would never notice.
	PlatformConfigName string
}

// +kubebuilder:rbac:groups=k8boss.io,resources=platformconfigs,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=k8boss.io,resources=platformconfigs/status,verbs=get;update

func (r *PlatformConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cr v1alpha1.PlatformConfig
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 0. Only the singleton drives anything.
	//
	// Rejected here rather than as a watch predicate on purpose: a predicate
	// drops the object silently, leaving a second CR sitting in the cluster with
	// empty status and no explanation, looking like the operator is broken. This
	// way the object says, on itself, that it governs nothing.
	if r.PlatformConfigName != "" && cr.Name != r.PlatformConfigName {
		logger.Info("ignoring non-singleton PlatformConfig",
			"name", cr.Name, "singleton", r.PlatformConfigName)
		return ctrl.Result{}, r.setNotSingleton(ctx, &cr)
	}

	// 1. Push the kill switch. This endpoint is never itself gated by the
	//    flag — otherwise a paused operator could not unpause itself.
	pausedState, err := r.Backend.SetPaused(ctx, cr.Spec.Paused, "k8boss-operator/PlatformConfig/"+cr.Name)
	if err != nil {
		return r.notReady(ctx, &cr, pauseErrorReason(err),
			fmt.Sprintf("could not push spec.paused=%t to the control-plane kill switch, so the "+
				"server-side flag's state is unknown and may not match this spec: %s",
				cr.Spec.Paused, describeErr(err)))
	}
	if pausedState.Paused != cr.Spec.Paused {
		// A 200 that did not take effect is not a success.
		return r.notReady(ctx, &cr, ReasonWriteNotVerified,
			fmt.Sprintf("the control plane accepted the kill-switch write but reports paused=%t "+
				"while spec.paused=%t; the flag is not confirmed to match this spec.",
				pausedState.Paused, cr.Spec.Paused))
	}

	// 2. Read provider health back. Read-only, so it runs regardless of pause.
	health, err := r.Backend.ProviderHealth(ctx, r.ClusterID)
	if err != nil {
		// We could not look at all. Every provider's health is unknown —
		// which is emphatically not "every provider is unhealthy", so
		// status.providerHealth is left untouched rather than filled with
		// fabricated false entries.
		setCondition(&cr.Status.Conditions, cr.Generation, ConditionProviderHealthKnown,
			metav1.ConditionFalse, ReasonHealthUndetermined,
			fmt.Sprintf("Provider health could not be read from the control-plane API, so the "+
				"health of every provider is UNKNOWN (not unhealthy). Any entries in "+
				"status.providerHealth are stale from an earlier successful read: %s",
				describeErr(err)))
		return r.notReady(ctx, &cr, healthErrorReason(err),
			fmt.Sprintf("kill switch is confirmed at paused=%t, but provider health could not be "+
				"determined: %s", pausedState.Paused, describeErr(err)))
	}

	entries, undetermined := splitProviderHealth(health.Providers)

	// Every provider the control plane reported gets an entry, including the
	// ones whose health could not be determined — those carry
	// Health=Unknown with a Reason. status.providerHealth is therefore a
	// complete list, and a consumer never has to infer meaning from a
	// provider's absence.
	cr.Status.ProviderHealth = entries

	if len(undetermined) > 0 {
		setCondition(&cr.Status.Conditions, cr.Generation, ConditionProviderHealthKnown,
			metav1.ConditionFalse, ReasonHealthUndetermined,
			fmt.Sprintf("Health is UNKNOWN (not unhealthy) for %d provider(s): %s. These are "+
				"listed in status.providerHealth with health=Unknown, not as unhealthy — K8Boss "+
				"could not determine their health, which is a different answer from determining "+
				"they are unhealthy.", len(undetermined), strings.Join(undetermined, "; ")))
	} else {
		setCondition(&cr.Status.Conditions, cr.Generation, ConditionProviderHealthKnown,
			metav1.ConditionTrue, ReasonHealthKnown,
			fmt.Sprintf("Health was determined for all %d provider(s).", len(entries)))
	}

	// 3. Ready reflects only what was actually confirmed: the kill switch
	//    matches spec, and a health read completed. It does NOT claim every
	//    provider is healthy — that is what ProviderHealthKnown plus the
	//    per-provider entries are for.
	cr.Status.ObservedGeneration = cr.Generation
	setCondition(&cr.Status.Conditions, cr.Generation, ConditionReady,
		metav1.ConditionTrue, ReasonReconciled,
		fmt.Sprintf("Control plane confirmed the kill switch at paused=%t; provider health read "+
			"returned %d provider(s), %d of them undetermined.",
			pausedState.Paused, len(entries), len(undetermined)))

	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after successful reconcile: %w", err)
	}
	logger.Info("reconciled PlatformConfig", "paused", pausedState.Paused,
		"providers", len(entries), "undeterminedProviders", len(undetermined))
	return ctrl.Result{RequeueAfter: HealthPollInterval}, nil
}

// splitProviderHealth maps every reported provider to a status entry —
// Health True/False for a real verdict, Unknown for one we could not
// determine. The second return lists the undetermined ones as human-readable
// "name: why we could not tell" strings, for the condition message.
func splitProviderHealth(providers []backendclient.ProviderHealth) ([]v1alpha1.ProviderHealthStatus, []string) {
	now := metav1.Now()
	entries := make([]v1alpha1.ProviderHealthStatus, 0, len(providers))
	var undetermined []string

	for _, p := range providers {
		if !p.Determinate() {
			why := p.Message
			if why == "" {
				if !p.Checked {
					why = "the control plane reported checked=false with no explanation"
				} else {
					why = "the control plane reported checked=true but returned no healthy verdict"
				}
			}
			undetermined = append(undetermined, fmt.Sprintf("%s: %s", p.Name, why))
			// Still emit an entry. Omitting the provider would leave a consumer
			// reading status.providerHealth with a shorter list and no way to
			// tell a provider we could not reach from one that was never
			// configured — the absence would read as "these are all of them".
			// Health=Unknown says which question failed.
			entries = append(entries, v1alpha1.ProviderHealthStatus{
				Name:       p.Name,
				Health:     v1alpha1.ProviderHealthUnknown,
				Reason:     why,
				ObservedAt: now,
			})
			continue
		}

		entry := v1alpha1.ProviderHealthStatus{
			Name:       p.Name,
			Health:     v1alpha1.ProviderHealthFalse,
			ObservedAt: now,
		}
		if *p.Healthy {
			entry.Health = v1alpha1.ProviderHealthTrue
		}
		// LastQueryError may only be empty when the provider is healthy and a
		// query actually succeeded (v1alpha1.ProviderHealthStatus's own
		// contract). An unhealthy provider with no reported query error still
		// gets a non-empty explanation rather than an empty field that reads
		// like "nothing went wrong".
		switch {
		case p.LastQueryError != nil && *p.LastQueryError != "":
			entry.LastQueryError = *p.LastQueryError
		case entry.Health != v1alpha1.ProviderHealthTrue && p.Message != "":
			entry.LastQueryError = p.Message
		case entry.Health != v1alpha1.ProviderHealthTrue:
			entry.LastQueryError = "reported unhealthy; the control plane returned no query error " +
				"and no message"
		}
		if len(p.Blockers) > 0 {
			blockers := "blockers: " + strings.Join(p.Blockers, ", ")
			if entry.LastQueryError == "" {
				entry.LastQueryError = blockers
			} else {
				entry.LastQueryError += " (" + blockers + ")"
			}
		}
		entries = append(entries, entry)
	}
	return entries, undetermined
}

// notReady records Ready=False and returns the error so the controller backs
// off. observedGeneration is not advanced.
func (r *PlatformConfigReconciler) notReady(ctx context.Context, cr *v1alpha1.PlatformConfig,
	reason, message string) (ctrl.Result, error) {

	setCondition(&cr.Status.Conditions, cr.Generation, ConditionReady,
		metav1.ConditionFalse, reason, message)
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status (%s): %w", reason, err)
	}
	return ctrl.Result{}, fmt.Errorf("%s: %s", reason, message)
}

// setNotSingleton records, on the object itself, that it drives nothing —
// distinct from notReady(), which returns an error to trigger controller
// backoff. There is nothing to retry here: the verdict cannot change without a
// human renaming the object.
func (r *PlatformConfigReconciler) setNotSingleton(ctx context.Context,
	cr *v1alpha1.PlatformConfig) error {

	setCondition(&cr.Status.Conditions, cr.Generation, ConditionReady,
		metav1.ConditionFalse, ReasonNotSingleton,
		fmt.Sprintf("This PlatformConfig is named %q, but this operator answers only to %q. "+
			"It governs nothing: its spec.paused is NOT pushed to the control-plane kill switch "+
			"and its providers are not evaluated. Rename it or delete it.",
			cr.Name, r.PlatformConfigName))
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("updating status (%s): %w", ReasonNotSingleton, err)
	}
	return nil
}

func pauseErrorReason(err error) string {
	var apiErr *backendclient.APIError
	if errors.As(err, &apiErr) {
		return ReasonReconcileFailed
	}
	return ReasonBackendUnreachable
}

func healthErrorReason(err error) string {
	var apiErr *backendclient.APIError
	if errors.As(err, &apiErr) {
		return ReasonHealthUndetermined
	}
	return ReasonBackendUnreachable
}

// describeErr renders the API's stable {category, code, message} keys when we
// have them, so a condition message names the specific failure rather than
// saying "request failed".
func describeErr(err error) string {
	var apiErr *backendclient.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("%s — %s", apiErr.Code, apiErr.Message)
	}
	return err.Error()
}

// SetupWithManager registers this reconciler with the manager.
func (r *PlatformConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// GenerationChangedPredicate breaks a self-driving loop. Reconcile
		// rewrites status on every pass (including a fresh lastChecked
		// timestamp), and this controller watches its own object — so each write
		// re-enqueued the object, which reconciled, which wrote status again.
		// The result was a continuous spin against the control-plane API's
		// /paused and /provider-health endpoints, at whatever rate the work
		// queue allowed rather than the HealthPollInterval it was meant to run
		// at.
		//
		// Safe because status-subresource writes do not bump
		// metadata.generation, so only real spec edits re-trigger — and Create
		// and Delete still pass the predicate. Periodic health polling is
		// unaffected: it comes from ctrl.Result{RequeueAfter: HealthPollInterval}
		// in Reconcile, not from this watch.
		For(&v1alpha1.PlatformConfig{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("platformconfig").
		Complete(r)
}
