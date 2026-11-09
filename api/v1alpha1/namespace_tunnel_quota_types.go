package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NamespaceTunnelQuotaSpec sets the maximum number of live AgentEdgeTunnels
// admitted in this namespace. A missing quota object means unlimited capacity.
type NamespaceTunnelQuotaSpec struct {
	// Limit is the maximum number of reservations. Zero freezes new admissions.
	// +kubebuilder:validation:Minimum=0
	Limit int32 `json:"limit"`
}

// TunnelReservation records one AgentEdgeTunnel UID. UIDs, rather than names,
// prevent a deleted-and-recreated object from inheriting an old reservation.
type TunnelReservation struct {
	// Name is the AgentEdgeTunnel name associated with this reservation.
	Name string `json:"name"`
	// UID uniquely identifies the AgentEdgeTunnel across deletion and recreation.
	UID string `json:"uid"`
}

// NamespaceTunnelQuotaStatus is maintained exclusively by the operator.
type NamespaceTunnelQuotaStatus struct {
	// Reservations includes every provisioned tunnel and every admitted
	// provisioning attempt. Entries remain until external cleanup succeeds.
	// +listType=map
	// +listMapKey=uid
	Reservations []TunnelReservation `json:"reservations,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Limit",type=integer,JSONPath=`.spec.limit`
// NamespaceTunnelQuota is the per-namespace limit and reservation ledger.
type NamespaceTunnelQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec defines the maximum number of admitted tunnels in this namespace.
	Spec NamespaceTunnelQuotaSpec `json:"spec,omitempty"`
	// Status stores reservations maintained by the operator.
	Status NamespaceTunnelQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// NamespaceTunnelQuotaList contains NamespaceTunnelQuota resources.
type NamespaceTunnelQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items is the list of namespace tunnel quotas.
	Items []NamespaceTunnelQuota `json:"items"`
}

// init registers the namespace quota API types with the scheme builder.
func init() {
	SchemeBuilder.Register(&NamespaceTunnelQuota{}, &NamespaceTunnelQuotaList{})
}
