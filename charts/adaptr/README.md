# adaptr Helm Chart

Deploy [adaptr](https://github.com/sqoia-dev/adaptr) — SPA Subpath Router — on
Kubernetes. Supports standalone deployment and sidecar injection into existing
Deployments.

## Prerequisites

- Kubernetes 1.21+
- Helm 3.2+

## Installation

### Standalone (recommended for most setups)

```bash
helm install my-adaptr oci://ghcr.io/sqoia-dev/charts/adaptr \
  --set config.target=my-spa-service:3000 \
  --set config.basePath=/myapp
```

Or with a values file:

```bash
helm install my-adaptr oci://ghcr.io/sqoia-dev/charts/adaptr -f my-values.yaml
```

Minimum required `my-values.yaml`:

```yaml
config:
  target: "my-spa-service:3000"
  basePath: "/myapp"
```

### Sidecar mode

When `sidecar.enabled=true`, no Deployment or Service is created. The chart
renders only a ConfigMap (if `customRewrites.enabled=true`). Use this to
validate your values and then copy the container spec into your existing
Deployment manually.

```yaml
sidecar:
  enabled: true

config:
  basePath: "/myapp"
  externalPort: 8080
```

In your existing Deployment's container list, add:

```yaml
- name: adaptr
  image: ghcr.io/sqoia-dev/adaptr:0.3.0
  ports:
    - containerPort: 8080
  env:
    - name: TARGET
      value: "localhost:3000"
    - name: BASE_PATH
      value: "/myapp"
    - name: EXTERNAL_PORT
      value: "8080"
```

## Configuration

| Parameter | Description | Default |
|---|---|---|
| `image.repository` | Container image repository | `ghcr.io/sqoia-dev/adaptr` |
| `image.tag` | Image tag | `"0.3.0"` |
| `image.pullPolicy` | Pull policy | `IfNotPresent` |
| `config.target` | Upstream address to proxy (required) | `""` |
| `config.basePath` | URL subpath prefix (e.g. `/myapp`) | `""` |
| `config.externalPort` | Port adaptr listens on | `8080` |
| `config.rewriteHtml` | Rewrite HTML/JS/CSS asset paths | `true` |
| `config.passthroughPaths` | Comma-separated prefixes to skip rewriting | `""` |
| `config.maxRewriteBodyBytes` | Max body size buffered for rewriting | `10485760` |
| `service.type` | Service type | `ClusterIP` |
| `service.port` | Service port | `8080` |
| `ingress.enabled` | Create an Ingress resource | `false` |
| `ingress.className` | Ingress class name | `""` |
| `resources.limits.memory` | Memory limit | `128Mi` |
| `resources.requests.cpu` | CPU request | `50m` |
| `resources.requests.memory` | Memory request | `64Mi` |
| `sidecar.enabled` | Sidecar mode (no Deployment or Service) | `false` |
| `replicaCount` | Number of Deployment replicas | `1` |
| `serviceAccount.create` | Create a dedicated ServiceAccount | `true` |
| `customRewrites.enabled` | Mount a custom rewrite rules ConfigMap | `false` |

## Security

All Pods run with a hardened security context:

- `runAsNonRoot: true`
- `runAsUser: 65534` (nobody)
- `readOnlyRootFilesystem: true`
- `allowPrivilegeEscalation: false`
- All Linux capabilities dropped

## Health Checks

adaptr exposes a `/health` endpoint that returns `{"status":"ok"}` when the
process is running.  Both liveness and readiness probes target this endpoint.

The `/health` check verifies adaptr is alive — it does not check upstream
connectivity.  If the upstream target is temporarily unreachable, adaptr
continues to serve requests (and return 502s from the upstream), which is the
correct proxy behavior.

## Metrics (Future)

adaptr currently handles one upstream target per instance.  A `/metrics`
endpoint (Prometheus text format) is planned for a future release to expose
per-instance request counts, rewrite latency, and error rates.  For now,
`/health` is sufficient for liveness and readiness signalling.

Set `config.metricsEnabled: true` in values to opt into the metrics endpoint
when it ships.  This flag is a no-op in the current release.

## Upgrading

```bash
helm upgrade my-adaptr oci://ghcr.io/sqoia-dev/charts/adaptr \
  --set image.tag=<new-version>
```

## Uninstalling

```bash
helm uninstall my-adaptr
```
