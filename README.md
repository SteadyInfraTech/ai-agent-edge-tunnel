# ai-agent-edge-tunnel

`ai-agent-edge-tunnel` is a Kubernetes operator designed to provision short-lived Cloudflare Zero Trust Tunnels, DNS routes, and Access policies specifically for ephemeral AI Agent microservices (e.g., AutoGen, LangGraph, CrewAI).

## How it works

```yaml
apiVersion: edge.steadyinfra.com/v1alpha1
kind: AgentEdgeTunnel
metadata:
  name: sample-agent-tunnel
spec:
  service: { name: langgraph-worker, port: 8080 }   # in-namespace Service to expose
  cloudflare: { zoneID: "...", subdomain: agent-job-8f92 }
  ttl: 2h
  security:
    allowedEmails: [devops@company.com]             # or: allowPublic: true
```

For each object the operator creates, in this order:

1. a Cloudflare **Tunnel** (its ID and credentials are stored immediately, so retries never leak tunnels);
2. for objects with `security.allowedEmails`, a Cloudflare **Access** application and allow policy, so the hostname is protected *before* it exists (objects with `security.allowPublic: true` deliberately get none);
3. a proxied **DNS** CNAME, which is what makes `agent-job-8f92.<base-domain>` reachable;
4. a **cloudflared** Deployment in the object's namespace, configured to send that hostname, and nothing else, to `service`.

If the Access policy of a protected object cannot be created, nothing after it runs: the hostname never goes live unprotected.
When the `ttl` expires, or the object is deleted, everything is removed in reverse order
(connector first, because Cloudflare will not delete a tunnel that is still connected).
`Terminated` is final; an expired object is never re-provisioned.

```console
$ kubectl get agentedgetunnels
NAME                  PHASE    HOSTNAME                         CONNECTED   EXPIRES
sample-agent-tunnel   Active   agent-job-8f92.agents.acme.dev   true        2026-11-05T14:00:00Z
```

`Active` means the tunnel and DNS record exist, plus the Access policy for email-protected objects; `CONNECTED` is whether the cloudflared pod
has connected to Cloudflare's edge.

### Access policy verification cadence

Email-protected active tunnels re-verify the complete Cloudflare Access application and policy on a per-tunnel
jittered interval of 5-10 minutes. Session reconciliation and Kubernetes watch events reuse the last successful
verification until that deadline, rather than issuing Access API reads on every 10-second session poll. Successful
checks persist their verification time and next deadline in status.

A Cloudflare HTTP 429 is treated as API-budget pressure, not proof of policy drift: the tunnel remains Active,
the existing tunnel state is retained and surfaced as a rate-limit warning, and verification is deferred for a
per-tunnel jittered 1-2 minute backoff. Other verification errors remain visible as unprotected and are retried by reconciliation;
a confirmed policy mismatch still terminates the tunnel fail-closed.

## Exposing an AgentSession

