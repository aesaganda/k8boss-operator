package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuntimeSecurityPolicySpec deliberately does not reimplement Tetragon's
// policy language. The K8Boss control plane's RuntimeSecurityProvider stays
// the sole contract for runtime enforcement (ADR-0003, mirrors the
// FlowProvider/RuntimeSecurityProvider separation invariant) — this CRD
// attaches K8Boss Knowledge Graph evidence to an existing Tetragon
// TracingPolicy, it does not replace it. This reconciler must never import
// or call anything under the FlowProvider path.
type RuntimeSecurityPolicySpec struct {
	Selector WorkloadSelector `json:"selector"`

	// TracingPolicyRef names an existing Tetragon TracingPolicy in this
	// namespace that this CR correlates Knowledge Graph evidence against.
	TracingPolicyRef string `json:"tracingPolicyRef,omitempty"`

	// DeniedBinaries is a minimal inline rule for the common case —
	// forwarded to RuntimeSecurityProvider as-is, not interpreted here.
	DeniedBinaries []string `json:"deniedBinaries,omitempty"`
}

type RuntimeSecurityPolicyStatus struct {
	// Conditions surfaces reconcile outcome on every path, including errors.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CorrelatedEvidenceCount is how many runtime events the backend has
	// actually attached to this CR's Knowledge Graph edges so far — 0 means
	// either "none yet" or "correlation is failing"; Conditions must say
	// which (no-confident-conclusion-from-insufficient-evidence).
	CorrelatedEvidenceCount int64 `json:"correlatedEvidenceCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
type RuntimeSecurityPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RuntimeSecurityPolicySpec   `json:"spec,omitempty"`
	Status RuntimeSecurityPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type RuntimeSecurityPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeSecurityPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimeSecurityPolicy{}, &RuntimeSecurityPolicyList{})
}
