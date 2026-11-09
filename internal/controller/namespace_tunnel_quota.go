package controller

import (
	"context"
	"fmt"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// quotaReader deliberately bypasses the manager cache for reservation CAS
// retries. A stale cached resourceVersion cannot safely arbitrate admission.
func (r *AgentEdgeTunnelReconciler) quotaReader() client.Reader {
	if r.APIReader != nil { return r.APIReader }
	return r.Client
}

// ensureQuotaReservation atomically reserves one namespace slot. Existing
// provisioned tunnels are grandfathered when a quota is first introduced;
// new provisioning attempts must fit under the configured limit.
func (r *AgentEdgeTunnelReconciler) ensureQuotaReservation(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, existing bool) error {
	reader := r.quotaReader()
	key := types.NamespacedName{Namespace: t.Namespace, Name: "default"}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var quota agentopsv1alpha1.NamespaceTunnelQuota
		if err := reader.Get(ctx, key, &quota); err != nil {
			if apierrors.IsNotFound(err) { return nil } // no quota means unlimited
			return err
		}

		// Rebuild the baseline from the API server on every CAS attempt. This
		// accounts for tunnels that predate quota creation without counting
		// merely-Pending objects that have not been admitted.
		var tunnels agentopsv1alpha1.AgentEdgeTunnelList
		if err := reader.List(ctx, &tunnels, client.InNamespace(t.Namespace)); err != nil { return err }
		reservations := make(map[string]agentopsv1alpha1.TunnelReservation, len(quota.Status.Reservations)+len(tunnels.Items)+1)
		for _, item := range quota.Status.Reservations {
			if item.UID != "" { reservations[item.UID] = item }
		}
		for i := range tunnels.Items {
			item := &tunnels.Items[i]
			if item.UID != "" && item.Status.TunnelID != "" && item.Status.Phase != agentopsv1alpha1.PhaseTerminated {
				reservations[string(item.UID)] = agentopsv1alpha1.TunnelReservation{Name:item.Name, UID:string(item.UID)}
			}
		}
		uid := string(t.UID)
		if uid == "" { return fmt.Errorf("AgentEdgeTunnel %s/%s has no UID; refusing quota admission", t.Namespace, t.Name) }
		_, alreadyReserved := reservations[uid]
		if !alreadyReserved && !existing && int32(len(reservations)) >= quota.Spec.Limit {
			return fmt.Errorf("namespace tunnel quota exhausted: %d of %d reservations are in use", len(reservations), quota.Spec.Limit)
		}
		reservations[uid] = agentopsv1alpha1.TunnelReservation{Name:t.Name, UID:uid}
		next := make([]agentopsv1alpha1.TunnelReservation, 0, len(reservations))
		for _, reservation := range reservations { next = append(next, reservation) }
		quota.Status.Reservations = next
		return r.Status().Update(ctx, &quota)
	})
}

// releaseQuotaReservation is called only after external resources have been
// removed. Failure keeps the AgentEdgeTunnel finalizer/phase retryable.
func (r *AgentEdgeTunnelReconciler) releaseQuotaReservation(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) error {
	reader := r.quotaReader()
	key := types.NamespacedName{Namespace:t.Namespace, Name:"default"}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var quota agentopsv1alpha1.NamespaceTunnelQuota
		if err := reader.Get(ctx, key, &quota); err != nil {
			if apierrors.IsNotFound(err) { return nil }
			return err
		}
		uid := string(t.UID)
		next := make([]agentopsv1alpha1.TunnelReservation, 0, len(quota.Status.Reservations))
		changed := false
		for _, reservation := range quota.Status.Reservations {
			if reservation.UID == uid { changed = true; continue }
			next = append(next, reservation)
		}
		if !changed { return nil }
		quota.Status.Reservations = next
		return r.Status().Update(ctx, &quota)
	})
}
