---
title: API reference
---

# API reference

All endpoints are served by the gateway on `:8080` by default. By default no authentication is enforced at the gateway level — secure via your API gateway (APISIX, ingress, etc.). Optional gateway-side auth is available via `auth.mode` (`oauth2` | `proxy`) — see [Authentication](../configure/authentication).

## Async jobs

### Submit a job

```
POST /jobs/{service_type}
```

Accepts a multipart form. Returns `202 Accepted` with a job ID.

**Form fields**

| Field | Required | Description |
|-------|----------|-------------|
| `file` | yes | Input file |
| `model` | no | Target model. Auto-selected if only one is registered for the type |
| `operation` | no | Operation name (e.g. `transcription`). Required if the model has multiple operations |
| `callback_url` | no | Webhook URL called on completion (see [Webhooks](#webhooks)) |

**Headers**

| Header | Description |
|--------|-------------|
| `Idempotency-Key` | Optional. A repeat submission with the same key (scoped per consumer) returns the original job instead of starting a duplicate inference. See [Idempotency](../configure/configuration#idempotency). |

**Response**

```json
{ "job_id": "01HXYZ...", "service_type": "audio", "model": "whisper", "status": "pending" }
```

- `200 OK` + `X-Idempotent-Replay: true` — a job with this `Idempotency-Key` already exists; the original is returned (no new inference).
- `409 Conflict` — the `Idempotency-Key` was reused but its job is no longer available; retry with a new key.

---

### Get job status / result

```
GET /jobs/{service_type}/{id}
```

Returns the job record. When status is `completed`, the `result` field contains the inference output.

**Response fields**

| Field | Description |
|-------|-------------|
| `id` | Job ID |
| `status` | `pending` \| `processing` \| `completed` \| `failed` \| `cancelled` |
| `service_type` | Service type |
| `model` | Model used |
| `result` | Inference result (only when `completed`) |
| `error` | Error message (only when `failed`) |
| `created_at` | ISO 8601 timestamp |

**Status codes**

- `200 OK` — the job record (any non-terminal or terminal status above).
- `410 Gone` + `{"status":"expired"}` — the job **existed** but its retention TTL has passed and the record is gone. Distinguishes "timed out / cleaned up" from "never existed".
- `404 Not Found` — no job with this ID was ever submitted (or ownership check failed).

If `server.consumer_header` is configured and the header is present, ownership is enforced: a consumer can only access their own jobs.

---

### Cancel a job

```
DELETE /jobs/{service_type}/{id}
```

Returns `202 Accepted`. If the job is already processing, a cancellation signal is sent to the relay — the relay stops inference asynchronously. The job record is kept until TTL expires.

---

### List jobs

```
GET /jobs
```

Lists the caller's jobs across all service types. Requires `server.consumer_header` to be configured — returns `400` otherwise.

**Query parameters**

| Parameter | Description |
|-----------|-------------|
| `service_type` | Filter by service type (optional) |
| `status` | Filter by status (optional) |

---

## Webhooks

If a job was submitted with `callback_url`, the gateway `POST`s a JSON notification there when the job reaches a terminal state. Delivery is durable — one inline attempt, then Redis-backed retries with exponential backoff, then a dead-letter list. See [Configuration — Webhooks](../configure/configuration#webhooks) for `max_retries` / `retry_backoff` / `max_backoff` / `signing_secret`.

**Request headers**

| Header | Description |
|--------|-------------|
| `X-Job-ID` | The job ID |
| `X-Gatewai-Signature` | Present when `webhooks.signing_secret` is set: `t=<unix>,v1=<hex>` where `v1 = HMAC-SHA256(secret, "<t>.<raw_body>")`. Verify it and reject stale `t` for replay protection. |

**Body**

```json
{
  "job_id": "01HXYZ...",
  "service_type": "audio",
  "status": "completed",
  "result": { },
  "error": "",
  "completed_at": "2026-08-25T10:00:00Z"
}
```

A `2xx`–`4xx` response is treated as delivered; a `5xx` or a network error is retried. After `webhooks.max_retries` attempts the delivery is dead-lettered to the `webhook:deadletter` Redis list.

---

## Sync proxy

### OpenAI-compatible proxy

```
POST /v1/*
```

Proxies the request to the configured `inference_url`. Handles both JSON and multipart requests. When `provider` is set on the service, routes through the LLM proxy (OpenAI/Anthropic/Ollama/passthrough).

---

### List models

```
GET /v1/models
```

Returns an OpenAI-compatible model list for all registered services with a `model` field set. Each entry carries GatewAI capability metadata and, when the alias rewrites the model name, a `backend_model` field exposing the real model that runs behind it:

```json
{
  "object": "list",
  "data": [
    {
      "id": "next-gen",
      "object": "model",
      "owned_by": "gatewai",
      "service_type": "llm",
      "provider": "passthrough",
      "backend_model": "meta-llama/Meta-Llama-3-8B-Instruct",
      "capabilities": { "supports_sync": true, "supports_streaming": true }
    }
  ]
}
```

`backend_model` is omitted when the alias is forwarded unchanged. It only ever reflects the service-level `backend_model` — a per-backend `backends[].model` override is not surfaced here.

**Backend passthrough** — `GET /v1/models?model=<name>` proxies to that model's backend and returns its native model info (context size, etc.).

**Visibility** — models gated via `services[].visibility` are filtered by caller audience: a restricted model is omitted from the list and returns `404` on `?model=` for callers outside its audience, so a hidden model is indistinguishable from a non-existent one. See [Service registry → Model visibility](../configure/service-registry.md#model-visibility-visibility).

---

## Usage

### Get my usage

```
GET /usage
```

Returns cumulative totals and current-window values for the calling consumer across all service types.

Requires `server.consumer_header` to be configured — returns `501 Not Implemented` if absent. Returns `400 Bad Request` if the header is missing from the request.

**Response**

```json
{
  "consumer": "alice",
  "retention": "all-time",
  "last_active": "2026-06-29T10:55:00Z",
  "usage": [
    {
      "service_type": "llm",
      "total": {
        "requests": 1250,
        "tokens": { "prompt": 45000, "completion": 12000 }
      },
      "window": {
        "requests": 42,
        "tokens": 8500,
        "reset_at": "2026-06-29T11:00:00Z"
      }
    },
    {
      "service_type": "audio",
      "total": {
        "requests": 80,
        "jobs": 80,
        "processing_time_seconds": 3600.0
      },
      "window": {
        "requests": 5,
        "processing_time_seconds": 150.5,
        "reset_at": "2026-06-29T11:00:00Z"
      }
    }
  ]
}
```

**Response fields**

| Field | Description |
|---|---|
| `consumer` | Consumer identifier from the consumer header |
| `retention` | Retention period for cumulative data: `"all-time"` or a duration string (e.g. `"8760h"`) |
| `last_active` | Timestamp of the consumer's last tracked request (omitted if no activity) |
| `usage[].service_type` | Service type (matches `services[].type` in config) |
| `usage[].total.requests` | All-time (or retention-window) cumulative request count |
| `usage[].total.jobs` | Async jobs submitted (async service types only) |
| `usage[].total.processing_time_seconds` | Cumulative inference seconds (async only) |
| `usage[].total.tokens` | LLM token counts (`prompt` + `completion`) — `llm` service type only |
| `usage[].window` | Current rate-limit window values; omitted when no active limit exists |
| `usage[].window.reset_at` | When the current window expires |

Service types with zero cumulative data are omitted. `window` is omitted when no rate-limit key exists in Redis (no limit configured or window expired).

---

## Admin

### List all consumers' usage

```
GET /-/usage
```

Returns usage for all consumers, paginated. Caller is responsible for upstream authentication.

**Query parameters**

| Parameter | Default | Description |
|---|---|---|
| `consumer` | — | Return a single consumer by exact name; `total` is always `1` |
| `type` | — | Filter by service type; consumers are ranked by request count for that type, and only that type's data is returned per consumer |
| `limit` | `20` | Page size (max `100`) |
| `offset` | `0` | Pagination offset |

`consumer` and `type` can be combined: `?consumer=alice&type=llm` returns Alice's LLM data only.

**Examples**

```bash
# All consumers, page 2
GET /-/usage?limit=20&offset=20

# Top consumers for the llm service type
GET /-/usage?type=llm&limit=10

# Single consumer lookup
GET /-/usage?consumer=alice

# Single consumer, one service type
GET /-/usage?consumer=alice&type=audio
```

**Response**

```json
{
  "total": 150,
  "limit": 20,
  "offset": 0,
  "consumers": [
    {
      "consumer": "alice",
      "retention": "all-time",
      "last_active": "2026-06-29T10:55:00Z",
      "usage": [
        {
          "service_type": "llm",
          "total": {
            "requests": 1250,
            "tokens": { "prompt": 45000, "completion": 12000 }
          },
          "window": {
            "requests": 42,
            "tokens": 8500,
            "reset_at": "2026-06-29T11:00:00Z"
          }
        }
      ]
    }
  ]
}
```

`total` is the number of known consumers (or `1` when `consumer` is set). Each element of `consumers` follows the same shape as `GET /usage`.

---

### Usage report

```
GET /-/usage/report
```

Cross-consumer, calendar-aligned usage totals for one service type — for finance/BI reporting (e.g. "total tokens for `llm` in March 2026"). Unlike `GET /-/usage`, this is not a per-consumer breakdown: it sums requests, jobs, processing time, and LLM tokens across **all** consumers into UTC calendar buckets (day/week/month). Caller is responsible for upstream authentication.

Requires `server.consumer_header` to be configured (same gate as `GET /usage` / `GET /-/usage`).

**Query parameters**

| Parameter | Required | Description |
|---|---|---|
| `type` | Yes | Service type, e.g. `audio`, `llm` |
| `period` | Yes | Bucket granularity: `daily`, `weekly`, or `monthly` |
| `from` | Yes | Range start (inclusive), UTC, `YYYY-MM-DD` |
| `to` | Yes | Range end (inclusive), UTC, `YYYY-MM-DD` |
| `total` | No | When `true`, also sum every bucket into a top-level `total` field |

The `[from, to]` range is capped at 400 buckets per request (about 400 days, ~7.5 years of weekly buckets, or ~33 years of monthly buckets) — returns `400 Bad Request` beyond that.

**Example**

```bash
GET /-/usage/report?type=llm&period=monthly&from=2026-01-01&to=2026-03-31&total=true
```

**Response**

```json
{
  "service_type": "llm",
  "period": "monthly",
  "from": "2026-01-01",
  "to": "2026-03-31",
  "buckets": [
    { "bucket": "202601", "requests": 12000, "tokens": { "prompt": 450000, "completion": 120000 } },
    { "bucket": "202602", "requests": 15000, "tokens": { "prompt": 510000, "completion": 138000 } },
    { "bucket": "202603", "requests": 9000,  "tokens": { "prompt": 300000, "completion": 81000 } }
  ],
  "total": {
    "requests": 36000,
    "tokens": { "prompt": 1260000, "completion": 339000 }
  }
}
```

**Response fields**

| Field | Description |
|---|---|
| `buckets[].bucket` | Bucket ID: `YYYYMMDD` (daily), ISO `YYYY-Www` (weekly), or `YYYYMM` (monthly) |
| `buckets[].requests` | Total requests across all consumers in this bucket |
| `buckets[].jobs` | Async jobs submitted (async service types only) |
| `buckets[].processing_time_seconds` | Cumulative inference seconds (async only) |
| `buckets[].tokens` | LLM token counts (`prompt` + `completion`) — omitted when no tokens were tracked in that bucket |
| `total` | Sum of every bucket in `buckets`. Only present when `total=true` is passed |

Buckets with no activity are still returned, zero-filled — the response always has one entry per bucket in the requested range.

---

### Hot reload config

```
POST /-/reload
```

Reloads `config.yaml` from disk without restarting the process. Returns `200` on success, `500` if the new config is invalid (the old config remains active).

---

### Purge jobs

```
POST /-/jobs/purge
```

Deletes all job records and their S3 objects for a given model. Intended for maintenance/cleanup.

**Request body (JSON)**

```json
{ "model": "whisper-large-v3" }
```

---

## Docs

| Endpoint | Description |
|----------|-------------|
| `GET /docs` | Swagger UI for the gateway API |
| `GET /openapi.yaml` | Raw OpenAPI 3.0.3 spec (generated at startup from the live registry) |
| `GET /docs/spec/{type}/{model}` | Swagger UI for the inference backend spec (requires `swagger_url` in service config) |

---

## System

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Returns `200 OK`. Used by Kubernetes readiness/liveness probes |
| `GET /metrics` | Prometheus metrics in text exposition format |
