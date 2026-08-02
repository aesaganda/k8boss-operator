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

	// ResolvedFlowProvider records which FlowProvider this policy was
	// reconciled under: spec.providerRef when set, otherwise the cluster
	// default read from PlatformConfig at reconcile time. That last part is
	// why it is worth a status field at all rather than being derivable from
	// the spec — editing PlatformConfig later does not rewrite it, so it says
	// what was in effect for THIS reconcile.
	//
	// It is NOT a claim that anything is enforcing this policy. A FlowProvider
	// is flow *telemetry* (Hubble/Calico/RHNO); enforcement is the CNI's job
	// and K8Boss does not observe it. This field was called `enforcedProvider`
	// and did read as that claim — see NOTES.md for what real enforcement
	// confirmation would require.
	//
	// Set only on the confirmed-success path, so an unverified write leaves it
	// empty rather than stamping intent (ADR-0003).
	ResolvedFlowProvider string `json:"resolvedFlowProvider,omitempty"`
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
