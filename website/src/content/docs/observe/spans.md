---
title: Span reference
---

# Span reference

All spans emitted by the gateway and relay. Every span carries the `service.name` resource attribute (`gatewai/gateway` or `gatewai/relay`) and inherits `service.version` from the binary build version.

W3C TraceContext propagation is enabled globally. Incoming HTTP requests extract a parent span from the `traceparent` header; the relay continues the trace via the `trace_context` field in the Redis job record.

## Gateway spans

### HTTP server span — `<METHOD> <route>` (middleware)

Created for every incoming HTTP request by `OtelMiddleware`. The span starts named after the HTTP method and, once chi resolves the route, is renamed to `<METHOD> <route>` (e.g. `POST /jobs/{service_type}`) — there is no fixed `gateway.http.server` name.

| Attribute | Type | Value |
|-----------|------|-------|
| `http.method` | string | `GET`, `POST`, … |
| `http.route` | string | chi route pattern, e.g. `/jobs/{service_type}` |
| `http.status_code` | int | response status |

Span kind: **Server**. Status set to `Error` when `http.status_code >= 500`.

---

### `gateway.job.submit` — async job submission

Created inside `POST /jobs/{service_type}` after rate-limit checks pass and the file is uploaded to S3. This is the root span for the async job lifecycle — its `traceparent` is stored in `job.trace_context` so the relay can continue the trace.

| Attribute | Type | Value |
|-----------|------|-------|
| `job_id` | string | UUID of the created job |
| `service_type` | string | e.g. `audio`, `llm` |
| `model` | string | resolved model name |
| `consumer` | string | value of `server.consumer_header` |

---

### `gateway.s3.upload` — S3 upload

| Attribute | Type | Value |
|-----------|------|-------|
| `s3.operation` | string | `upload` |
| `s3.key` | string | object key, e.g. `{job_id}/input.wav` |

---

### `gateway.s3.get` — S3 download

| Attribute | Type | Value |
|-----------|------|-------|
| `s3.operation` | string | `get` |
| `s3.key` | string | object key |

---

### `gateway.s3.delete` — S3 delete

| Attribute | Type | Value |
|-----------|------|-------|
| `s3.operation` | string | `delete` |
| `s3.key` | string | object key |

---

### `gateway.redis.enqueue` — Redis queue enqueue

Created inside `SaveJob` when the job record is written and pushed onto `relay:{model}:pending`.

| Attribute | Type | Value |
|-----------|------|-------|
| `job_id` | string | UUID |
| `service_type` | string | |
| `model` | string | |
| `queue` | string | `relay:{model}:pending` |

---

### `gateway.llm.request` — LLM proxy request

Created for every LLM proxy request (`POST /v1/*` with a configured `provider`).

| Attribute | Type | Value |
|-----------|------|-------|
| `service_type` | string | |
| `model` | string | alias model name |
| `provider` | string | `openai`, `anthropic`, `ollama`, `passthrough` |
| `consumer` | string | |
| `http.status_code` | int | final response status (set after response) |
| `llm.backend_model` | string | rewritten model name sent to the backend |

Status set to `Error` on backend failure or translation error.

---

### `gateway.consumer.job_completed` — async job completion

Created by the Redis pub/sub subscriber when a job completion notification is received on `jobs:{model}:completed`.

| Attribute | Type | Value |
|-----------|------|-------|
| `job_id` | string | |
| `model` | string | |

---

### `gateway.webhook.send` — webhook delivery

Created by `WebhookSender.Send`. Restores the original trace context from `job.trace_context` so this span appears as a child of the original submit span in the same trace.

| Attribute | Type | Value |
|-----------|------|-------|
| `job_id` | string | |
| `service_type` | string | |
| `job_status` | string | `completed` or `failed` |

Status set to `Error` after all retries are exhausted.

---

## Relay spans

### `relay.process_job` — full job pipeline

Root span for relay processing. Extracts the gateway's trace context from `job.trace_context` so this span becomes a child of `gateway.job.submit` in the same distributed trace.

| Attribute | Type | Value |
|-----------|------|-------|
| `job_id` | string | |
| `service_type` | string | |
| `model` | string | |

Child spans: `relay.inference_call`.

---

### `relay.inference_call` — HTTP call to inference API

Created inside `runInference`. After building the multipart request, the relay injects the active `traceparent` as a W3C HTTP header — the inference API can create child spans if it is OTel-instrumented.

| Attribute | Type | Value |
|-----------|------|-------|
| `job_id` | string | |
| `model` | string | |
| `inference_url` | string | path appended to `inference.base_url`, e.g. `/v1/audio/transcriptions` |

Status set to `Error` on inference failure. Retried once; both attempts appear as separate spans under `relay.process_job`.

---

## Trace propagation diagram

```
Client request
  │
  │ HTTP (traceparent header optional — extracted by OtelMiddleware)
  ▼
[POST /jobs/{service_type}]        ← SpanKind: Server (named <METHOD> <route>)
  └─ [gateway.job.submit]          ← job.trace_context = traceparent stored in Redis
       ├─ [gateway.s3.upload]
       └─ [gateway.redis.enqueue]

                ┊ (async, via Redis queue)

[relay.process_job]                ← context restored from job.trace_context
  └─ [relay.inference_call]        ← traceparent injected in HTTP header
         │
         │ HTTP (traceparent header → inference API)
         ▼
       Inference API (optional child spans)

                ┊ (async, via Redis pub/sub)

[gateway.consumer.job_completed]
  └─ [gateway.webhook.send]        ← context restored from job.trace_context
```

## TraceQL queries

Common queries for the [GatewAI — Traces](../observe/opentelemetry#grafana-dashboards) dashboard or Grafana Explore.

```
# All GatewAI spans in the last hour
{service.name=~"gatewai.*"}

# Slow job submissions (> 500ms total)
{span.name="gateway.job.submit", duration > 500ms}

# Error traces only
{status=error, service.name=~"gatewai.*"}

# Full end-to-end trace for a specific job
{span.job_id="<job-uuid>"}

# Slow inference calls (> 30s)
{span.name="relay.inference_call", duration > 30s}

# Failed webhook deliveries
{span.name="gateway.webhook.send", status=error}

# LLM requests by backend model
{span.name="gateway.llm.request"} | select(span.llm.backend_model, duration, status)
```

## RED metrics via TraceQL

With Tempo 2.3+ you can derive rate, error, and duration metrics directly from traces without a separate metrics pipeline:

```
# Request rate per service
{service.name=~"gatewai.*"} | rate() by(resource.service.name)

# Error rate
{status=error, service.name=~"gatewai.*"} | rate()

# p95 duration per operation
{service.name=~"gatewai.*"} | quantile_over_time(duration, 0.95) by(span.name)
```

These queries power the **RED — Métriques par opération** row in `gatewai-traces-v1`.
