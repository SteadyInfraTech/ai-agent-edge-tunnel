# ai-agent-edge-tunnel

`ai-agent-edge-tunnel` is a Kubernetes operator designed to provision short-lived Cloudflare Zero Trust Tunnels, DNS routes, and Access policies specifically for ephemeral AI Agent microservices (e.g., AutoGen, LangGraph, CrewAI).

## Key Features

- **Ephemeral Lifecycle**: Dynamically creates Cloudflare Tunnels, CNAME DNS records, and Access policies when an `AgentEdgeTunnel` resource is reconciled, and automatically cleans them up on pod/job termination or TTL expiration.
- **Zero-Trust Security**: No inbound open ports required on your cluster.
- **Webhook & Auth Guardrails**: Enforces JWT checks and HMAC webhook signature validation at the edge before traffic hits the agent container.
- **Testable Integration**: Includes mock interfaces and live test support using `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID`.

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
│   ├── cloudflare/               # Cloudflare API client wrapper & mocks
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
   go test -v ./internal/cloudflare/... -run TestLiveCloudflareTunnel
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
2. Add `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, and `CLOUDFLARE_ZONE_ID`.
3. The CI workflow in `.github/workflows/ci.yml` will automatically consume these secrets during integration test steps.

## License
Apache-2.0
