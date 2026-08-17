package v1alpha1

import (
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

	// Backend connection is deliberately NOT a field here. It arrives as env
	// (K8BOSS_BACKEND_URL, K8BOSS_OPERATOR_TOKEN via secretKeyRef), which the
	// kubelet resolves — so the operator needs no `get` on secrets at all.
	// A Secret ref on this type would have required cluster-wide secret read
	// (the ref could name any namespace) to service a field nothing reads.
	// The operator still never holds Postgres credentials and never writes to
	// Postgres directly (ADR-0003); every graph mutation goes through the API.

	// Paused halts every reconciler this operator runs (SegmentationPolicy,
	// RuntimeSecurityPolicy) without deleting or deregistering anything they
	// already reconciled. This is the CRD-native half of the previously
	// deferred delivery kill switch. Defaults true: an
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

	// Health is tri-state on purpose. A plain bool cannot say "we could not
	// look", which would force an undetermined provider to assert Unknown as
	// False — the exact conflation this repo separates elsewhere as
	// no_flow_observed vs no_flow_provider_available. Unknown is also the
	// zero value, so a half-populated entry reads as "unknown", never as
	// "healthy".
	// +kubebuilder:validation:Enum=Unknown;True;False
	// +kubebuilder:default=Unknown
	Health ProviderHealth `json:"health"`

	// LastQueryError is empty only when Health is True and a query has
	// actually succeeded — never a default-empty value for "haven't checked
	// yet" (ADR-0003 / no-confident-conclusion-from-insufficient-evidence).
	LastQueryError string `json:"lastQueryError,omitempty"`

	// Reason explains a Health of Unknown or False: which query failed, or
	// why the control plane could not be asked. Required for both — a
	// non-True health with no reason is a verdict with no evidence.
	Reason string `json:"reason,omitempty"`

	ObservedAt metav1.Time `json:"observedAt"`
}

// ProviderHealth is a three-valued health verdict: whether a provider is
// healthy, unhealthy, or could not be determined.
type ProviderHealth string

const (
	// ProviderHealthUnknown means K8Boss could not determine this provider's
	// health — NOT that the provider is unhealthy. Zero value by design.
	ProviderHealthUnknown ProviderHealth = "Unknown"
	ProviderHealthTrue    ProviderHealth = "True"
	ProviderHealthFalse   ProviderHealth = "False"
)

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
