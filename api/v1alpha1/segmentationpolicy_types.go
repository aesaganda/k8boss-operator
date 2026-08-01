package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadSelector picks the workloads a policy governs, within the CR's own namespace.
type WorkloadSelector struct {
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// SegmentationRule is a minimal representative shape, not a NetworkPolicy
// reimplementation — Phase 1 (teammate-api) is expected to revisit field
// coverage against real product requirements; this is a compilable contract,
// not a final API (ADR-0003).
type SegmentationRule struct {
	// Ports this rule applies to; empty means all ports.
	Ports []int32 `json:"ports,omitempty"`

	// PeerSelector selects the workloads this rule allows traffic to/from.
	PeerSelector map[string]string `json:"peerSelector,omitempty"`

	// PeerNamespace scopes PeerSelector; empty means the policy's own namespace.
	PeerNamespace string `json:"peerNamespace,omitempty"`
}

type SegmentationPolicySpec struct {
	Selector WorkloadSelector `json:"selector"`

	Ingress []SegmentationRule `json:"ingress,omitempty"`
	Egress  []SegmentationRule `json:"egress,omitempty"`

	// ProviderRef overrides the cluster's PlatformConfig.spec.flowProvider
	// for this policy; empty means use the cluster default.
	ProviderRef string `json:"providerRef,omitempty"`
}

type SegmentationPolicyStatus struct {
	// Conditions surfaces reconcile outcome on every path, including errors.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// EnforcedProvider is set only once the backend control-plane API
	// confirms the enforcement path actually applied this policy — never
	// speculatively set to the requested provider before confirmation
	// (ADR-0003: no Ready=true on unconfirmed state).
	EnforcedProvider string `json:"enforcedProvider,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
type SegmentationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SegmentationPolicySpec   `json:"spec,omitempty"`
	Status SegmentationPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SegmentationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SegmentationPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SegmentationPolicy{}, &SegmentationPolicyList{})
}
