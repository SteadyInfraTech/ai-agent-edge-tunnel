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
	Provider string `json:"provider,omitempty"` // e.g. "github", "stripe"
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
	TTL               string            `json:"ttl,omitempty"` // e.g., "2h"
	CleanupOnFinish   bool              `json:"cleanupOnFinish"`
	Security          SecurityConfig    `json:"security,omitempty"`
}

// AgentEdgeTunnelStatus defines the observed state of AgentEdgeTunnel
type AgentEdgeTunnelStatus struct {
	TunnelID  string      `json:"tunnelID,omitempty"`
	Hostname  string      `json:"hostname,omitempty"`
	Phase     string      `json:"phase,omitempty"` // Pending, Active, Terminated, Failed
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

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

func init() {
	SchemeBuilder.Register(&AgentEdgeTunnel{}, &AgentEdgeTunnelList{})
}
