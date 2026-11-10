package controller

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentopsv1alpha1 "github.com/SteadyInfraTech/ai-agent-edge-tunnel/api/v1alpha1"
)

// agentSessionGVK is the AgentSession kind of agentops-operator. It is read as
// unstructured, so this operator does not import that project's Go types: the
// fields read below are the whole contract (see docs/design/edge-exposure.md in
// agentops-operator).
var agentSessionGVK = schema.GroupVersionKind{Group: "agents.steadyinfra.com", Version: "v1alpha1", Kind: "AgentSession"}

// sessionPollInterval is how often a session is re-read. The session kind is
// not watched, so this operator starts without agentops-operator installed.
const sessionPollInterval = 10 * time.Second

// hasConnector reports whether the operator runs a cloudflared connector for t.
func hasConnector(t *agentopsv1alpha1.AgentEdgeTunnel) bool {
	return t.Spec.Service != nil || t.Spec.SessionRef != nil
}

// target returns the Service the connector forwards to: spec.service, or the
// endpoint recorded from the session when the tunnel was bound. It returns nil
// when there is no connector.
func target(t *agentopsv1alpha1.AgentEdgeTunnel) (*agentopsv1alpha1.ServiceTarget, error) {
	switch {
	case t.Spec.Service != nil:
		return t.Spec.Service, nil
	case t.Spec.SessionRef != nil:
		return parseEndpoint(t.Status.SessionEndpoint, t.Namespace)
	}
	return nil, nil
}

// parseEndpoint turns a session endpoint, http://<service>.<namespace>.svc:<port>,
// into a ServiceTarget. The endpoint is interpolated into the connector's
// config, and a session could otherwise point a tunnel at any host, so only a
// Service in the tunnel's own namespace is accepted.
func parseEndpoint(endpoint, namespace string) (*agentopsv1alpha1.ServiceTarget, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("session endpoint %q is not a plain http://<service>.<namespace>.svc:<port> URL", endpoint)
	}
	labels := strings.Split(u.Hostname(), ".")
	if len(labels) != 3 || labels[2] != "svc" || labels[1] != namespace || len(validation.IsDNS1035Label(labels[0])) > 0 {
		return nil, fmt.Errorf("session endpoint %q must name a Service in namespace %q", endpoint, namespace)
	}
	port := 80
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return nil, fmt.Errorf("session endpoint %q has an invalid port", endpoint)
		}
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("session endpoint %q has an invalid port", endpoint)
	}
	return &agentopsv1alpha1.ServiceTarget{Name: labels[0], Port: int32(port)}, nil
}

