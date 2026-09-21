# gatewai-gateway

Helm chart for the **GatewAI** API gateway — accepts file uploads, pushes async jobs to a Redis queue consumed by relay sidecars, and exposes sync (OpenAI-compatible) endpoints for AI inference services.

## Prerequisites

- Kubernetes ≥ 1.25
- Helm ≥ 3.10
- Redis-HA instance (included as subchart)
- S3-compatible bucket (Scaleway Object Storage, AWS S3, MinIO…)
- Relay sidecar container deployed alongside each inference pod (see `relay/`)

## Installation

### Via Helm repository (recommended)

The chart is published automatically to GitHub Pages on every push to `main` that changes `helm/`.

```bash
helm repo add GatewAI https://ia-generative.github.io/GatewAI
helm repo update
helm install gatewai-gateway GatewAI/gatewai-gateway -f values.yaml
```

### From source

```bash
helm dependency update ./helm/gateway
helm upgrade --install gatewai-gateway ./helm/gateway -f values.yaml
```

## Architecture

```
Client
  │  POST /jobs/{service_type}        (async — Redis queue)
  │  POST /v1/*                       (sync direct proxy or LLM proxy)
  ▼
Gateway (:8080)
  ├── S3 — upload/download
  ├── Redis — job state + relay queue (relay:<model>:pending) + pub/sub completion
  └── Relay sidecar (inside inference pod)
            ├── BLMOVE relay:<model>:pending → processes job
            └── PUBLISH jobs:<model>:completed → Gateway → Redis update + Webhook
```

## Configuration

### Image

| Parameter | Description | Default |
|---|---|---|
| `image.repository` | Gateway image | `ghcr.io/qatr-io/gatewai/gateway` |
| `image.tag` | Image tag | `v0.20.0` |
| `image.pullPolicy` | Pull policy | `IfNotPresent` |

### Config

| Parameter | Description | Default |
|---|---|---|
| `config.existingConfigMap` | **Option B** — name of an existing ConfigMap containing a `config.yaml` key. When set, the chart does not create a ConfigMap. | `""` |

When `config.existingConfigMap` is set, the chart mounts the referenced ConfigMap as `/etc/GatewAI/config.yaml`. The ConfigMap must contain the key `config.yaml`. Use this to manage configuration externally (e.g. with a GitOps tool or External Secrets).

### Server / request body size

| Parameter | Description | Default |
|---|---|---|
| `server.consumerHeader` | Header identifying the consumer (e.g. `X-Consumer-Username`). Required for per-consumer rate limiting and job ownership. | `""` |
| `server.userTypeHeader` | Header carrying the consumer type; matched against `rate_limits[type][user_type]`. | `""` |
| `server.priorityHeader` | Header marking high-priority requests (pushed to the head of the Redis queue). | `""` |
| `server.maxBodyMB` | Max request body (MiB) on the **sync JSON path** (`POST /v1/*`). Oversized requests get a clean **413**. `0` = 1 MiB default. | `0` |

The sync JSON path carries base64-embedded images for vision models. The default 1 MiB cap is fine for text but too small for images — raise `server.maxBodyMB` to enable them. **Base64 inflates binary by ~33%**, so a 30 MB image becomes ~40 MB of JSON: size the cap on the *encoded* payload (e.g. `maxBodyMB: 45` for 30 MB images). This is independent of `services[].maxFileSizeMB`, which bounds the multipart file-upload path only.

### S3

Two options — choose one:

| Parameter | Description |
|---|---|
| `s3.endpoint` | S3-compatible endpoint (e.g. `https://s3.fr-par.scw.cloud`) |
| `s3.region` | Region (e.g. `fr-par`) |
| `s3.bucket` | Bucket name |
| `s3.accessKey` | **Option A** — access key (chart creates a Secret) |
| `s3.secretKey` | **Option A** — secret key |
| `s3.existingSecret` | **Option B** — name of an existing Secret containing `S3_ACCESS_KEY` and `S3_SECRET_KEY` |

### At-rest encryption (S3)

Files are encrypted with AES-256-GCM before upload and decrypted on download. The same key must be configured in all relay sidecars (`ENCRYPTION_KEY` env var).

Generate a key: `openssl rand -hex 32`

| Parameter | Description |
|---|---|
| `encryption.key` | **Option A** — hex-encoded 32-byte key (chart creates a Secret) |
| `encryption.existingSecret` | **Option B** — existing Secret with key `ENCRYPTION_KEY` (e.g. managed by External Secrets Operator) |

Leave both empty to disable encryption.

### Services

Each entry in `services` registers one inference model with the gateway. Three operating modes are supported per service:

| Mode | Required fields | Behaviour |
|---|---|---|
| **Async** | `inferenceURL` (or `backends`) | `POST /jobs/{type}` pushes to Redis relay queue; relay processes and publishes completion; `GET /jobs/{type}/{id}` polls result |
| **Sync direct proxy** | `inferenceURL` (or `backends`) | `POST /v1/*` → proxied directly to inference backend; `POST /jobs/{type}` → 405 |
| **LLM proxy** | `provider`, `inferenceURL` (or `backends`) | `POST /v1/*` JSON → proxy with cache, metrics, provider translation |

