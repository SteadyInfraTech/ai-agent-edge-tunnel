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
	Name string `json:"name"`
	UID  string `json:"uid"`
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
type NamespaceTunnelQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NamespaceTunnelQuotaSpec   `json:"spec,omitempty"`
	Status            NamespaceTunnelQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type NamespaceTunnelQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NamespaceTunnelQuota `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NamespaceTunnelQuota{}, &NamespaceTunnelQuotaList{})
}
