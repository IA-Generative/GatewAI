---
title: Configuration reference
---

# Configuration reference

Configuration is loaded from `config.yaml` (or `$CONFIG_PATH`). Values of the form `${VAR}` or `${VAR:-default}` are expanded from the environment.

## Server

```yaml
server:
  addr: ":8080"
  read_timeout: 120s
  write_timeout: 0s        # 0 = no timeout (recommended for long sync jobs)
  idle_timeout: 120s
  max_body_mb: 1           # cap on the sync JSON body (POST /v1/*); 0 or absent = 1 MiB
  consumer_header: ""      # header name for consumer identification (e.g. X-Consumer-Username)
  priority_header: ""      # header name for priority routing (e.g. X-Priority)
  user_type_header: ""     # header name for user type (e.g. X-User-Type) — rate limiting + LLM metrics
```

### `max_body_mb`

Caps the request body (in MiB) on the **sync JSON path** (`POST /v1/*`), enforced with `http.MaxBytesReader` — oversized requests get a clean **`413 Request Entity Too Large`** instead of being silently truncated. `0` or absent means the **1 MiB default**.

Raise it for **vision models** that receive base64-embedded images: base64 inflates binary by ~33%, so a 30 MB image needs a cap around `45`. Helm exposes this as `server.maxBodyMB`.

This is independent of `services[].max_file_size_mb`, which bounds only the **multipart upload** path (async `POST /jobs/{service_type}`), not the sync JSON body.

### `consumer_header`

When set to a non-empty header name (e.g. `X-Consumer-Username`, typically injected by APISIX after authentication):

