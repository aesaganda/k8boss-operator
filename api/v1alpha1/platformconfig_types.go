package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FlowProviderKind mirrors k8boss-agent's FLOW_PROVIDER selection
// (k8boss-agent/internal/providers/provider.go New()) — this CRD is the
// k8s-native surface for a choice that is env/YAML-only today.
type FlowProviderKind string

const (
	FlowProviderRHNO     FlowProviderKind = "RHNO"
	FlowProviderHubble   FlowProviderKind = "Hubble"
	FlowProviderCalico   FlowProviderKind = "Calico"
	FlowProviderGoldmane FlowProviderKind = "Goldmane"
	FlowProviderMock     FlowProviderKind = "Mock"
)

// PlatformConfigSpec is cluster-wide K8Boss operator configuration. There is
// exactly one PlatformConfig per cluster (name: "default", enforced by
// Phase 1's admission webhook, not by this type).
type PlatformConfigSpec struct {
	// FlowProvider selects which FlowProvider backs flow telemetry for this cluster.
	// +kubebuilder:validation:Enum=RHNO;Hubble;Calico;Goldmane;Mock
	FlowProvider FlowProviderKind `json:"flowProvider"`

	// KnowledgeGraphConnectionRef points at the Secret holding the backend
	// control-plane API connection the operator's reconcilers call. The
	// operator never holds Postgres credentials and never writes to Postgres
	// directly (ADR-0003) — every graph mutation goes through this API.
	KnowledgeGraphConnectionRef corev1.LocalObjectReference `json:"knowledgeGraphConnectionRef"`

	// Paused halts every reconciler this operator runs (SegmentationPolicy,
	// RuntimeSecurityPolicy) without deleting or deregistering anything they
	// already reconciled. This is the CRD-native half of the previously
	// deferred delivery kill switch — see NOTES.md. Defaults true: an
	// operator with no PlatformConfig applied yet must not silently start
	// mutating cluster state.
	// +kubebuilder:default=true
	Paused bool `json:"paused"`
}

// ProviderHealthStatus mirrors the existing FlowProvider/RuntimeSecurityProvider
// health_check()/last_query_error contract (docs/runbook-unverified-changes.md)
// as a k8s status condition — it is a read-back of that contract, not a new one.
type ProviderHealthStatus struct {
	Name string `json:"name"`

	Healthy bool `json:"healthy"`

	// LastQueryError is empty only when Healthy is true and a query has
	// actually succeeded — never a default-empty value for "haven't checked
	// yet" (ADR-0003 / no-confident-conclusion-from-insufficient-evidence).
	LastQueryError string `json:"lastQueryError,omitempty"`

	ObservedAt metav1.Time `json:"observedAt"`
}

type PlatformConfigStatus struct {
	// Conditions surfaces reconcile outcome, e.g. Type=Ready, Type=Paused.
	// Every reconcile path — including early-return error paths — sets a
	// condition here; none may exit silently (ADR-0003).
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	ProviderHealth []ProviderHealthStatus `json:"providerHealth,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type PlatformConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformConfigSpec   `json:"spec,omitempty"`
	Status PlatformConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PlatformConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlatformConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PlatformConfig{}, &PlatformConfigList{})
}
