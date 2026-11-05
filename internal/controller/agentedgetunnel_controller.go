package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	cf "github.com/SteadyInfraTech/ai-agent-edge-tunnel/internal/cloudflare"
)

// AgentEdgeTunnelReconciler reconciles an AgentEdgeTunnel object
type AgentEdgeTunnelReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	CloudflareClient cf.TunnelService
}

func (r *AgentEdgeTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var tunnel agentopsv1alpha1.AgentEdgeTunnel
	if err := r.Get(ctx, req.NamespacedName, &tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Check deletion timestamp for finalizer / cleanup
	if !tunnel.ObjectMeta.DeletionTimestamp.IsZero() {
		logger.Info("Cleaning up Cloudflare resources for terminating AgentEdgeTunnel", "name", tunnel.Name)
		if tunnel.Status.TunnelID != "" {
			_ = r.CloudflareClient.DeleteDNSRecord(ctx, tunnel.Spec.Cloudflare.ZoneID, tunnel.Status.Hostname)
			_ = r.CloudflareClient.DeleteTunnel(ctx, tunnel.Status.TunnelID)
		}
		return ctrl.Result{}, nil
	}

	// 2. Active Phase check & TTL handling
	if tunnel.Status.Phase == "Active" {
		if tunnel.Status.ExpiresAt != nil && time.Now().After(tunnel.Status.ExpiresAt.Time) {
			logger.Info("AgentEdgeTunnel reached TTL expiration; marking for cleanup", "name", tunnel.Name)
			tunnel.Status.Phase = "Terminated"
			_ = r.Status().Update(ctx, &tunnel)
			_ = r.CloudflareClient.DeleteDNSRecord(ctx, tunnel.Spec.Cloudflare.ZoneID, tunnel.Status.Hostname)
			_ = r.CloudflareClient.DeleteTunnel(ctx, tunnel.Status.TunnelID)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 3. Provisioning Phase
	logger.Info("Provisioning Cloudflare Edge Tunnel", "name", tunnel.Name)
	info, err := r.CloudflareClient.CreateTunnel(ctx, tunnel.Name)
	if err != nil {
		logger.Error(err, "Failed to create Cloudflare Tunnel")
		tunnel.Status.Phase = "Failed"
		_ = r.Status().Update(ctx, &tunnel)
		return ctrl.Result{}, err
	}

	fullHostname := fmt.Sprintf("%s.agents.yourdomain.com", tunnel.Spec.Cloudflare.Subdomain)
	if err := r.CloudflareClient.CreateDNSRecord(ctx, tunnel.Spec.Cloudflare.ZoneID, fullHostname, info.ID); err != nil {
		logger.Error(err, "Failed to attach Cloudflare DNS record")
		return ctrl.Result{}, err
	}

	// Calculate expiration
	var expiresAt *metav1.Time
	if tunnel.Spec.TTL != "" {
		if duration, err := time.ParseDuration(tunnel.Spec.TTL); err == nil {
			t := metav1.NewTime(time.Now().Add(duration))
			expiresAt = &t
		}
	}

	// Update Status
	tunnel.Status.TunnelID = info.ID
	tunnel.Status.Hostname = fullHostname
	tunnel.Status.Phase = "Active"
	tunnel.Status.ExpiresAt = expiresAt

	if err := r.Status().Update(ctx, &tunnel); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("AgentEdgeTunnel successfully provisioned and active", "hostname", fullHostname)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *AgentEdgeTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentopsv1alpha1.AgentEdgeTunnel{}).
		Complete(r)
}
