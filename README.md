# ai-agent-edge-tunnel

`ai-agent-edge-tunnel` is a Kubernetes operator designed to provision short-lived Cloudflare Zero Trust Tunnels, DNS routes, and Access policies specifically for ephemeral AI Agent microservices (e.g., AutoGen, LangGraph, CrewAI).

## How it works

```yaml
apiVersion: agentops.io/v1alpha1
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
2. a Cloudflare **Access** application and allow policy, so the hostname is protected *before* it exists;
3. a proxied **DNS** CNAME, which is what makes `agent-job-8f92.<base-domain>` reachable;
4. a **cloudflared** Deployment in the object's namespace, configured to send that hostname, and nothing else, to `service`.

If the Access policy cannot be created, nothing after it runs: the hostname never goes live unprotected.
When the `ttl` expires, or the object is deleted, everything is removed in reverse order
(connector first, because Cloudflare will not delete a tunnel that is still connected).
`Terminated` is final; an expired object is never re-provisioned.

```console
$ kubectl get agentedgetunnels
NAME                  PHASE    HOSTNAME                         CONNECTED   EXPIRES
sample-agent-tunnel   Active   agent-job-8f92.agents.acme.dev   true        2026-11-05T14:00:00Z
```

`Active` means the tunnel, Access policy and DNS record exist; `CONNECTED` is whether the cloudflared pod
has connected to Cloudflare's edge.

## Security model

- **Never public by omission.** Set `security.allowedEmails`, or opt in with `security.allowPublic: true`
  (for example a webhook receiver). Neither, or both, is rejected.
- **Unimplemented protections are rejected, not ignored.** `security.webhookValidation` (edge HMAC checks) and
  `security.serviceTokens` are not implemented yet; setting them fails validation so nobody believes a
  protection is active when it is not.
- **Credentials stay in a Secret.** The tunnel credentials and config are mounted from a Secret owned by the
  object (garbage-collected with it) with mode `0440`; they never appear in args, env vars or `kubectl describe`.
- **Hardened connector pod:** non-root, read-only root filesystem, all capabilities dropped, `RuntimeDefault`
  seccomp, no service account token.
- **Least-privilege operator.** It can `get/create/update/delete` Secrets but not `list` or `watch` them, so it
  cannot enumerate Secrets in the cluster. Its cache is disabled for Secrets to make that work.
- **No adoption.** The operator refuses to overwrite a Secret, Deployment or DNS record it does not own.
- **Immutable once created:** `spec.service`, `spec.security` and `spec.cloudflare` cannot change after the
  tunnel exists; delete and recreate instead.
- Metrics (`--metrics-bind-address`) are plain HTTP without authentication; restrict them with a NetworkPolicy.

## Status

Early `v1alpha1`.

| | |
|---|---|
| Implemented | Tunnel, Access policy, DNS and cloudflared connector per object; TTL (clock starts when Active); finalizer cleanup that retries; leader election, health probes, metrics |
| Not implemented | Edge HMAC webhook validation; Access service tokens; `cleanupOnFinish`; reacting to the target pod finishing; more than one connector replica |
| Known limits | A crash between creating a Cloudflare resource and recording its ID can orphan it (a sweep keyed on an owner tag is the planned fix). The connector Secret cannot be regenerated because Cloudflare returns tunnel credentials only once: if it is deleted, delete and recreate the object. Access applications are created per object, so very large numbers of tunnels count against your Cloudflare plan's limits. |
| Deprecated | `spec.targetPodSelector` is ignored; use `spec.service` |

## Project Structure

```text
.
├── .github/workflows/ci.yml       # GitHub Actions CI for linting and testing
├── api/v1alpha1/                 # CRD Go Structs
├── cmd/main.go                   # Operator entrypoint
├── Dockerfile                    # Distroless, non-root image
├── config/                       # Kustomize manifests
│   ├── crd/bases/
│   ├── default/                  # namespace + CRD + RBAC + manager
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
- Go `1.22+`
- `kubectl` configured to target a Kubernetes cluster
- Cloudflare Account ID and API Token (with Tunnel, DNS, and Access permissions)

### Configuration

| Flag / env | Purpose |
|---|---|
| `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN` | Required. The operator exits at startup if either is missing. The token needs Tunnel, DNS and Access edit permissions. |
| `--base-domain` / `EDGE_BASE_DOMAIN` | Required. Hostnames are `<subdomain>.<base-domain>`. |
| `--connector-image` / `CONNECTOR_IMAGE` | Required. The cloudflared image; pin a version or digest. |
| `--leader-elect` | Enable when running more than one replica. |
| `--health-probe-bind-address` | `/healthz` and `/readyz` (default `:8081`). |
| `--metrics-bind-address` | Prometheus metrics; `0` (default) disables them. |
| `--dry-run` | Use an in-memory fake instead of the Cloudflare API. Nothing is provisioned in Cloudflare; the connector Deployment is still created in the cluster. Local development only. |
| `--zap-devel`, `--zap-log-level` | Logging (JSON by default). |

### Deploy

```bash
kubectl create namespace edge-tunnel-system
kubectl -n edge-tunnel-system create secret generic edge-tunnel-cloudflare \
  --from-literal=account-id=... --from-literal=api-token=...
# Set EDGE_BASE_DOMAIN and CONNECTOR_IMAGE in config/manager/manager.yaml, then:
make docker-build IMG=<your-registry>/ai-agent-edge-tunnel:<tag>   # and push it
make deploy
kubectl apply -f config/samples/agentops_v1alpha1_agentedgetunnel.yaml
```

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
   export CLOUDFLARE_TEST_HOSTNAME="ci-test.agents.example.com"  # optional; enables the DNS part
   go test -v ./internal/cloudflare/ -run TestLiveCloudflareTunnel
   ```

3. **Install the CRD only** (to try it with `--dry-run`): `make install`

## Configuring GitHub Actions CI Secrets

To run live integration tests in GitHub CI:
1. Navigate to your repository **Settings -> Secrets and variables -> Actions**.
2. Add `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_ZONE_ID` and (optionally) `CLOUDFLARE_TEST_HOSTNAME`.
3. The CI workflow in `.github/workflows/ci.yml` will automatically consume these secrets during integration test steps.

## License
Apache-2.0