- The consumer name is stored in the job record (`consumer_name` field in Redis).
- A Redis sorted set `consumer:{name}:jobs` is maintained per consumer (score = creation timestamp, same TTL as the job). Used by `GET /jobs`.
- `GET /jobs/{service_type}/{id}` enforces **ownership**: if the header is present in the request, `job.consumer_name` must match — returns `404` on mismatch (no information leak about other consumers' jobs).
- `GatewAI_jobs_by_consumer_total{mode, service_type, model, consumer}` is incremented per submission.

Leave empty in deployments without upstream authentication — no behaviour change, zero overhead.

### `priority_header`

When set and a request carries this header, the job is inserted at the **head** of the Redis queue (`LPUSH relay:<model>:pending`) instead of the tail (`RPUSH`). The relay always pops from the head (`BLMOVE LEFT RIGHT`), so priority jobs are picked up first by the next available pod.

Leave empty to disable priority routing.

### `user_type_header`

HTTP header injected by the upstream API gateway (e.g. APISIX, OPA) after token introspection. Typical value: `X-User-Type`. The header value (e.g. `sa`, `user`) is used:

- As a label on all LLM request/token/duration metrics (`user_type`)
- To select the rate limit tier from `rate_limits[service_type][user_type]`

Leave empty to disable user-type differentiation — the `"*"` fallback tier applies for rate limiting.

## Metrics

```yaml
metrics:
  top_consumers: 10        # expose top-N consumers in Prometheus; 0 = disabled
  consumer_labels: false   # direct per-consumer Prometheus labels (< 50 consumers only)
```

### `top_consumers`

When set to a positive integer, enables Redis sorted-set tracking of per-consumer LLM token usage. A background goroutine reads the top-N consumers every 60 seconds (and immediately at startup) and exposes them as `GatewAI_llm_consumer_tokens_top{consumer, user_type, type}`.

Only suitable when `server.consumer_header` is configured. Requires `server.user_type_header` for `user_type` labelling.

:::caution
Do **not** use `consumer_labels: true` with more than ~50 consumers. Each consumer creates a new Prometheus time series; at 100 k+ consumers this causes OOM. Use `top_consumers` instead.
:::

:::note
Token metrics (and `top_consumers`) are only populated when the LLM backend's response is OpenAI-compatible — i.e. it (or its translation, for `provider: anthropic`) contains a top-level `usage.prompt_tokens` / `usage.completion_tokens` object. With `provider: passthrough`, a backend that omits this `usage` shape is silently skipped: no error, but no tokens counted. See [LLM proxy](./llm-proxy).
:::

## Rate limits

```yaml
rate_limits:
  audio:
    sa:
      rate: 100
      period: 1m
      max_concurrent: 20         # optional: pending+processing async jobs at once
      processing_time: 36000     # optional: cumulative inference seconds per window
      processing_period: 24h
    user:
      rate: 20
      period: 1m
      token_rate: 500000     # optional: token budget per window (sync LLM only)
      token_period: 24h
      max_concurrent: 5
      processing_time: 3600
      processing_period: 24h
    "*":             # fallback: user_type absent or not listed
      rate: 10
      period: 1m
  ocr:
    "*":
      rate: 5
      period: 1m
```

Per-consumer, per-service fixed-window rate limiting backed by Redis. Returns `429 Too Many Requests` with a `Retry-After` header when exceeded.

| Field | Description |
|---|---|
| Key (e.g. `audio`) | Matches `services[].type` — all models of the same type share the counter |
| Sub-key (e.g. `sa`) | User type from `server.user_type_header`; `"*"` is the catch-all fallback |
| `rate` | Maximum requests allowed in the `period` |
| `period` | Window duration: `30s`, `1m`, `1h`, `24h` |
| `token_rate` | Optional: maximum total LLM tokens per `token_period`; applies on sync LLM requests only |
| `token_period` | Window duration for the token budget: `1h`, `24h` |
| `max_concurrent` | Optional: maximum pending+processing async jobs at the same time per consumer; `0` = disabled |
| `processing_time` | Optional: maximum cumulative inference seconds per `processing_period`; async only; `0` = disabled |
| `processing_period` | Window duration for the processing-time budget: `1h`, `24h` |

Leave `rate_limits` empty to disable. See [Rate limiting](../configure/rate-limiting) for details.

:::note
`token_rate` / `token_period` are enforced from the LLM response's `usage.prompt_tokens` / `usage.completion_tokens` fields, so they require an OpenAI-compatible response body (see the note under [Metrics](#metrics) above). `max_concurrent` and `processing_time` are unaffected — they don't depend on response content.
:::

## Token limits per model

For finer-grained control, token budgets can be set **per model** under `services[].token_limits`. This is the recommended approach when several models share the same `type` but should have independent quotas.

```yaml
services:
  - type: llm
    model: gpt-4o
    provider: openai
    inference_url: "https://api.openai.com"
    token_limits:
      user:
        token_rate: 100000
        token_period: 24h
      sa:
        token_rate: 1000000
        token_period: 1h
      "*":
        token_rate: 50000
        token_period: 24h
```

When both `rate_limits[type][user_type].token_rate` and `services[].token_limits[user_type].token_rate` are configured, **both** checks must pass. The model-level limit is enforced first.

## S3

```yaml
s3:
  endpoint: "${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
  region: "${S3_REGION:-fr-par}"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket: "${S3_BUCKET:-GatewAI-jobs}"
```

## Encryption

```yaml
encryption:
  key: "${ENCRYPTION_KEY:-}"   # hex-encoded 32-byte AES-256 key; empty = disabled
```

Generate a key:

```bash
openssl rand -hex 32
```

:::caution
The encryption key must be identical on all gateway and relay instances.
:::

## Redis

```yaml
redis:
  addr: "${REDIS_ADDR:-redis:6379}"
  password: "${REDIS_PASSWORD:-}"
  db: 0
  pending_max_age: "${PENDING_MAX_AGE:-2h}"   # duration string; empty or "0s" = disabled
```

`pending_max_age` controls the stale-pending sweep (GC Phase 1): jobs still in `pending` state after this duration are marked failed and their S3 input files are deleted. Must be shorter than the S3 input object lifetime.

## Lifecycle

```yaml
lifecycle:
  persists_result: false        # true = keep records until TTL; false = delete on first GET or webhook
  job_ttl:
    global:    "${LIFECYCLE_JOB_TTL:-}"           # fallback for all statuses; empty = 2h safety net
    completed: "${LIFECYCLE_JOB_TTL_COMPLETED:-}" # override for completed jobs
    pending:   "${LIFECYCLE_JOB_TTL_PENDING:-}"   # override for pending/processing jobs
    failed:    "${LIFECYCLE_JOB_TTL_FAILED:-}"    # override for failed jobs
  gc:
    enabled:       ${LIFECYCLE_GC_ENABLED:-false}
    interval:      "${LIFECYCLE_GC_INTERVAL:-15m}"
    orphan_min_age: "${LIFECYCLE_GC_ORPHAN_MIN_AGE:-5m}"
```

### `persists_result`

When `false` (default): Redis record and S3 result file are deleted immediately after first consumption (first `GET /jobs/{id}` that returns a completed result, or first successful webhook delivery). Minimises storage usage.

When `true`: records survive until their TTL expires. Clients can re-fetch the result multiple times within the TTL window.

### `job_ttl`

Per-status Redis TTL for job records. `global` is the fallback for all statuses; per-status values take precedence. When all values are empty, an internal 2h safety net applies to orphaned records.

Example — keep completed results for 24h, fail quickly:

```yaml
lifecycle:
  job_ttl:
    global:    "2h"
    completed: "24h"
```

### `gc`

Background garbage collector — runs as a goroutine on a configurable ticker. Four phases per tick:

**Phase 0 — processing-queue reaper** (runs when `gc.enabled: true`):
Requeues async jobs abandoned in `relay:<model>:processing` by a relay pod that died mid-job (OOM, node loss, SIGKILL) without releasing its **lease**. Each job the relay pops carries a short-lived lease key (`relay:<model>:lease:<id>`, TTL `lease_ttl`, refreshed every `lease_ttl/3`); when the pod dies the lease expires. For each processing entry with no live lease and a still-valid job record, the reaper requeues it to `relay:<model>:pending` (up to `gc.max_reap_attempts` times, then **dead-letters** it to `relay:<model>:deadletter` and marks the job failed). The lease check is atomic with the reclaim (Lua), so a healthy worker's job is never requeued, and it is idempotent across gateway replicas. Counted in `gatewai_async_jobs_reaped_total{model,outcome}`. This complements Phase 3: Phase 0 handles orphans whose record is still live (crash recovery), Phase 3 removes orphans whose record is already gone (cleanup). See [Relay queue](../set-up/queue#crash-recovery).

**Phase 1 — stale-pending sweep** (runs when `redis.pending_max_age` > 0):
Marks pending jobs older than `pending_max_age` as failed and deletes their S3 input files.

**Phase 2 — S3 orphan cleanup** (runs when `gc.enabled: true`):
Lists all S3 objects, groups by job ID (first path segment), and deletes any file whose Redis record has expired. Covers both `input_ref` and `result_ref` orphans — catches failed webhooks, `persists_result: true` jobs after TTL, and never-polled results.

**Phase 3 — relay queue orphan cleanup** (runs when `gc.enabled: true`):
Removes job IDs from `relay:<model>:pending` and `relay:<model>:processing` whose Redis job record has expired. A relay pod that exits before calling `Done` — e.g. `os.Exit(1)` on an infra error reading the job, or during inference — never removes its own entry, and with one-pod-per-job scaling no later pod picks that job back up to finish the cleanup. Left unswept, that entry inflates `gatewai_relay_queue_depth` forever, even once nothing is actually running. Swept entries are counted in `gatewai_relay_queue_orphans_swept_total{model,state}`. See [Relay queue](../set-up/queue#stale-processing-list) for the underlying failure mode and manual recovery.

Safety: if Redis `Ping` or `MGET` fails before any deletion, phases 2 and 3 are aborted entirely and an error is logged. This prevents mass deletion when Redis is temporarily unavailable.

All `gc` parameters are hot-reload safe.

| Field | Default | Description |
|---|---|---|
| `gc.enabled` | `false` | Master switch — GC is off by default |
| `gc.interval` | `"15m"` | How often the GC runs |
| `gc.orphan_min_age` | `"5m"` | Minimum object age before orphan check — avoids deleting objects from in-flight uploads not yet registered in Redis |
| `gc.max_reap_attempts` | `3` | Phase 0: times a lease-expired processing job is requeued before it is dead-lettered to `relay:<model>:deadletter` |

## Webhooks

Durable delivery of job-completion callbacks (`callback_url`). On completion the gateway makes one inline `POST`; on failure (network error or `5xx`) the retry is **persisted in Redis** (a `webhook:retries` sorted set plus a per-job task key) and worked by a background loop with exponential backoff — so a gateway restart never drops pending retries. After `max_retries` attempts the webhook is **dead-lettered** to the `webhook:deadletter` list. A `4xx` response counts as delivered (the consumer received it) and is not retried.

```yaml
webhooks:
  max_retries: 3          # total attempts (incl. the inline one) before dead-letter
  retry_backoff: "30s"    # base backoff; doubles each retry
  max_backoff: "10m"      # backoff cap
  signing_secret: ""      # HMAC secret; empty = unsigned
```

| Field | Default | Description |
|---|---|---|
| `max_retries` | `3` | Total delivery attempts (including the first inline attempt) before dead-lettering |
| `retry_backoff` | `"30s"` | Base delay before the first retry; doubles on each subsequent retry |
| `max_backoff` | `"10m"` | Upper bound on the exponential backoff |
| `signing_secret` | `""` | When set, signs every webhook — see [signing](#webhook-signing). Supports `${VAR}`. Empty = unsigned |

### Webhook signing

When `signing_secret` is set, every delivery carries an `X-Gatewai-Signature` header so consumers can verify authenticity and reject replays:

```
X-Gatewai-Signature: t=<unix_seconds>,v1=<hex>
```

where `v1 = HMAC-SHA256(secret, "<t>.<raw_body>")`. The timestamp is fresh per attempt. Consumer verification:

```
t, v1 = parse(header)
reject if abs(now - t) > tolerance          # replay protection
expected = hex(hmac_sha256(secret, f"{t}.{raw_body}"))
accept if hmac_equal(expected, v1)
```

Metrics: `gatewai_webhook_deliveries_total{result}` (`delivered`|`deadletter`) and `gatewai_webhook_retry_queue_depth`.

## Jobs

Async submission behaviour (`POST /jobs/{service_type}`).

```yaml
jobs:
  idempotency_ttl: "24h"       # Idempotency-Key → job mapping retention
  expired_marker_ttl: "168h"   # "job existed" tombstone TTL (410 vs 404 on poll)
```

| Field | Default | Description |
|---|---|---|
| `idempotency_ttl` | `"24h"` | How long an `Idempotency-Key` → job mapping is remembered. A repeat submission with the same key (scoped per consumer) returns the original job instead of starting a duplicate inference — see [Idempotency](#idempotency) |
| `expired_marker_ttl` | `"168h"` | How long a lightweight `jobmeta:<id>` tombstone is kept. While it lives, polling a job whose record TTL has passed returns `410` (status `expired`) instead of `404`. Should exceed the job record TTL |

### Idempotency

Send an optional `Idempotency-Key` header on `POST /jobs/{service_type}`. The key is reserved in Redis (`idem:{consumer}:{key}`, `SETNX`) against the new job's ID just before the record is saved:

- **New key** → the job is created (`202`).
- **Repeat key, job still available** → the original job is returned (`200` + `X-Idempotent-Replay: true`), with **no** second inference.
- **Repeat key, job gone** (expired / its save failed) → `409`.

Scoped per consumer, so distinct or absent keys never collide — dedup only fires on a *reused* key (a client retry of one request). Metric: `gatewai_idempotency_requests_total{service_type,outcome}` (`created`|`replayed`|`conflict`).

## Services

```yaml
backend_pools:
  reranker-pool:
    members:
      - url: "http://backend-primary:8000"
        weight: 90
        model: "meta-llama/Meta-Llama-3-8B-Instruct"   # per-member model override
        headers:
          Authorization: "Bearer ${PRIMARY_TOKEN}"       # per-member header override
      - url: "http://backend-canary:8000"
        weight: 10
        model: "meta-llama/Meta-Llama-3.1-8B-Instruct"
        headers:
          Authorization: "Bearer ${CANARY_TOKEN}"

services:
  - type: audio                            # service type
    model: "whisper-large-v3"              # OpenAI model field
    default: true                          # fallback when model omitted

    # Sync routing
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inference_url: "http://backend:80"     # base URL; original path appended

    # File validation (async mode only)
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

    # Backend authentication (sync-direct only, optional)
    inference_headers:
      Authorization: "Bearer ${RERANKER_API_KEY}"
      X-Api-Key: "${BACKEND_KEY}"

    # Multi-backend routing (optional — replaces inference_url when set)
    # weight > 0 = eligible for weighted-random primary selection
    # weight = 0 = fallback-only (tried after all weight>0 members fail)
    backend_pool: reranker-pool

    # LLM proxy (optional — activates when provider is set)
    provider: passthrough          # openai | anthropic | ollama | passthrough
    backend_model: "meta-llama/Meta-Llama-3-8B-Instruct"  # default model rewrite; overridden by a pool member's model
    response_cache_ttl: 3600       # seconds; 0 = disabled

    # Swagger spec (optional)
    swagger_url: "https://example.com/openapi.json"
    swagger_headers:
      Authorization: "Bearer ${TOKEN}"
      Accept: "application/octet-stream"
```

### Field reference

| Field | Required | Default | Description |
|---|---|---|---|
| `type` | yes | — | Service type, used in `/jobs/{service_type}` |
| `model` | no | `""` | OpenAI model field value for routing |
| `default` | no | `false` | Fallback model when request omits `model` |
| `operations` | no | `{}` | Map of operation name → URL paths |
| `inference_url` | no | `""` | Backend base URL for direct proxy (single backend, legacy — use `backend_pool` for multi-backend) |
| `backend_pool` | no | `""` | Name of a `backend_pools` entry to route through. Takes precedence over `inference_url`; mutually exclusive with `backends`. |
| `backends` | no | `[]` | **Deprecated**, use `backend_pools`/`backend_pool` instead. Legacy inline list of backends with weighted routing, kept for backward compatibility. |
| `accepted_exts` | no | any | Allowed file extensions (e.g. `.mp3`) — async mode only |
| `max_file_size_mb` | no | `100` | Max upload size in MB |
| `inference_headers` | no | `{}` | HTTP headers injected on every sync-direct / LLM proxy request |
| `provider` | no | `""` | LLM provider: `openai`, `anthropic`, `ollama`, `passthrough` |
| `backend_model` | no | `""` | Backend model name — gateway rewrites the `model` field in the request |
| `response_cache_ttl` | no | `0` | Redis response cache TTL in seconds; `0` = disabled |
| `max_concurrent_sync` | no | `0` | Max simultaneous sync requests for this model across all replicas; `0` = unlimited. Returns `503` when full. |
| `priority_reserved_sync` | no | `0` | Slots of `max_concurrent_sync` reserved for requests carrying `server.priority_header` — see [Service registry](../configure/service-registry#reserving-capacity-for-priority-requests-priority_reserved_sync) |
| `token_limits` | no | `{}` | Per-user-type token budgets for LLM requests — see [Rate limiting](../configure/rate-limiting#token-budget-limiting) |
| `swagger_url` | no | `""` | URL to fetch an OpenAPI spec from |
| `swagger_headers` | no | `{}` | HTTP headers for `swagger_url` fetch |
| `deprecated` | no | `false` | Marks the model deprecated in `GET /v1/models` and the OpenAPI spec — see [Deprecating a model](../configure/service-registry#deprecating-a-model-deprecated). Informational only. |

### `backend_pools` / `backend_pool`

Named, reusable groups of backends for blue/green, canary, or fallback routing — and, when several services reference the same pool, a shared rate-limit / concurrency budget. `backend_pool` on a service takes precedence over `inference_url`; it's mutually exclusive with the legacy inline `backends` list.

```yaml
backend_pools:
  chat-pool:
    members:
      - url: "http://backend-a:8000"
        weight: 90       # weighted-random primary selection
        model: "model-v1"   # overrides service-level backend_model for this member
        headers:            # overrides service-level inference_headers for this member
          Authorization: "Bearer ${TOKEN_A}"
      - url: "http://backend-b:8000"
        weight: 10
        model: "model-v2"
        headers:
          Authorization: "Bearer ${TOKEN_B}"
      - url: "http://backend-fallback:8000"
        weight: 0          # fallback-only: tried after all weight>0 members fail
    rate_limit: { rate: 50, period: 1s }   # optional pool-wide budget, shared by every service referencing this pool
    max_concurrent: 30                      # optional pool-wide concurrency budget

services:
  - type: llm
    model: "chat"
    provider: passthrough
    backend_pool: chat-pool
```

**Per-member fields:**

| Field | Description |
|---|---|
| `url` | Backend URL (required) |
| `weight` | Routing weight. `0` = fallback-only (never primary-selected). |
| `model` | Overrides service-level `backend_model` for this member only. |
| `headers` | HTTP headers injected on requests to this member. Highest precedence, above the pool's own `headers` and the service's `inference_headers`. |
| `rate_limit` | Optional per-member `rate`/`period` cap, checked on every backend attempt. |
| `max_concurrent` | Optional per-member concurrency cap. |

**Pool-level fields:**

| Field | Description |
|---|---|
| `members` | List of pool members (required, ≥1) — see above. |
| `headers` | Pool-level default headers, overridden by a member's own headers. |
| `rate_limit` | Pool-wide `rate`/`period` budget shared across all members and all services referencing this pool. |
| `max_concurrent` | Pool-wide concurrency budget, acquired once per client request, shared across all services referencing this pool. |
| `priority_reserved_concurrent` | Slots of `max_concurrent` reserved for requests carrying `server.priority_header` — mirrors `services[].priority_reserved_sync`. |

See [Service registry → Backend pools](../configure/service-registry#backend-pools-backend_pools) for the full write-up (semaphore/rate-limit semantics, circuit breaker interaction).

### `backends` (deprecated)

A service may still declare an inline `backends` list instead of `backend_pool` — same per-member fields as above (`url`, `weight`, `model`, `headers`), without pool-level sharing. It remains supported for backward compatibility but **will be removed in a future release**; use `backend_pools`/`backend_pool` for all new services.

On network error or 5xx, the next backend is tried. On 4xx (including 401), the retry loop stops — client errors are not retried.

### `inference_headers`

Arbitrary HTTP headers injected on every request forwarded to the inference backend. Only applies to the **sync-direct proxy** and **LLM proxy** flows. Has no effect on async jobs.

- Header values support `${VAR}` / `${VAR:-default}` env expansion.
- Config headers **override** any header with the same name sent by the client.
- `backends[].headers` takes further precedence over `inference_headers` for a specific backend.

```yaml
inference_headers:
  Authorization: "Bearer ${BACKEND_API_KEY}"
  X-Api-Key: "${BACKEND_KEY}"
```

## OpenTelemetry

```yaml
opentelemetry:
  enabled: false
  service_name: ""          # overrides the default "gatewai/gateway" resource attribute
  exporter:
    endpoint: "http://otel-collector:4318"
    insecure: false
    headers:
      Authorization: "Bearer ${OTEL_TOKEN}"
  traces:
    enabled: true
    sample_ratio: 1.0
    ignore_paths:           # path prefixes excluded from tracing (prefix-based)
      - /health             # default list — applied when ignore_paths is absent
      - /metrics
      - /docs
      - /openapi.yaml
  metrics:
    enabled: false
    interval: "60s"
  logs:
    enabled: false
```

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Master switch — `false` = no-op providers, zero overhead |
| `service_name` | `"gatewai/gateway"` | OTel resource `service.name` attribute |
| `exporter.endpoint` | — | Base OTLP/HTTP endpoint for all signals |
| `exporter.insecure` | `false` | Skip TLS verification (dev only) |
| `traces.enabled` | `true` | Enable distributed tracing |
| `traces.sample_ratio` | `1.0` | Sampling rate: `1.0` = always, `0.1` = 10% |
| `traces.ignore_paths` | `[/health, /metrics, /docs, /openapi.yaml]` | Path prefixes excluded from tracing; overrides the default list entirely when set |
| `metrics.enabled` | `false` | Enable OTLP metrics push |
| `metrics.interval` | `"60s"` | Push interval |
| `logs.enabled` | `false` | Enable OTLP log export via slog bridge |

See [OpenTelemetry](../observe/opentelemetry.md) for the full guide (per-signal endpoint overrides, sampling, Helm sub-chart, backend examples).

## Hot reload

`POST /-/reload` re-reads the config file and atomically swaps the router. S3 and Redis connections are not re-initialised.

The `configmap-reload` sidecar can trigger this automatically on ConfigMap changes.

Parameters updated at runtime without pod restart:

| Parameter | Notes |
|---|---|
| `services.*` | Full registry rebuild |
| `lifecycle.*` (all fields) | Including `gc.enabled`, `gc.interval`, `gc.orphan_min_age`, `job_ttl.*` |
| `redis.pending_max_age` | Picked up on the next GC tick |
| `rate_limits` | Applied immediately to new requests |
| `audit_log` | Toggled without restart |
| `server.user_type_header` | Applied to new requests |

Not hot-reloadable (restart required): `redis.addr`, `s3.*`, `server.addr`, `server.*_timeout`, `metrics.top_consumers`.

## Usage tracking

```yaml
usage:
  retention: ""      # Go duration string; empty = all-time (no TTL)
```

Controls how long per-consumer cumulative usage data is kept in Redis sorted sets.

| Field | Description |
|---|---|
| `retention` | Duration before usage sorted sets expire. Empty or absent = no expiry (all-time accumulation). Accepts Go duration strings: `"720h"` (30 days), `"8760h"` (365 days). Note: `"d"` suffix is not a valid Go duration — use `"h"`. |

When `server.consumer_header` is not configured, a no-op tracker is used and no data is written. `retention` is hot-reloadable and affects new sorted-set keys only (existing keys keep their original TTL).

Whenever consumer tracking is active, the gateway also writes cross-consumer calendar-aligned aggregate counters (`usage:agg:*` in Redis) — plain totals per service type/metric/day/week/month, with no per-consumer breakdown. These aren't governed by `usage.retention`: daily buckets expire after 400 days, weekly after 800 days, and monthly buckets are kept indefinitely (their cardinality stays trivial). They power `GET /-/usage/report`, described below.

See [API reference](../reference/api) for the `GET /usage`, `GET /-/usage`, and `GET /-/usage/report` endpoints that expose this data.