```yaml
services:
  # Full mode: async (Redis queue) + sync direct proxy
  - type: audio
    model: "whisper-large-v3"
    default: true                 # fallback when request omits "model" field
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inferenceURL: "http://GatewAI-transcription-predictor.default.svc.cluster.local"
    acceptedExts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    maxFileSizeMB: 500

  # Sync-direct only: POST /v1/* proxied directly to inferenceURL
  - type: reranker
    model: "bge-reranker-v2-m3"
    operations:
      rerank:
        - "/rerank"
    inferenceURL: "http://GatewAI-reranker-predictor.default.svc.cluster.local"
    # No relay queue — sync-direct mode only
```

**Field reference:**

| Field | Description |
|---|---|
| `type` | Service type used in async routes (`/jobs/{type}`). Multiple models can share the same type. |
| `model` | Model identifier — matched against the `model` field in the request. |
| `default` | `true` → used as fallback when `model` is omitted and multiple models are registered for the type. |
| `deprecated` | `true` → marks the model deprecated. Surfaced in `GET /v1/models` (`capabilities.deprecated`) and as `deprecated: true` on the corresponding OpenAPI operations (per-model swagger doc always; a shared sync path only when every model on it is deprecated). Informational only — does not affect routing or availability. |
| `operations` | Map of `operationName → [url-paths]`. All paths are indexed for sync routing. The first path of the selected operation is forwarded in async InputEvents. |
| `inferenceURL` | Base URL of the Knative InferenceService predictor (cluster-local). The original request path is appended at runtime. Single-backend legacy — use `backendPool` for multi-backend. |
| `backendPool` | Name of a `backendPools[]` entry to route through instead of `inferenceURL`/`backends` — mutually exclusive with both. See below. |
| `backends` | **Deprecated**, use `backendPools`/`backendPool` instead. Legacy inline list of backends with weighted routing, takes precedence over `inferenceURL`. Kept for backward compatibility only. |
| `acceptedExts` | Allowed file extensions (e.g. `[".mp3", ".wav"]`). Empty or absent = all extensions accepted. |
| `maxFileSizeMB` | Maximum upload size in MB. `0` or absent = 100 MB default. |
| `inferenceHeaders` | HTTP headers injected on every request to the backend (sync-direct and LLM proxy). Values support `${VAR}` env expansion. |
| `provider` | Activates LLM proxy mode: `openai`, `anthropic`, `ollama`, `passthrough`. Absent = legacy direct proxy. |
| `backendModel` | Default model name sent to the backend (rewrites the `model` field in the request body). Overridden by a `backendPools[].members[].model` (or legacy `backends[].model`). |
| `responseCacheTTL` | Redis response cache TTL in seconds. `0` = disabled. LLM proxy only. |
| `maxConcurrentSync` | Max simultaneous sync requests for this model across all replicas. `0` = unlimited. Returns `503` when full. |
| `priorityReservedSync` | Slots of `maxConcurrentSync` reserved for requests carrying `server.priorityHeader`. `0` = no reservation. |
| `swaggerURL` | Optional URL to an OpenAPI JSON spec for this service. Fetched once at startup; served at `GET /swagger/{type}/{model}`. Failures are logged and skipped. |
| `swaggerHeaders` | Optional map of HTTP headers sent when fetching `swaggerURL`. Values support `${VAR}` env expansion. |

#### Backend pools (`backendPools`)

A named, load-balancer-style group of member backends, referenced from `services[]` by name via `backendPool: <name>` — instead of every service declaring its own `backends`/`inferenceURL`. Lets several service/model aliases (e.g. two `type: llm` entries with different `model` values) share one physical backend's auth headers, rate limit, and concurrency budget. This is also the preferred way to do blue/green, canary, or fallback routing for a *single* alias — the legacy inline `backends[]` list is deprecated.

| Field | Description |
|---|---|
| `members[]` | List of pool members (required, ≥1). |
| `members[].url` | Backend URL (required). |
| `members[].weight` | Routing weight. `0` = fallback-only (never primary-selected via weighted-random). |
| `members[].model` | Overrides `backendModel` for this specific member — useful for canary deployments. |
| `members[].headers` | HTTP headers injected on requests to this member. Highest precedence of the three header sources (below). Values support `${VAR}` env expansion. |
| `members[].rateLimit` | Optional per-member `rate`/`period` cap, checked on every backend attempt. |
| `members[].maxConcurrent` | Optional per-member concurrency cap. |
| `headers` | Pool-level default headers, merged under `members[].headers`. Lowest precedence of the three header sources (below). |
| `rateLimit` | Pool-wide `rate`/`period` budget shared across all members and all services referencing this pool. |
| `maxConcurrent` | Pool-wide concurrency budget shared across all members and all services referencing this pool. Acquired once per client request. |
| `priorityReservedConcurrent` | Slots of `maxConcurrent` reserved for requests carrying `server.priorityHeader` — mirrors `services[].priorityReservedSync`. |

