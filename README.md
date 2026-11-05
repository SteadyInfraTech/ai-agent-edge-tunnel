# ai-agent-edge-tunnel

`ai-agent-edge-tunnel` is a Kubernetes operator designed to provision short-lived Cloudflare Zero Trust Tunnels, DNS routes, and Access policies specifically for ephemeral AI Agent microservices (e.g., AutoGen, LangGraph, CrewAI).

## Status

Early `v1alpha1`. What works today and what is planned:

| | |
|---|---|
| Implemented | Create a Cloudflare Tunnel and proxied CNAME per `AgentEdgeTunnel`; record tunnel ID before DNS so retries never leak tunnels; TTL expiry (clock starts when Active); finalizer-based cleanup that retries on failure; `Terminated` is final |
| Planned | Run `cloudflared` for the tunnel and store its token in a Secret; Cloudflare Access policies from `security.allowedEmails`; service tokens; edge HMAC webhook validation; `targetPodSelector` / `cleanupOnFinish` |

The `security.*`, `targetPodSelector` and `cleanupOnFinish` fields are accepted by
the API but **not acted on yet**, so a tunnel created today is not protected by
Access and does not route to any pod.

## Project Structure

```text
.
├── .github/workflows/ci.yml       # GitHub Actions CI for linting and testing
├── api/v1alpha1/                 # CRD Go Structs
├── cmd/main.go                   # Operator entrypoint
├── config/                       # Kustomize base manifests & samples
│   ├── crd/bases/
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
| `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN` | Required. The operator exits at startup if either is missing. |
| `--base-domain` / `EDGE_BASE_DOMAIN` | Required. Hostnames are `<subdomain>.<base-domain>`. |
| `--dry-run` | Use an in-memory fake instead of Cloudflare. Nothing is provisioned. Local development only. |
| `--zap-devel`, `--zap-log-level` | Logging (JSON by default). |

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

3. **Deploy CRD to Cluster**:
   ```bash
   kubectl apply -f config/crd/bases/agentops.io_agentedgetunnels.yaml
   ```

4. **Deploy Sample CR**:
   ```bash
   kubectl apply -f config/samples/agentops_v1alpha1_agentedgetunnel.yaml
   ```

## Configuring GitHub Actions CI Secrets

To run live integration tests in GitHub CI:
1. Navigate to your repository **Settings -> Secrets and variables -> Actions**.
2. Add `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_ZONE_ID` and (optionally) `CLOUDFLARE_TEST_HOSTNAME`.
3. The CI workflow in `.github/workflows/ci.yml` will automatically consume these secrets during integration test steps.

## License
Apache-2.0