Instead of a fixed `spec.service`, a tunnel can follow an `AgentSession` from
[agentops-operator](https://github.com/SteadyInfraTech/agentops-operator):

```yaml
spec:
  sessionRef: { name: support-session-8f92 }   # same namespace; mutually exclusive with service
```

The session's `status.phase` and `status.endpoint` are the whole contract; the session is read as
unstructured, so this operator does not depend on agentops-operator and starts without it installed.

- **Waiting.** Until the session is `Bound` (or if it does not exist yet), nothing is created in
  Cloudflare and the tunnel stays `Pending`. Create the two objects in either order.
- **Pinning.** When the session is first seen `Bound`, its UID and endpoint are recorded in status
  before anything is created. A session deleted and recreated under the same name has a different UID
  and does not inherit the tunnel or its Access policy.
- **Ending.** The tunnel is removed and ends `Terminated` when the session is `Completed`, `Expired` or
  `Failed`, is deleted or replaced, or is bound to a different endpoint. Extending a session's duration
  needs no action here, because the session's phase decides. `spec.ttl` is only an optional hard cap.
- **Target.** Only an endpoint of the form `http://<service>.<this namespace>.svc:<port>` is accepted;
  anything else (another namespace, another host, https, a path) is refused and nothing is created.
- **Service isolation.** The target must be a selector-backed ClusterIP Service with a TCP port.
  ExternalName, headless, selectorless, and external-IP Services are rejected. Every EndpointSlice
  endpoint must reference a non-hostNetwork Pod selected by that Service in the same namespace,
  and its addresses must match that Pod's IPs. Service and EndpointSlice changes are rechecked;
  an active tunnel is terminated if its origin becomes unsafe.
- **Latency.** The session is polled every 10 seconds, so a tunnel can outlive its session by up to that long.
- **Isolation.** The tunnel exposes the agent's Service, not one session. Access decides who can reach it,
  not which session they may use; see the design note for what that means for multi-session agents.
- **Clients** must wait for `status.phase: Active` and `status.connectorReady: true`, keep watching
  through `Failed` (transient errors retry), and treat only `Terminated` as final.
- **RBAC.** The operator needs `get` on `agentsessions` (`agents.steadyinfra.com`) in the namespaces it manages;
  both install options grant it.

## Security model

- **Never public by omission.** Set `security.allowedEmails`, or opt in with `security.allowPublic: true`
  (for example a webhook receiver). Neither, or both, is rejected. Public tunnels additionally require the
  object's namespace to appear in the operator's `EDGE_PUBLIC_NAMESPACES` allowlist; an empty allowlist
  denies public tunnels everywhere. Removing a namespace from the allowlist causes its existing public
  tunnels to be cleaned up and marked `Terminated`.
- **One configured Cloudflare zone.** `CLOUDFLARE_ZONE_ID` and `CLOUDFLARE_ZONE_NAME` are required at startup. The operator verifies
  `EDGE_BASE_DOMAIN` is within the configured zone name and rejects objects whose `spec.cloudflare.zoneID` differs, preventing tenants from selecting another zone through
  their custom resource. Configure `EDGE_BASE_DOMAIN` to a domain served by that zone.
- **Admission-time tenant guard.** The default Kustomize install also applies a Kubernetes `ValidatingAdmissionPolicy` and binding. It reads the same `edge-tunnel-config` ConfigMap as the operator, denies `spec.cloudflare.zoneID` values that do not match `CLOUDFLARE_ZONE_ID`, checks that `EDGE_BASE_DOMAIN` is within `CLOUDFLARE_ZONE_NAME`, and permits `security.allowPublic: true` only when the request namespace is listed in `EDGE_PUBLIC_NAMESPACES`. The binding uses `parameterNotFoundAction: Deny`, so creates fail closed when the parameter ConfigMap is missing. The parameterized policy applies to creates; the CRD makes `spec.cloudflare` and `security.allowPublic` immutable, so those admission-critical values cannot be changed later. A separate parameter-independent policy permits the controller service account to change controller finalizers and permits the Kubernetes generic-garbage-collector service account to remove only its own `foregroundDeletion` or `orphan` propagation finalizers. This exception requires the cluster's `kube-controller-manager` to run with `--use-service-account-credentials=true`, so the generic garbage collector authenticates as `system:serviceaccount:kube-system:generic-garbage-collector`; deployments using the controller-manager kubeconfig identity are not supported by this admission policy. This allows tenant metadata updates and safe cleanup during a ConfigMap outage without bypassing zone or public-exposure checks. The operator repeats these checks during reconciliation as defense in depth. Admission reads the ConfigMap on each request, while the operator loads it at startup; restart the manager after every ConfigMap change so both enforce the same values. This admission policy requires Kubernetes 1.30 or newer.
- **Unimplemented protections are rejected, not ignored.** `security.webhookValidation` (edge HMAC checks) and
  `security.serviceTokens` are not implemented yet; setting them fails validation so nobody believes a
  protection is active when it is not.
- **Credentials stay in a Secret.** The tunnel credentials and config are mounted from a Secret owned by the
  object (garbage-collected with it) with mode `0440`; they never appear in args, env vars or `kubectl describe`.
- **Hardened connector pod:** non-root, read-only root filesystem, all capabilities dropped, `RuntimeDefault`
  seccomp, no service account token.
- **Operator RBAC and blast radius.** The operator can `get/create/update/delete` Secrets but not `list` or
  `watch` them, so it cannot enumerate Secrets. Its cache is disabled for Secrets to make that work. This is
  not a namespace boundary in the default install: the default ClusterRole is cluster-wide and permits the
  operator to `get` Secrets by known name and to create, update, patch, or delete Deployments in **any**
  namespace. A compromised operator pod could therefore read known Secret names and change workloads
  cluster-wide, in addition to managing AgentEdgeTunnel resources. Treat the operator and its Cloudflare
  credentials as cluster-privileged components; isolate its pod, restrict who can modify its configuration
  and watched resources, and do not install it into a multi-tenant cluster with the default overlay unless
  that blast radius is intentional. For a narrower boundary, use the namespaced install below. The
  namespaced overlay removes the ClusterRole and ClusterRoleBinding and grants equivalent access only
  through namespace-local Roles and RoleBindings in explicitly listed namespaces. It limits Secret and
  Deployment access to those namespaces; it does not make the operator safe to share with mutually
  untrusted tenants inside a namespace it manages.
- **No adoption.** The operator refuses to overwrite a Secret, Deployment or DNS record it does not own.
- **Immutable once created:** `spec.service`, `spec.security` and `spec.cloudflare` cannot change after the
  tunnel exists; delete and recreate instead.
- Metrics are disabled in the shipped deployment (`--metrics-bind-address=0`). If enabled by changing the manager arguments to a non-zero bind address, controller-runtime serves them over plain HTTP without authentication by default. Do not enable that listener on a cluster-reachable interface unless a NetworkPolicy or equivalent network control restricts access to trusted Prometheus clients. The project does not ship a metrics Service or NetworkPolicy; health and readiness remain on port `8081`.

## Status

Early `v1alpha1`.

| | |
|---|---|
| Implemented | Tunnel, Access policy, DNS and cloudflared connector per object; TTL (clock starts when Active); finalizer cleanup that retries; leader election, health probes, metrics |
| Not implemented | Edge HMAC webhook validation; Access service tokens; `cleanupOnFinish`; reacting to the target pod finishing; more than one connector replica |
| Known limits | Namespace quotas are opt-in through a NamespaceTunnelQuota resource; namespaces without one remain unlimited. Cloudflare account capacity and API rate limits remain the external constraints. A crash between creating a Cloudflare resource and recording its ID can leave an orphan. A tunnel is recovered only when its connector Secret is still controlled by the same AgentEdgeTunnel and contains credentials naming that tunnel; otherwise reconciliation stops and the tunnel must be resolved manually. Matching Access applications cannot currently be proven to belong to an object because Cloudflare's lookup exposes no owner identity, so they are left untouched and require operator resolution rather than being adopted or deleted. Resources whose owner object is gone are not swept and must be removed by hand. Cleanup removes DNS independently, deletes the tunnel only after connector removal succeeds, and deletes Access only after tunnel deletion succeeds. The connector Secret cannot be regenerated because Cloudflare returns tunnel credentials only once: if it is deleted, delete and recreate the object. Access applications are created per object, so very large numbers of tunnels count against your Cloudflare plan's limits. |
| Deprecated | `spec.targetPodSelector` is ignored; use `spec.service` |


## Namespace tunnel quotas

Tunnel quotas are opt-in and namespace-scoped. Without a `NamespaceTunnelQuota`
object, the operator imposes no tunnel-count limit. These Kubernetes quotas
only constrain this operator's admission within a namespace; they do not limit
Cloudflare account-wide tunnel capacity or Cloudflare API rate limits. To cap a
namespace at 25 tunnels, an administrator applies:

```yaml
apiVersion: edge.steadyinfra.com/v1alpha1
kind: NamespaceTunnelQuota
metadata:
  name: default
  namespace: agents
spec:
  limit: 25
```

The object name is `default`; create at most one quota object per namespace.
A limit of zero blocks new admissions. Existing provisioned tunnels are
grandfathered when a quota is introduced or lowered; lowering a limit never
terminates running tunnels, but prevents new reservations until usage falls
below the limit.

Admission is reserved before the operator creates any Cloudflare resource.
Reservations are keyed by Kubernetes object UID and stored in the quota
status subresource. Updates use API-server resource versions as an atomic
compare-and-swap, so simultaneous creates cannot claim the same remaining
slot. Retries reuse the same reservation. The reservation remains held while
provisioning is retried and is released only after Cloudflare and connector
cleanup succeeds. A failed cleanup therefore retains both the finalizer and
the quota slot for a later retry.

The operator's RBAC grants it read access to quota specs and write access only
to quota status. The default and namespaced installs apply two admission
policies. The spec policy restricts quota `CREATE`, `UPDATE`, and `DELETE` to
members of `system:masters` or groups listed in the optional comma-separated
`EDGE_QUOTA_ADMIN_GROUPS` ConfigMap key. If the key is unset, only
`system:masters` is authorized. Its binding uses
`parameterNotFoundAction: Deny`, so all quota spec changes are blocked while
the ConfigMap is missing; restore it before managing quota objects.
The separate status policy restricts updates to `namespacetunnelquotas/status`
to the operator's `edge-tunnel-controller-manager` ServiceAccount and
`system:masters`, independently of the ConfigMap. Configured quota-admin
groups can manage `spec.limit` but cannot forge or remove reservations.
Changing `spec.limit` is an administrative capacity decision; do not grant
quota write permissions to tenant roles even with these policies installed.

## Project Structure

```text
.
├── .github/workflows/ci.yml       # GitHub Actions CI for linting and testing
├── api/v1alpha1/                 # CRD Go Structs
├── cmd/main.go                   # Operator entrypoint
├── Dockerfile                    # Distroless, non-root image
├── config/                       # Kustomize manifests
│   ├── crd/bases/
│   ├── default/                  # namespace + CRD + RBAC + admission policy + manager
│   ├── admission/                # ValidatingAdmissionPolicy + binding
│   ├── manager/
│   ├── rbac/
│   └── samples/
├── internal/
│   ├── cloudflare/               # Cloudflare API client wrapper
│   │   └── fake/                 # In-memory fake for tests and --dry-run
│   └── controller/               # Reconciler logic & tests
├── Makefile                      # Build, test, and dev helpers
├── go.mod
└── README.md
```

## Quickstart

### Prerequisites
- Go `1.27.0+` (see `go.mod`)
- Kubernetes `1.30+` (the install uses `ValidatingAdmissionPolicy` v1)
- Cluster control-plane requirement: `kube-controller-manager` must run with `--use-service-account-credentials=true`; the admission policy relies on the generic garbage collector's dedicated `system:serviceaccount:kube-system:generic-garbage-collector` identity for safe propagation-finalizer removal. With this flag disabled, the garbage collector uses the controller-manager kubeconfig identity and cleanup is denied.
- `kubectl` configured to target a Kubernetes cluster
- Cloudflare Account ID and API Token (with Tunnel, DNS, and Access permissions)

### Configuration

| Flag / env | Purpose |
|---|---|
| `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN` | Required. The operator exits at startup if either is missing. The token needs Tunnel, DNS and Access edit permissions. |
| `CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_ZONE_NAME` | Required. Pins this operator instance to one Cloudflare zone; the base domain must be the zone apex or one of its subdomains, and each object's `spec.cloudflare.zoneID` must match. |
| `EDGE_PUBLIC_NAMESPACES` | Optional comma-separated namespace allowlist for `security.allowPublic: true`. Empty or unset denies public tunnels in every namespace. |
| `EDGE_QUOTA_ADMIN_GROUPS` | Optional comma-separated Kubernetes groups authorized to manage `NamespaceTunnelQuota` objects. `system:masters` is authorized while the policy ConfigMap is available. |
| `--base-domain` / `EDGE_BASE_DOMAIN` | Required. Hostnames are `<subdomain>.<base-domain>`. After changing it and restarting the manager, existing tunnels whose recorded hostname no longer matches are terminated and cleaned up; tunnels are not silently migrated. |
| `--connector-image` / `CONNECTOR_IMAGE` | Required. The cloudflared image; pin a version or digest. |
| `--leader-elect` | Enable when running more than one replica. |
| `--health-probe-bind-address` | `/healthz` and `/readyz` (default `:8081`). |
| `--metrics-bind-address` | Prometheus metrics; disabled in the shipped deployment (`0`). To enable, set a non-zero bind address and restrict network access to trusted scrapers; the default HTTP endpoint has no authentication. |
| `--dry-run` | Use an in-memory fake instead of the Cloudflare API. Nothing is provisioned in Cloudflare; the connector Deployment is still created in the cluster. Local development only. Fake IDs start with `dryrun-`: a real-mode operator discards status that dry-run wrote, and a dry-run operator refuses (and will not delete) objects that hold real IDs, so the two never act on each other's state. |
| `--zap-devel`, `--zap-log-level` | Logging (JSON by default). |

### Deploy

```bash
kubectl create namespace edge-tunnel-system
kubectl -n edge-tunnel-system create secret generic edge-tunnel-cloudflare \
  --from-literal=account-id=... --from-literal=api-token=...
kubectl -n edge-tunnel-system create configmap edge-tunnel-config \
  --from-literal=EDGE_BASE_DOMAIN=agents.example.com \
  --from-literal=CLOUDFLARE_ZONE_ID=<zone-id> \
  --from-literal=CLOUDFLARE_ZONE_NAME=example.com \
  --from-literal=CONNECTOR_IMAGE=<pinned cloudflared image> \
  --from-literal=EDGE_PUBLIC_NAMESPACES=agent-public \
  --from-literal=EDGE_QUOTA_ADMIN_GROUPS=platform-admins
make docker-build IMG=<your-registry>/ai-agent-edge-tunnel:<tag>   # and push it
make deploy IMG=<your-registry>/ai-agent-edge-tunnel:<tag>        # needs the kustomize CLI
kubectl apply -f config/samples/edge_v1alpha1_agentedgetunnel.yaml
```

#### Namespace-scoped install

`make deploy-namespaced IMG=<image>` (or `kubectl apply -k config/overlays/namespaced`, which uses the checked-in image) installs the operator with no ClusterRole or ClusterRoleBinding. **Prefer this mode for production clusters that host multiple teams or trust domains**, unless cluster-wide workload management is an explicit requirement. It runs with
`--watch-namespaces=agents` (env `EDGE_WATCH_NAMESPACES`), so its cache covers only those namespaces, and a `Role`
and `RoleBinding` in each of them grant the Secret and Deployment access. A compromised operator pod can then read
Secrets by known name and manage Deployments only in the namespaces you listed. Edit `config/overlays/namespaced/tunnel-namespace.yaml` for your
namespace(s), repeating the Role and RoleBinding for each. To configure the watch list through `EDGE_WATCH_NAMESPACES` instead of a command-line
argument, remove `config/overlays/namespaced/watch-namespaces-patch.yaml` and remove its
`patches` entry from `config/overlays/namespaced/kustomization.yaml`. Then add
`EDGE_WATCH_NAMESPACES` to the `edge-tunnel-config` ConfigMap in `edge-tunnel-system`,
using the same comma-separated namespace list as the namespace-local Roles and RoleBindings.
For example, when creating the ConfigMap, include:

```bash
--from-literal=EDGE_WATCH_NAMESPACES=agents,other-managed-namespace
```

The ConfigMap is loaded into the manager's environment, and `EDGE_WATCH_NAMESPACES` supplies
the default for `--watch-namespaces` when no explicit command-line argument overrides it.
After changing the ConfigMap, restart the manager Deployment so its containers load the updated
environment value:

```bash
kubectl -n edge-tunnel-system rollout restart deployment/edge-tunnel-controller-manager
```

Keep the Role/RoleBinding namespaces and the effective watch list in sync.
AgentEdgeTunnel objects in other namespaces are ignored. This is a namespace-level boundary, not isolation
between tenants that share a managed namespace.

**Switching from the default install:** before applying the namespaced overlay, inspect all existing
tunnels and their namespaces:

```bash
kubectl get agentedgetunnels --all-namespaces
```

Every namespace containing a tunnel that should remain managed must be included in both the overlay's
namespace-local Role/RoleBinding resources and the manager's `--watch-namespaces` list
(`EDGE_WATCH_NAMESPACES`). Otherwise, the namespaced operator ignores those objects; it cannot
reconcile them or complete their finalizer cleanup. Keep those namespaces in the watch list, or migrate
the tunnel workloads and recreate their AgentEdgeTunnel objects in managed namespaces, or clean up the
tunnels while the existing operator still watches them. Do not switch the watch list and leave live
tunnels behind in unwatched namespaces.

Applying the namespaced overlay does not prune resources that are present in the old install but absent
from the overlay. After the namespaced install is working and its namespace-local Roles and RoleBindings
are in place, explicitly remove the old cluster-wide grant:

```bash
kubectl delete clusterrole edge-tunnel-manager-role
kubectl delete clusterrolebinding edge-tunnel-manager-rolebinding
```

Confirm the current cluster context and that these resources belong to this operator before deleting them.
Do not leave them behind: the namespaced overlay's rendered manifests omit them, but a normal
`kubectl apply -k` does not delete previously applied resources.

`make deploy` sets the manager image to `IMG` on a temporary copy of `config/`. **Use
`make undeploy` with care:** it first deletes every `AgentEdgeTunnel` object in every namespace
(`--all-namespaces`) and waits for finalizer cleanup, then deletes the resources in
`config/default`, including the manager, CRD, default cluster-wide RBAC, and the
`edge-tunnel-system` Namespace. Deleting that Namespace also deletes everything in it, including the
Quickstart `edge-tunnel-cloudflare` Secret and `edge-tunnel-config` ConfigMap. Back up or otherwise
preserve any Cloudflare credentials, configuration, or other namespace data you still need before teardown;
the Secret and ConfigMap must be recreated if you deploy again. This is not limited to the namespaces
watched by a namespaced install. On a shared cluster, check ownership with the other teams and confirm
that no other team owns or relies on any `AgentEdgeTunnel` objects before running it.

The final `kubectl delete -k config/default --ignore-not-found` tolerates resources that were
already removed, such as the ClusterRole and ClusterRoleBinding when switching from the default install
to the namespaced overlay. It still deletes the default-install resources that remain.

The command does **not** remove the namespace-scoped Role or RoleBinding created by
`config/overlays/namespaced/tunnel-namespace.yaml`, nor does it delete managed namespaces such as
`agents`. After the tunnel cleanup and manager teardown have completed, remove the namespaced RBAC
resources yourself in each managed namespace if they are no longer needed. For the example namespace:

```bash
kubectl delete role edge-tunnel-manager-role -n agents
kubectl delete rolebinding edge-tunnel-manager-rolebinding -n agents
```

Repeat for each namespace configured in the overlay. The namespace itself is retained; delete it separately
only if it is dedicated to this operator and you have confirmed that it contains no other workloads or data.

### Local Development & Testing

1. **Run Unit Tests (Mocked)**:
   ```bash
   make test
   ```

2. **Run Tests with Live Cloudflare API Token**:
   Set environment variables before running tests:
   ```bash
   export CLOUDFLARE_API_TOKEN="your-cloudflare-api-token"
   export CLOUDFLARE_ACCOUNT_ID="your-cloudflare-account-id"
   export CLOUDFLARE_ZONE_ID="your-cloudflare-zone-id"
   export CLOUDFLARE_TEST_HOSTNAME="ci-test.agents.example.com"  # optional locally; required by the GitHub live job for DNS coverage
   go test -v ./internal/cloudflare/ -run TestLiveCloudflareTunnel
   ```

3. **Install the CRD only** (to try it with `--dry-run`): `make install`

## Configuring GitHub Actions CI Secrets

The live Cloudflare test runs only after a merge to `main` (or when started by hand on `main`), never on pull
requests, so pull-request code cannot read the credentials. The live job now fails explicitly if any of its
four required environment secrets is missing, rather than silently running only a partial tunnel test. To enable it:
1. Navigate to **Settings -> Environments**, create an environment named `cloudflare-live`, and restrict its
   deployment branches to `main`.
2. Add `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_ZONE_ID` and
   `CLOUDFLARE_TEST_HOSTNAME` as **environment** secrets, and remove any repository-level copies.

### Live end-to-end test on kind (optional)

A second job, `Live end-to-end on kind`, runs both operators and the agentops-client in a kind cluster
against real Cloudflare: session to tunnel, connector, DNS, the Access gate on the public URL, and teardown.
On a successful run it checks that nothing is left in Cloudflare. After a failed run it tries to remove
leftovers for that run, but a failed lookup or delete, or a run that stopped before a tunnel ID was recorded,
can leave resources behind, so inspect Cloudflare (tunnels, DNS records, Access applications named `ci-<run id>`
or `early-<run id>`) after any failure. It needs everything above, with `CLOUDFLARE_TEST_HOSTNAME`
required (a hostname one level below the zone, such as `ci-test.example.com`), plus:

- Zero Trust enabled on the Cloudflare account. Without it Cloudflare answers `access.api.error.not_enabled`
  and no Access application can be created (Cloudflare asks for a payment method even on the free plan).
- An API token allowed to edit Access applications and policies, Cloudflare Tunnel and DNS.
- A repository **variable** (not secret) `LIVE_E2E_ENABLED` set to `true`. Without it the job is skipped.

## License
Apache-2.0
