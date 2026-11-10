package controller

import (
	"context"
	"errors"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
)

var errUnsafeServiceTarget = errors.New("unsafe Service target")

// validateServiceTarget verifies that a connector target is a selector-backed,
// ClusterIP Service whose EndpointSlices resolve only to matching Pods in the
// AgentEdgeTunnel's own namespace. EndpointSlices are user-writable resources,
// so checking the Service selector alone is not sufficient.
func (r *AgentEdgeTunnelReconciler) validateServiceTarget(ctx context.Context, tunnel *agentopsv1alpha1.AgentEdgeTunnel) error {
	targetService, err := target(tunnel)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnsafeServiceTarget, err)
	}
	if targetService == nil {
		return nil
	}

	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}

	key := client.ObjectKey{Namespace: tunnel.Namespace, Name: targetService.Name}
	var service corev1.Service
	if err := reader.Get(ctx, key, &service); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: Service %s/%s does not exist", errUnsafeServiceTarget, tunnel.Namespace, targetService.Name)
		}
		return fmt.Errorf("reading Service %s/%s: %w", tunnel.Namespace, targetService.Name, err)
	}
	if service.Spec.Type == corev1.ServiceTypeExternalName {
		return fmt.Errorf("%w: Service %s/%s is ExternalName", errUnsafeServiceTarget, tunnel.Namespace, service.Name)
	}
	if service.Spec.ClusterIP == "" {
		return fmt.Errorf("%w: Service %s/%s has no ClusterIP", errUnsafeServiceTarget, tunnel.Namespace, service.Name)
	}
	if service.Spec.ClusterIP == corev1.ClusterIPNone {
		return fmt.Errorf("%w: Service %s/%s is headless", errUnsafeServiceTarget, tunnel.Namespace, service.Name)
	}
	if len(service.Spec.Selector) == 0 {
		return fmt.Errorf("%w: Service %s/%s has no Pod selector", errUnsafeServiceTarget, tunnel.Namespace, service.Name)
	}

	portFound := false
	for _, port := range service.Spec.Ports {
		if port.Port == targetService.Port && (port.Protocol == "" || port.Protocol == corev1.ProtocolTCP) {
			portFound = true
			break
		}
	}
	if !portFound {
		return fmt.Errorf("%w: Service %s/%s does not expose TCP port %d", errUnsafeServiceTarget, tunnel.Namespace, service.Name, targetService.Port)
	}

	var slices discoveryv1.EndpointSliceList
	if err := reader.List(ctx, &slices, client.InNamespace(tunnel.Namespace), client.MatchingLabels{
		discoveryv1.LabelServiceName: service.Name,
	}); err != nil {
		return fmt.Errorf("listing EndpointSlices for Service %s/%s: %w", tunnel.Namespace, service.Name, err)
	}

	selector := labels.Set(service.Spec.Selector).AsSelectorPreValidated()
	for i := range slices.Items {
		slice := &slices.Items[i]
		if len(slice.Endpoints) == 0 {
			continue
		}
		if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
			return fmt.Errorf("%w: EndpointSlice %s/%s uses unsupported address type %q", errUnsafeServiceTarget, tunnel.Namespace, slice.Name, slice.AddressType)
		}
		for _, endpoint := range slice.Endpoints {
			ref := endpoint.TargetRef
			if ref == nil || ref.Kind != "Pod" || ref.Name == "" || (ref.Namespace != "" && ref.Namespace != tunnel.Namespace) {
				return fmt.Errorf("%w: EndpointSlice %s/%s contains an endpoint that is not a Pod in namespace %q", errUnsafeServiceTarget, tunnel.Namespace, slice.Name, tunnel.Namespace)
			}
			var pod corev1.Pod
			podKey := client.ObjectKey{Namespace: tunnel.Namespace, Name: ref.Name}
			if err := reader.Get(ctx, podKey, &pod); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("%w: EndpointSlice %s/%s references missing Pod %s/%s", errUnsafeServiceTarget, tunnel.Namespace, slice.Name, tunnel.Namespace, ref.Name)
				}
				return fmt.Errorf("reading endpoint Pod %s/%s: %w", tunnel.Namespace, ref.Name, err)
			}
			if (ref.UID != "" && ref.UID != pod.UID) || pod.Spec.HostNetwork || !selector.Matches(labels.Set(pod.Labels)) {
				return fmt.Errorf("%w: EndpointSlice %s/%s references a Pod outside the Service selector or using host networking", errUnsafeServiceTarget, tunnel.Namespace, slice.Name)
			}
			if len(endpoint.Addresses) == 0 {
				return fmt.Errorf("%w: EndpointSlice %s/%s contains an endpoint without an address", errUnsafeServiceTarget, tunnel.Namespace, slice.Name)
			}
			for _, address := range endpoint.Addresses {
				ip := net.ParseIP(address)
				if ip == nil || (slice.AddressType == discoveryv1.AddressTypeIPv4 && ip.To4() == nil) || (slice.AddressType == discoveryv1.AddressTypeIPv6 && ip.To4() != nil) || !podHasIP(&pod, ip) {
					return fmt.Errorf("%w: EndpointSlice %s/%s address %q does not belong to its referenced Pod", errUnsafeServiceTarget, tunnel.Namespace, slice.Name, address)
				}
			}
		}
	}
	return nil
}

func podHasIP(pod *corev1.Pod, want net.IP) bool {
	if pod.Status.PodIP != "" {
		if got := net.ParseIP(pod.Status.PodIP); got != nil && got.Equal(want) {
			return true
		}
	}
	for _, podIP := range pod.Status.PodIPs {
		if got := net.ParseIP(podIP.IP); got != nil && got.Equal(want) {
			return true
		}
	}
	return false
}

// requestsForService enqueues tunnels whose current Service target matches a
// changed Service or EndpointSlice in the same namespace.
func (r *AgentEdgeTunnelReconciler) requestsForService(ctx context.Context, namespace, serviceName string) []reconcile.Request {
	if namespace == "" || serviceName == "" {
		return nil
	}
	var tunnels agentopsv1alpha1.AgentEdgeTunnelList
	if err := r.List(ctx, &tunnels, client.InNamespace(namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range tunnels.Items {
		targetService, err := target(&tunnels.Items[i])
		if err != nil || targetService == nil || targetService.Name != serviceName {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tunnels.Items[i])})
	}
	return requests
}

func (r *AgentEdgeTunnelReconciler) mapServiceEvent(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.requestsForService(ctx, obj.GetNamespace(), obj.GetName())
}

func (r *AgentEdgeTunnelReconciler) mapEndpointSliceEvent(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.requestsForService(ctx, obj.GetNamespace(), obj.GetLabels()[discoveryv1.LabelServiceName])
}
