package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudflareConfig holds Cloudflare-specific metadata for tunnel provisioning
type CloudflareConfig struct {
	ZoneID    string `json:"zoneID"`
	Subdomain string `json:"subdomain"`
}

// WebhookValidation holds HMAC edge validation details
type WebhookValidation struct {
	Provider  string `json:"provider,omitempty"` // e.g. "github", "stripe"
	SecretRef string `json:"secretRef,omitempty"`
}

// SecurityConfig defines Zero Trust Access and edge validation rules
type SecurityConfig struct {
	AllowedEmails     []string           `json:"allowedEmails,omitempty"`
	ServiceTokens     bool               `json:"serviceTokens,omitempty"`
	WebhookValidation *WebhookValidation `json:"webhookValidation,omitempty"`
}

// AgentEdgeTunnelSpec defines the desired state of AgentEdgeTunnel
type AgentEdgeTunnelSpec struct {
	TargetPodSelector map[string]string `json:"targetPodSelector"`
	Cloudflare        CloudflareConfig  `json:"cloudflare"`
	// TTL is a Go duration such as "2h". The clock starts when the tunnel
	// becomes Active. Empty means no expiry.
	TTL string `json:"ttl,omitempty"`
	// CleanupOnFinish is reserved: it is accepted but not acted on yet.
	CleanupOnFinish bool           `json:"cleanupOnFinish"`
	Security        SecurityConfig `json:"security,omitempty"`
}

// Lifecycle phases of an AgentEdgeTunnel.
const (
	// PhasePending: Cloudflare resources are being created.
	PhasePending = "Pending"
	// PhaseActive: the tunnel and DNS record exist.
	PhaseActive = "Active"
	// PhaseTerminated: the TTL expired and the Cloudflare resources were removed.
	// This is final; the object is not re-provisioned.
	PhaseTerminated = "Terminated"
	// PhaseFailed: the last provisioning attempt failed; it is retried with backoff.
	PhaseFailed = "Failed"
)

// AgentEdgeTunnelStatus defines the observed state of AgentEdgeTunnel
type AgentEdgeTunnelStatus struct {
	// TunnelID is recorded as soon as the tunnel exists, before DNS is
	// configured, so a failed reconcile can be retried and cleaned up without
	// creating a second tunnel.
	TunnelID string `json:"tunnelID,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// Phase is one of Pending, Active, Terminated, Failed.
	Phase string `json:"phase,omitempty"`
	// Message describes the last error or transition.
	Message   string       `json:"message,omitempty"`
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.status.hostname`
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Active' || (has(self.spec.ttl) ? self.spec.ttl : '') == (has(oldSelf.spec.ttl) ? oldSelf.spec.ttl : '')",message="spec.ttl cannot change while the tunnel is Active; delete and recreate the object to change it"
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`

// AgentEdgeTunnel is the Schema for the agentedgetunnels API
type AgentEdgeTunnel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentEdgeTunnelSpec   `json:"spec,omitempty"`
	Status AgentEdgeTunnelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentEdgeTunnelList contains a list of AgentEdgeTunnel
type AgentEdgeTunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentEdgeTunnel `json:"items"`
}

// init registers the API types with the scheme builder.
func init() {
	SchemeBuilder.Register(&AgentEdgeTunnel{}, &AgentEdgeTunnelList{})
}