**Mutually exclusive** with `backends`/`inferenceURL` on the referencing service. **Header precedence** (lowest → highest): service `inferenceHeaders` → `backendPools[].headers` → `backendPools[].members[].headers`.

**Example — canary routing (90/10 split with per-member auth and a last-resort fallback):**

```yaml
backendPools:
  chat-canary:
    members:
      - url: "http://vllm-stable.default.svc.cluster.local:8000"
        weight: 90
        model: "meta-llama/Meta-Llama-3-8B-Instruct"
        headers:
          Authorization: "Bearer ${VLLM_STABLE_TOKEN}"
      - url: "http://vllm-canary.default.svc.cluster.local:8000"
        weight: 10
        model: "meta-llama/Meta-Llama-3.1-8B-Instruct"
        headers:
          Authorization: "Bearer ${VLLM_CANARY_TOKEN}"
      - url: "http://vllm-fallback.default.svc.cluster.local:8000"
        weight: 0   # last resort, only if stable and canary both return 5xx

services:
  - type: llm
    model: "chat"
    provider: passthrough
    responseCacheTTL: 300
    backendPool: chat-canary
    operations:
      chat:
        - "/v1/chat/completions"
```

Don't forget to inject the token env vars via `extraEnvVars`.

**Example — two aliases sharing one vLLM instance's rate limit and concurrency budget:**

```yaml
backendPools:
  vllm-llama3:
    members:
      - url: "http://vllm-1.default.svc.cluster.local:8000"
        weight: 3
        headers:
          Authorization: "Bearer ${VLLM_TOKEN}"
      - url: "http://vllm-2.default.svc.cluster.local:8000"
        weight: 1
    rateLimit:
      rate: 50
      period: "1s"
    maxConcurrent: 30
    priorityReservedConcurrent: 5

services:
  - type: llm
    model: "gpt-4o"
    provider: openai
    backendPool: vllm-llama3
    operations:
      chat:
        - "/v1/chat/completions"
  - type: llm
    model: "llama3-chat"
    provider: openai
    backendPool: vllm-llama3   # same pool, different alias — shares its budget
    operations:
      chat:
        - "/v1/chat/completions"
```

### Lifecycle and job retention

Controls how long Redis job records and S3 result files are kept, and when orphaned S3 files are cleaned up.

| Parameter | Description | Default |
|---|---|---|
| `lifecycle.persistsResult` | Keep Redis records and S3 results after first consumption (`GET` or webhook). `false` = immediate cleanup | `false` |
| `lifecycle.jobTTL.global` | Redis TTL for all job statuses (duration string, e.g. `"24h"`) | `""` (2h internal safety net) |
| `lifecycle.jobTTL.completed` | TTL override for completed jobs | `""` |
| `lifecycle.jobTTL.pending` | TTL override for pending/processing jobs | `""` |
| `lifecycle.jobTTL.failed` | TTL override for failed jobs | `""` |
| `lifecycle.gc.enabled` | Enable the unified GC | `false` |
| `lifecycle.gc.interval` | GC tick frequency | `"15m"` |
| `lifecycle.gc.orphanMinAge` | Minimum S3 object age before it is considered orphaned | `"5m"` |

The GC runs two phases per tick:
1. **Stale-pending sweep** — marks pending jobs stuck longer than `redis.pending_max_age` as failed and deletes their S3 input files.
2. **S3 orphan cleanup** — lists all S3 objects, groups them by job ID, and deletes any file whose Redis record has expired. Covers both `input_ref` and `result_ref` for all exit paths (failed webhooks, `persists_result: true`, never-polled results). Skipped entirely if Redis is unavailable.

All lifecycle parameters are hot-reload safe.

### Metrics configuration

| Parameter | Description | Default |
|---|---|---|
| `metricsConfig.topConsumers` | Expose top-N LLM consumers in Prometheus via Redis sorted sets; `0` = disabled | `0` |
| `metricsConfig.consumerLabels` | Direct per-consumer Prometheus labels — only for deployments with < 50 consumers | `false` |

### Usage tracking

Per-consumer, per-service-type request/activity/token counters, exposed via `GET /usage` (self-service) and `GET /-/usage` (admin, all consumers). Requires no extra setup beyond Redis; enabled by default.

| Parameter | Description | Default |
|---|---|---|
| `usage.retention` | Duration usage sorted-set keys are kept before expiry (Go duration string, e.g. `"720h"`). Empty or `"0"` = no TTL (all-time accumulation) | `""` |

### Rate limits

Per-consumer, per-service fixed-window limiting backed by Redis, keyed by `{consumer}:{service_type}:{user_type}`. Set it via the chart's `rateLimits` value (camelCase keys); the ConfigMap renders it into the gateway's `rate_limits` block:

