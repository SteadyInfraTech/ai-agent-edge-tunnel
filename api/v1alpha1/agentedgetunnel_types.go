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

// ServiceTarget is a selector-backed ClusterIP Service in the same namespace.
// ExternalName, headless, selectorless, and Services with externalIPs are rejected.
type ServiceTarget struct {
	// Name of a Service in the same namespace as the AgentEdgeTunnel.
	Name string `json:"name"`
	// Port of the Service. Traffic is forwarded as plain HTTP.
	Port int32 `json:"port"`
}

// SessionReference names an AgentSession (group agents.steadyinfra.com) in the
// same namespace as the AgentEdgeTunnel.
type SessionReference struct {
	// Name of the AgentSession.
	Name string `json:"name"`
}

// SecurityConfig defines Zero Trust Access and edge validation rules.
// A tunnel must be protected or explicitly public: set AllowedEmails, or set
// AllowPublic to opt in to an unauthenticated hostname.
type SecurityConfig struct {
	// AllowPublic exposes the hostname without any Cloudflare Access policy.
	// It is an explicit opt-in so that a tunnel is never public by omission,
	// for example for a webhook receiver that must be reachable by a third party.
	AllowPublic bool `json:"allowPublic,omitempty"`

	// AllowedEmails are the identities Cloudflare Access lets through.
	AllowedEmails []string `json:"allowedEmails,omitempty"`
	// ServiceTokens is not implemented yet and is rejected.
	ServiceTokens bool `json:"serviceTokens,omitempty"`
	// WebhookValidation is not implemented yet and is rejected.
	WebhookValidation *WebhookValidation `json:"webhookValidation,omitempty"`
}

// AgentEdgeTunnelSpec defines the desired state of AgentEdgeTunnel
type AgentEdgeTunnelSpec struct {
	// TargetPodSelector is deprecated and ignored; use Service.
	TargetPodSelector map[string]string `json:"targetPodSelector,omitempty"`
	// Service is where the tunnel sends traffic. When set, the operator runs a
	// cloudflared connector for the tunnel. When unset, the tunnel and DNS
	// record are created but nothing is connected to them. It cannot be added
	// or removed after creation.
	Service *ServiceTarget `json:"service,omitempty"`
	// SessionRef exposes the agent an AgentSession is bound to, instead of a
	// fixed Service. It is mutually exclusive with Service. Nothing is created
	// in Cloudflare until the session is Bound, and the tunnel is removed when
	// the session ends or is replaced. It cannot change once the session is
	// bound.
	SessionRef *SessionReference `json:"sessionRef,omitempty"`
	Cloudflare CloudflareConfig  `json:"cloudflare"`
	// TTL is a Go duration such as "2h". The clock starts when the tunnel
	// becomes Active. Empty means no expiry. With SessionRef it is only a hard
	// upper bound: the session's phase ends the tunnel first.
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
	Message string `json:"message,omitempty"`
	// AccessAppID is the Cloudflare Access application protecting the hostname.
	// It is recorded as soon as it exists, for the same reason as TunnelID.
	AccessAppID string `json:"accessAppID,omitempty"`
	// ConnectorReady is true when the cloudflared pod is connected to Cloudflare.
	ConnectorReady bool `json:"connectorReady,omitempty"`
	// SessionUID is the UID of the AgentSession the tunnel was bound to,
	// recorded before anything is created. A session with the same name but a
	// different UID is a different session and ends the tunnel.
	SessionUID string `json:"sessionUID,omitempty"`
	// SessionEndpoint is the session's endpoint at the time of binding. A
	// session bound to a different endpoint later ends the tunnel.
	SessionEndpoint string       `json:"sessionEndpoint,omitempty"`
	ExpiresAt       *metav1.Time `json:"expiresAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.status.hostname`
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Active' || (has(self.spec.ttl) ? self.spec.ttl : '') == (has(oldSelf.spec.ttl) ? oldSelf.spec.ttl : '')",message="spec.ttl cannot change while the tunnel is Active; delete and recreate the object to change it"
// +kubebuilder:validation:XValidation:rule="!(has(self.spec.service) && has(self.spec.sessionRef))",message="spec.service and spec.sessionRef are mutually exclusive"
// +kubebuilder:printcolumn:name="Connected",type=boolean,JSONPath=`.status.connectorReady`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.status.expiresAt`

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