// reconcileSession ties a tunnel with spec.sessionRef to its AgentSession.
//
//   - Until the session is Bound nothing is created in Cloudflare.
//   - When it is first seen Bound, its UID and endpoint are recorded in status
//     before anything is created, and both are checked on every pass. A session
//     deleted and recreated under the same name has a different UID, so it
//     cannot inherit the old tunnel and its Access policy.
//   - Completed, Expired, Failed, deleted, replaced, or bound to a different
//     endpoint: the tunnel is removed and ends as Terminated.
//
// done reports that Reconcile must return res and err as they are.
func (r *AgentEdgeTunnelReconciler) reconcileSession(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel) (res ctrl.Result, done bool, err error) {
	if t.Spec.SessionRef == nil {
		return ctrl.Result{}, false, nil
	}
	pinned := t.Status.SessionUID != ""

	if !pinned {
		// Report a bad spec now, not only once the session happens to bind.
		if _, verr := validate(t); verr != nil {
			if t.Status.Phase != agentopsv1alpha1.PhaseFailed || t.Status.Message != verr.Error() {
				t.Status.Phase = agentopsv1alpha1.PhaseFailed
				t.Status.Message = verr.Error()
				return ctrl.Result{}, true, r.Status().Update(ctx, t)
			}
			return ctrl.Result{}, true, nil
		}
	}

	session := &unstructured.Unstructured{}
	session.SetGroupVersionKind(agentSessionGVK)
	err = r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Spec.SessionRef.Name}, session)
	switch {
	case apierrors.IsNotFound(err):
		if pinned {
			return r.terminateForSession(ctx, t, "session was deleted")
		}
		return r.waitForSession(ctx, t, fmt.Sprintf("waiting for AgentSession %q to exist", t.Spec.SessionRef.Name))
	case meta.IsNoMatchError(err):
		// The AgentSession CRD is not installed. That says nothing about a
		// session that was bound, so never end a tunnel because of it. However,
		// the missing optional API must not suspend independent TTL, Service,
		// Access-policy, or connector safeguards for an already-active tunnel.
		if pinned && t.Status.Phase == agentopsv1alpha1.PhaseActive {
			log.FromContext(ctx).Error(err, "AgentSession CRD is unavailable; continuing active tunnel safeguards", "session", t.Spec.SessionRef.Name)
			res, activeErr := r.reconcileActive(ctx, t)
			return res, true, activeErr
		}
		return ctrl.Result{}, true, fmt.Errorf("the AgentSession kind %s is not installed: %w", agentSessionGVK.GroupKind(), err)
	case err != nil:
		return ctrl.Result{}, true, err
	}

	if pinned && string(session.GetUID()) != t.Status.SessionUID {
		return r.terminateForSession(ctx, t, "the AgentSession was replaced by a different session with the same name")
	}
	if pinned && !session.GetDeletionTimestamp().IsZero() {
		return r.terminateForSession(ctx, t, "session is being deleted")
	}

	phase, _, _ := unstructured.NestedString(session.Object, "status", "phase")
	endpoint, _, _ := unstructured.NestedString(session.Object, "status", "endpoint")

	switch phase {
	case "Completed", "Expired", "Failed":
		return r.terminateForSession(ctx, t, fmt.Sprintf("session is %s", phase))
	case "Bound":
	default:
		if pinned {
			// A bound session does not go back to waiting, so an unexpected
			// phase changes nothing: fall through, so the TTL is still enforced
			// and the connector is still kept up. Only the phases above end a
			// tunnel.
			return ctrl.Result{}, false, nil
		}
		return r.waitForSession(ctx, t, fmt.Sprintf("waiting for AgentSession %q to be Bound (it is %q)", session.GetName(), phase))
	}

	if _, perr := parseEndpoint(endpoint, t.Namespace); perr != nil {
		if pinned {
			return r.terminateForSession(ctx, t, "session endpoint changed to one that cannot be exposed: "+perr.Error())
		}
		return r.waitForSession(ctx, t, perr.Error())
	}
	if pinned {
		if endpoint != t.Status.SessionEndpoint {
			return r.terminateForSession(ctx, t, "session was bound to a different endpoint")
		}
		return ctrl.Result{}, false, nil
	}

	// First time Bound: pin the session before creating anything.
	t.Status.SessionUID = string(session.GetUID())
	t.Status.SessionEndpoint = endpoint
	t.Status.Phase = agentopsv1alpha1.PhasePending
	t.Status.Message = "session is Bound; creating the tunnel"
	if err := r.Status().Update(ctx, t); err != nil {
		return ctrl.Result{}, true, err
	}
	log.FromContext(ctx).Info("Pinned AgentSession", "session", session.GetName(), "uid", t.Status.SessionUID, "endpoint", endpoint)
	return ctrl.Result{}, false, nil
}

// waitForSession records why nothing has been created yet and polls again.
func (r *AgentEdgeTunnelReconciler) waitForSession(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, msg string) (ctrl.Result, bool, error) {
	if t.Status.Phase != agentopsv1alpha1.PhasePending || t.Status.Message != msg {
		t.Status.Phase = agentopsv1alpha1.PhasePending
		t.Status.Message = msg
		if err := r.Status().Update(ctx, t); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	return ctrl.Result{RequeueAfter: sessionPollInterval}, true, nil
}

// terminateForSession removes the tunnel's resources and marks it Terminated.
func (r *AgentEdgeTunnelReconciler) terminateForSession(ctx context.Context, t *agentopsv1alpha1.AgentEdgeTunnel, reason string) (ctrl.Result, bool, error) {
	log.FromContext(ctx).Info("Terminating AgentEdgeTunnel because of its session", "name", t.Name, "reason", reason)
	return ctrl.Result{}, true, r.terminate(ctx, t, "AgentSession ended: "+reason+"; Cloudflare resources removed")
}