```yaml
# values.yaml
rateLimits:
  llm:
    sa:                   # user_type from server.userTypeHeader
      rate: 100
      period: 1m
      tokenRate: 250000   # optional: max cumulative tokens per window (LLM only)
      tokenPeriod: 1m
    user:
      rate: 20
      period: 1m
    "*":                  # fallback when user_type is absent or unmatched
      rate: 10
      period: 1m
```

Each entry may also carry `maxConcurrent` (async jobs in flight) and `processingTime`/`processingPeriod` (cumulative inference seconds per window). Returns `429 Too Many Requests` with `Retry-After` when exceeded.

#### Per-model token budgets (`tokenLimits`)

A service (model) can carry its own token budget, enforced independently of `rateLimits` — both must pass. Because it uses a **separate window**, you can combine a real-time cap in `rateLimits` (e.g. tokens/minute) with a longer budget here (e.g. tokens/day):

```yaml
# values.yaml
services:
  - type: llm
    model: chat-pro
    # ...
    tokenLimits:
      sa:
        tokenRate: 30000000   # tokens per day for this model
        tokenPeriod: 24h
      user:
        tokenRate: 500000
        tokenPeriod: 24h
```

`"*"` acts as the fallback user type; only `tokenRate`/`tokenPeriod` are used. Since it is per-service, the budget applies per model.

### Authentication (optional)

Gateway-side OAuth2 access-token validation. Absent `auth` block = no gateway auth (identity trusted from upstream headers). Auth config changes require a pod restart.

| Parameter | Description | Default |
|---|---|---|
| `auth.mode` | `oauth2` — validate tokens; `proxy` — trust upstream identity headers | `""` (disabled) |
| `auth.oauth2.issuer` | IdP issuer URL (used for JWKS discovery) | |
| `auth.oauth2.jwksUrl` | Explicit JWKS endpoint (overrides discovery) | `""` |
| `auth.oauth2.audiences` | Required `aud` claim values | `[]` |
| `auth.oauth2.validation` | `auto` (JWT locally, opaque via introspection) \| `jwt` \| `introspection` | `auto` |
| `auth.oauth2.introspection.clientId` | Client ID for RFC 7662 introspection (supports `${VAR}`) | |
| `auth.oauth2.introspection.clientSecret` | Client secret (supports `${VAR}`) | |
| `auth.oauth2.introspection.cacheTtl` | How long to cache introspection results | `60s` |
| `auth.oauth2.claims.consumer` | JWT claim mapped to consumer identity | `preferred_username` |
| `auth.oauth2.claims.scopes` | JWT claim for scopes | `scope` |
| `auth.oauth2.claims.groups` | JWT claim for groups | `groups` |
| `auth.oauth2.claims.roles` | JWT claim for roles | `roles` |
| `auth.proxy.consumerHeader` | Header carrying consumer identity (upstream proxy mode) | |
| `auth.proxy.userTypeHeader` | Header carrying user type | |
| `auth.proxy.groupsHeader` | Header carrying groups | |
| `auth.proxy.rolesHeader` | Header carrying roles | |

### Access control (optional)

Default-deny model/service allowlist. Requires `auth.mode` to be set.

| Parameter | Description |
|---|---|
| `policies.default` | `deny` (default-deny) \| `allow` (disabled) |
| `policies.rules[]` | Allow rules — each grants a set of callers access to specific models |
| `policies.rules[].match.groups` | Groups allowed by this rule (any-of) |
| `policies.rules[].match.roles` | Roles allowed |
| `policies.rules[].match.scopes` | Scopes allowed |
| `policies.rules[].match.consumers` | Consumer identities allowed |
| `policies.rules[].match.user_types` | User types allowed |
| `policies.rules[].allow_models` | Glob patterns for allowed model names |
| `policies.rules[].allow_service_types` | Allowed service types (optional) |
| `policies.rules[].limits.rate` | Per-member request rate limit for this group | |
| `policies.rules[].limits.period` | Rate limit window (e.g. `1m`) | |
| `policies.rules[].limits.token_rate` | Per-member token budget | |
| `policies.rules[].limits.token_period` | Token budget window | |

Enforced on both sync (`POST /v1/*`) and async (`POST /jobs/*`) after model resolution. Hot-reloadable. Metric: `gatewai_authz_decisions_total{service_type, model, decision}`.

### OpenTelemetry

Distributed tracing and OTLP push for traces, metrics, and logs.

| Parameter | Description | Default |
|---|---|---|
| `opentelemetry.enabled` | Enable OTel instrumentation | `false` |
| `opentelemetry.serviceName` | `service.name` resource attribute | `gatewai/gateway` |
| `opentelemetry.exporter.endpoint` | OTLP/HTTP endpoint | `http://<release>-otlp:4318` |
| `opentelemetry.exporter.insecure` | Disable TLS verification | `true` |
| `opentelemetry.traces.enabled` | Export traces | `true` |
| `opentelemetry.traces.sampleRatio` | Sampling ratio (0.0–1.0) | `1.0` |
| `opentelemetry.metrics.enabled` | Push metrics via OTLP | `false` |
| `opentelemetry.metrics.interval` | Push interval | `60s` |
| `opentelemetry.logs.enabled` | Forward structured logs via OTLP | `false` |

**Option A — bundled OTel Collector** (`otlp.enabled: true`): deploys the `opentelemetry-collector` sub-chart alongside the gateway (aliased `otlp`). Configure `otlp.config` with your exporters.

**Option B — Operator CRD** (`otlpOperator.enabled: true`): creates an `OpenTelemetryCollector` CR; requires the OTel Operator to be installed on the cluster.

### Extra environment variables

Arbitrary environment variables injected into the gateway container after the chart-managed ones. Accepts any Kubernetes env syntax (`value`, `valueFrom`, `secretKeyRef`, …).

| Parameter | Description | Default |
|---|---|---|
| `extraEnvVars` | List of additional env var entries | `[]` |

Example — inject a GitHub token from an existing Secret (used by `swaggerHeaders`):

```yaml
extraEnvVars:
  - name: GITHUB_TOKEN
    valueFrom:
      secretKeyRef:
        name: github-token
        key: token
```

### Config hot reload

The gateway exposes `POST /-/reload` to reload its configuration at runtime. Calling this endpoint re-reads `config.yaml`, rebuilds the service registry, Swagger specs, OpenAPI spec, and routing table — without pod restart.

> **Note:** S3 and Redis connection parameters are not reloaded on hot reload.

The chart can deploy a [`configmap-reload`](https://github.com/jimmidyson/configmap-reload) sidecar that watches the ConfigMap volume and triggers `/-/reload` automatically whenever the ConfigMap is updated (e.g. via GitOps or `kubectl edit`).

| Parameter | Description | Default |
|---|---|---|
| `configReloader.enabled` | Deploy the configmap-reload sidecar | `false` |
| `configReloader.image` | Sidecar image | `ghcr.io/jimmidyson/configmap-reload:v0.14.0` |
| `configReloader.listenPort` | Port for the sidecar metrics/health server | `9533` |

Example:

```yaml
configReloader:
  enabled: true
```

### Redis HA

The chart includes [redis-ha](https://github.com/DandyDeveloper/charts/tree/master/charts/redis-ha) as a subchart. HAProxy is enabled by default to expose a stable master endpoint.

| Parameter | Description | Default |
|---|---|---|
| `redis-ha.enabled` | Deploy Redis HA | `true` |
| `redis-ha.haproxy.enabled` | Enable HAProxy frontend | `true` |
| `redis-ha.replicas` | Redis replica count | `3` |
| `redis-ha.redis.config.appendonly` | AOF persistence | `"yes"` |
| `redis-ha.redis.config.appendfsync` | AOF fsync policy | `"everysec"` |
| `redis-ha.persistentVolume.enabled` | PVC for AOF/RDB files | `true` |

**Durability:** async state lives entirely in Redis — job records, the relay `pending`/`processing` queues, per-job leases, and the webhook retry queue. The chart enables **AOF** (`appendfsync everysec`, ≤1s loss window) and **RDB** snapshots on a PersistentVolume so this state survives a Redis pod restart or master failover. Without persistence, a master crash can lose recently-enqueued jobs and their lease/webhook state. Disable only for ephemeral/dev clusters.

## API reference

### Async

```
POST /jobs/{service_type}
  Content-Type: multipart/form-data
  Fields:
    file         — binary (required)
    model        — model name (required if multiple models for the type, no default)
    operation    — operation name (required if multiple operations, no default)
    callback_url — webhook URL called on completion (optional)

GET /jobs/{service_type}/{id}
  Returns: { job_id, status, model, result, error, created_at, updated_at }
  Note: result S3 file is deleted after this call — subsequent calls return 404.
```

> `POST /jobs/{service_type}` returns **405** for services without a relay queue (sync-direct only).

### Sync (OpenAI-compatible)

```
POST /v1/<operation-path>
  Body: same as OpenAI API; "model" field selects the backend
  Routing:
    any content-type → direct proxy to inferenceURL (or selected backend)
```

### Other endpoints

```
GET  /health        → { "status": "ok", "time": "..." }
GET  /metrics       → Prometheus text format
GET  /openapi.yaml  → OpenAPI 3.0.3 spec (generated at startup from registry)
GET  /docs          → Swagger UI
POST /-/reload      → reload config at runtime (204 on success, 500 on error)
```

## Monitoring (Prometheus Operator)

A `ServiceMonitor` can be created automatically if the [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator) is installed in the cluster.

| Parameter | Description | Default |
|---|---|---|
| `metrics.serviceMonitor.enabled` | Create a `ServiceMonitor` | `false` |
| `metrics.serviceMonitor.namespace` | Namespace for the `ServiceMonitor` (defaults to release namespace) | `""` |
| `metrics.serviceMonitor.interval` | Scrape interval | `30s` |
| `metrics.serviceMonitor.scrapeTimeout` | Scrape timeout | `10s` |
| `metrics.serviceMonitor.additionalLabels` | Extra labels on the `ServiceMonitor` — use to match the Prometheus Operator `serviceMonitorSelector` | `{}` |
| `metrics.serviceMonitor.relabelings` | Prometheus `relabelings` rules | `[]` |
| `metrics.serviceMonitor.metricRelabelings` | Prometheus `metricRelabelings` rules | `[]` |

Example with `kube-prometheus-stack`:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack   # matches Prometheus Operator selector
    interval: 30s
```

### Gateway metrics

| Metric | Type | Labels |
|---|---|---|
| `GatewAI_requests_total` | counter | `mode`, `service_type`, `model`, `status` |
| `GatewAI_request_duration_seconds` | histogram | `mode`, `service_type`, `model` |
| `GatewAI_sync_wait_duration_seconds` | histogram | `service_type`, `model` |
| `GatewAI_sync_jobs_in_flight` | gauge | — |
| `GatewAI_s3_operation_duration_seconds` | histogram | `operation` |
| `GatewAI_s3_errors_total` | counter | `operation` |
| `GatewAI_redis_operation_duration_seconds` | histogram | `operation` |
| `GatewAI_redis_errors_total` | counter | `operation` |
| `GatewAI_jobs_by_consumer_total` | counter | `mode`, `service_type`, `model`, `consumer` |
| `GatewAI_llm_requests_total` | counter | `service_type`, `model`, `backend_model`, `provider`, `user_type`, `status` |
| `GatewAI_llm_request_duration_seconds` | histogram | `service_type`, `model`, `backend_model`, `provider`, `user_type` |
| `GatewAI_llm_tokens_total` | counter | `service_type`, `model`, `backend_model`, `user_type`, `type` |
| `GatewAI_llm_tokens_per_request` | histogram | `service_type`, `model`, `backend_model`, `user_type` |
| `GatewAI_llm_consumer_tokens_top` | gauge | `consumer`, `user_type`, `type` |
| `GatewAI_cache_hits_total` | counter | `service_type`, `model` |
| `GatewAI_cache_misses_total` | counter | `service_type`, `model` |
| `GatewAI_cache_errors_total` | counter | `service_type`, `model`, `operation` |
| `GatewAI_ratelimit_requests_total` | counter | `service_type`, `user_type`, `result` |
| `GatewAI_ratelimit_consumer_hits_total` | counter | `service_type`, `user_type`, `consumer` |
| `GatewAI_ratelimit_errors_total` | counter | `service_type` |

### Relay sidecar metrics

The relay sidecar exposes its own `/metrics` endpoint (scraped separately, e.g. via a PodMonitor on port 8080 of the InferenceService pod).

| Metric | Type | Labels |
|---|---|---|
| `GatewAI_relay_jobs_total` | counter | `service_type`, `status` |
| `GatewAI_relay_inference_duration_seconds` | histogram | `service_type`, `model` |
| `GatewAI_relay_input_size_bytes` | histogram | `service_type` |
| `GatewAI_relay_sync_priority` | gauge | — |
| `GatewAI_relay_deferred_total` | counter | — |
| `GatewAI_relay_s3_operation_duration_seconds` | histogram | `operation` |
| `GatewAI_relay_s3_errors_total` | counter | `operation` |
| `GatewAI_relay_proxy_requests_total` | counter | `service_type`, `status` |
| `GatewAI_relay_proxy_duration_seconds` | histogram | `service_type` |

## Upgrade guide

```bash
# 1. Pull the latest chart
helm repo update

# 2. Review breaking changes — table below, and the version-specific notes further down
#    https://github.com/Qatr-io/GatewAI/blob/main/CHANGELOG.md

# 3. Upgrade
helm upgrade gatewai-gateway GatewAI/gatewai-gateway -f values.yaml

# 4. Verify rollout
kubectl rollout status deployment/gatewai-gateway
kubectl logs -l app=gatewai-gateway --tail=50

# Rollback if needed
helm rollback gatewai-gateway
```

### Breaking changes at a glance

| Version | Change |
|---|---|
| 0.23.0 | `GET /v1/models`'s `backendModel` no longer reflects a per-backend override; `backendModels[]` removed (see [0.22.0 → 0.23.0](#0220--0230)) |
| 0.20.1 | `PrometheusRule` alerts renamed, dropped the legacy `Kevent` prefix |
| 0.17.0 | `guardrails.pii` boolean removed — use `guardrails.checks` |
| 0.15.0 | Go module & Helm chart renamed: `kevent-gateway` → `gatewai-gateway` |
| 0.14.0 | Kafka removed entirely — `syncTopic`/`syncPriority` also removed (0.13.0) |
| 0.11.0 | `redis.pendingMaxAgeHours` → `redis.pendingMaxAge` (duration string); `lifecycle.jobTTL.success` → `lifecycle.jobTTL.completed` |
| 0.8.0 | LLM metrics gained a `backend_model` label — update PromQL `by`/`without` clauses |
| 0.7.0 | `api_key` removed from service config — use `inferenceHeaders` |

**Upcoming:** the inline `services[].backends` list is deprecated in favor of [`backendPools`/`backendPool`](#backend-pools-backendpools) and will be removed in a future release — migrate now.

### 0.1.0 → 0.2.0

- `openai_path` (string) renamed to `openai_paths` (list) in service config
- `inference_url` is now a base URL; the original request path is appended at runtime
- At-rest AES-256-GCM encryption added (`encryption.key` / `encryption.existingSecret`)

### 0.2.x → 0.3.0

- `openai_paths` (flat list) replaced by `operations` map (`operationName → [paths]`)
- `syncTopic` field added per service (enables sync-direct proxy for multipart `POST /v1/*`)

### 0.3.x → 0.5.x

- `config.existingConfigMap` option added — reference an external ConfigMap instead of letting the chart create one
- Services without relay queue are valid (sync-direct only mode)
- `acceptedExts` empty = all extensions accepted (previously would reject all files)
- `maxFileSizeMB: 0` or absent = 100 MB default (previously 0 meant no limit)

### 0.5.x → 0.5.15

- `swaggerHeaders` field added per service — optional HTTP headers for authenticated `swaggerURL` fetching
- `extraEnvVars` added — inject arbitrary env vars (e.g. secrets) into the gateway container
- `configReloader` section added — optional `configmap-reload` sidecar for automatic hot reload on ConfigMap update
- `POST /-/reload` endpoint: reloads services, Swagger specs, OpenAPI spec at runtime

### 0.5.15 → 0.7.0

- **LLM proxy** added — `provider`, `backendModel`, `responseCacheTTL` fields per service
- **Response caching** — Redis-backed, keyed on canonical SHA-256 of request body
- **Per-consumer rate limiting** — fixed-window Redis rate limiting via `rate_limits` in config
- **Consumer metrics** — `metricsConfig.topConsumers` exposes top-N LLM consumers via Redis sorted sets
- **SSE streaming** support for LLM proxy (`"stream": true` requests)
- `inferenceHeaders` field added per service — inject auth headers on backend requests

### 0.7.0 → 0.8.0

**Breaking change:** LLM metrics now include a `backend_model` label. Existing PromQL queries and dashboard panels targeting `GatewAI_llm_requests_total`, `GatewAI_llm_request_duration_seconds`, `GatewAI_llm_tokens_total`, or `GatewAI_llm_tokens_per_request` must be updated to include `backend_model` in `by`/`without` clauses, or use `{backend_model=~".*"}` as a wildcard.

- **Multi-backend routing** — `backends[]` list per service with weighted-random primary selection, automatic fallback on 5xx/network error, and `weight: 0` for last-resort backends
- **Per-backend `model` override** — `backends[].model` overrides `backendModel` for a specific backend; enables canary deployments with different model versions
- **Per-backend `headers` override** — `backends[].headers` overrides `inferenceHeaders` for a specific backend; enables per-backend authentication tokens
- **`backend_model` label** on all 4 LLM metrics — identifies which backend model version served each request

### 0.8.0 → 0.11.0

**Breaking changes:**

- `redis.pending_max_age_hours` (integer, hours) renamed to `redis.pending_max_age` (duration string). Update your values or `config.yaml`:
  ```yaml
  # Before:
  redis:
    pending_max_age_hours: 2
  # After:
  redis:
    pending_max_age: "2h"
  ```
  The env var also changes: `PENDING_MAX_AGE_HOURS` → `PENDING_MAX_AGE` (value becomes e.g. `"2h"`).

- `lifecycle.jobTTL.success` renamed to `lifecycle.jobTTL.completed`. Update `values.yaml` if set:
  ```yaml
  # Before:
  lifecycle:
    jobTTL:
      success: "24h"
  # After:
  lifecycle:
    jobTTL:
      completed: "24h"
  ```

**New optional parameters:**
- `lifecycle.gc.enabled` / `lifecycle.gc.interval` / `lifecycle.gc.orphanMinAge` — unified GC (off by default, see [Lifecycle and job retention](#lifecycle-and-job-retention))

### 0.17.0 → 0.18.0

No breaking changes.

**New optional parameters:**
- `auth` — gateway-side OAuth2 authentication (see [Authentication](#authentication-optional))
- `policies` — policy-based default-deny access control (see [Access control](#access-control-optional))
- `opentelemetry` — OTel tracing/metrics/logs export (see [OpenTelemetry](#opentelemetry))
- `otlp` — bundled OTel Collector sub-chart
- `otlpOperator` — OTel Operator CRD option

### 0.18.0 → 0.19.0

No breaking changes.

**New features:**
- **Generic per-service usage tracking** — request/activity/token counters per consumer, for every service type (not just LLM). Exposed via `GET /usage` (self-service) and `GET /-/usage` (admin). See [Usage tracking](#usage-tracking).
- **Token budget enforcement extended to async jobs** — `token_limits` now also applies pre-flight to `POST /jobs/{service_type}` and is recorded on async job completion, not just the sync LLM path.

**New optional parameters:**
- `usage.retention`

### 0.19.0 → 0.20.0

No breaking changes for the chart itself. Requires the relay to be redeployed with `gateway.base_url` set (see the relay's own release notes) once the new gateway image is live.

### 0.20.0 → 0.20.1

No breaking changes.

**New optional parameters:**
- `alerting.prometheusRule.thresholds.jobsPendingFor` (default `15m`) / `.jobsRunningFor` (default `1h`) — two new `PrometheusRule` alerts, `RelayJobsPendingTooLong` and `RelayJobsRunningTooLong`, based on `gatewai_relay_queue_depth{state}` and `gatewai_jobs_total`. See `values.yaml` (`alerting.prometheusRule.thresholds`) and the [alerting docs](https://qatr-io.github.io/GatewAI/observe/prometheus/#alerts) for the full alert list.

All shipped alerts also lost their legacy `Kevent` name prefix (e.g. `KeventGatewayHighErrorRate` → `GatewayHighErrorRate`) — update any existing Alertmanager routes/silences matching on the old alert names.

**New features:**
- **`GET /-/usage/report`** (admin) — cross-consumer, calendar-aligned usage totals per service type, for finance/BI reporting; optional `total=true` sums all buckets.
- **`POST /-/quota/reset`** (admin) — clears a consumer's rate-limit/token-budget Redis keys for one service type.
- **`gatewai_relay_queue_depth{model,state}`** — new gauge exposing live relay queue depth.

### 0.20.1 → 0.21.0

No breaking changes.

**New features:**
- **`backend_model` exposed in `GET /v1/models`** — each model object now carries the real backend model it forwards to; a `backendModels[]` array lists every distinct backend model when a canary/mixed fleet serves one alias (**note:** this array was removed again in 0.23.0, see below).
- **`services[].visibility`** — gate a model to specific `userTypes`/`groups`; hidden models are indistinguishable from non-existent ones for callers outside the audience.
- **Processing-queue crash recovery (lease reaper)** — abandoned async jobs are requeued (then dead-lettered after `lifecycle.gc.maxReapAttempts`) instead of stuck forever.
- **HMAC-signed webhooks** (`webhooks.signingSecret`) — `X-Gatewai-Signature` header for authenticity + replay protection.
- **Distinct `410 Gone` / `expired` status** on `GET /jobs/{type}/{id}` once a job's retention TTL has passed (`jobs.expiredMarkerTtl`).
- **Durable webhook delivery** — retries persist in Redis instead of an in-memory goroutine; dead-lettered after `webhooks.maxRetries`.
- **Idempotency keys** on `POST /jobs/{type}` (`Idempotency-Key` header, `jobs.idempotencyTtl`).

**Changed (non-breaking, but affects storage):**
- **Redis persistence (AOF + RDB) is now enabled by default** in the bundled `redis-ha` subchart. Existing installs get a `PersistentVolume` on upgrade unless overridden — set `redis-ha.persistentVolume.enabled: false` to opt out (not recommended, see [Redis HA](#redis-ha)).

### 0.21.0 → 0.22.0

No breaking changes.

**New features:**
- **Model-backed guardrail detectors** (`services[].guardrails.models[]`) alongside the existing regex checks, with per-mode behaviour (`async`/shadow vs `sync`/inline) and a verdict cache.
- **Output-stage guardrails** (`services[].guardrails.output`) with streaming enforcement (`flag`/`block`/`buffer`).
- **Async result-stage guardrails** (`services[].guardrails.async`) — scans async job results (transcript, OCR, …).
- **Per-backend circuit breaker** (`circuitBreaker`, opt-in) with optional active health probing; a model with all backends open is marked `capabilities.degraded: true`.
- **Cross-model fallback** (`services[].fallbackModel`, opt-in).

### 0.22.0 → 0.23.0

**Breaking change:** `GET /v1/models`'s `backendModel` now sources **only** the service-level `backendModel` — a per-backend override (`backends[].model` or `backendPools[].members[].model`, for canary/mixed-fleet setups) is no longer surfaced in the models listing, though it still rewrites the outgoing request to that backend. The `backendModels[]` array (added in 0.21.0) is removed accordingly — update any tooling that parsed it.

**New features:**
- **`backendPools`** — see [Backend pools](#backend-pools-backendpools) above.
- **Guardrail trace tagging** (`guardrail.flagged=true` + stage/action/detectors span attributes) and **flagged-sample records** for reviewing async/shadow detections.

### Upcoming: `backends` removal

The inline `services[].backends` list is **deprecated** as of 0.23.0 in favor of [`backendPools`/`backendPool`](#backend-pools-backendpools) — see the field reference above. It's still fully supported today, but will be removed in a future release. Migrate new and existing services now, including single-alias canary/blue-green/fallback setups (a pool with one referencing service still works).
