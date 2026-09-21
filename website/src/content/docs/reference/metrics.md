---
title: Metrics reference
---

# Metrics reference

All metrics use the `gatewai_` prefix (gateway) or `gatewai_relay_` prefix (relay). See [Prometheus](../observe/prometheus) for setup instructions.

## Gateway

### Requests

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_requests_total` | counter | `mode`, `service_type`, `model`, `status` | Total requests handled by the gateway |
| `gatewai_request_duration_seconds` | histogram | `mode`, `service_type`, `model` | End-to-end request duration. Buckets: 0.1s–300s |
| `gatewai_model_hidden_total` | counter | `service_type`, `model` | Requests rejected (404) because the model is gated by `services[].visibility` and the caller is outside its audience |

### Async jobs

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_async_jobs_submitted_total` | counter | `service_type`, `model` | Jobs accepted (HTTP 202) |
| `gatewai_async_jobs_cancelled_total` | counter | `service_type`, `model` | Jobs cancelled by client (`DELETE /jobs/{type}/{id}`) |
| `gatewai_async_jobs_cancelled_while_processing_total` | counter | `service_type`, `model` | Jobs cancelled while relay was processing them |
| `gatewai_async_jobs_purged_total` | counter | `model` | Jobs deleted via admin purge endpoint |
| `gatewai_async_stale_jobs_swept_total` | counter | `model` | Pending jobs marked failed by the stale-job GC |
| `gatewai_async_jobs_reaped_total` | counter | `model`, `outcome` (`requeued`\|`deadletter`\|`dropped`) | Orphaned processing jobs handled by the GC phase-0 lease reaper |
| `gatewai_jobs_total` | counter | `service_type`, `model`, `status` (`completed`\|`failed`) | Async jobs reaching a terminal outcome, incremented once on the gateway side (not the relay) so it stays reliable regardless of relay deployment shape |
| `gatewai_idempotency_requests_total` | counter | `service_type`, `outcome` (`created`\|`replayed`\|`conflict`) | Async submissions carrying an `Idempotency-Key`, by outcome |
| `gatewai_jobs_by_consumer_total` | counter | `mode`, `service_type`, `model`, `consumer` | Jobs submitted per consumer. Requires `metricsConfig.consumerLabels: true` |
| `gatewai_webhook_deliveries_total` | counter | `result` (`delivered`\|`deadletter`) | Terminal webhook outcomes |
| `gatewai_webhook_retry_queue_depth` | gauge | — | Webhooks currently scheduled for retry in Redis |
| `gatewai_relay_queue_depth` | gauge | `model`, `state` (`pending`\|`processing`) | Live length of each relay queue list, read from Redis on every scrape |
| `gatewai_relay_queue_orphans_swept_total` | counter | `model`, `state` (`pending`\|`processing`) | Relay queue entries removed by GC phase 3 because their job record no longer exists |

### Rate limiting

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_ratelimit_requests_total` | counter | `service_type`, `user_type`, `result` | Requests evaluated by rate limiter (`result`: `allowed`\|`rejected`) |
| `gatewai_ratelimit_consumer_hits_total` | counter | `service_type`, `user_type`, `consumer` | Per-consumer rate-limit evaluations (use with `group()` for distinct count) |
| `gatewai_ratelimit_errors_total` | counter | `service_type` | Redis errors during rate-limit checks (requests allowed on error) |
| `gatewai_token_ratelimit_checked_total` | counter | `service_type`, `user_type`, `result` | Token budget checks (`result`: `allowed`\|`rejected`) |
| `gatewai_token_ratelimit_errors_total` | counter | `service_type` | Redis errors during token budget checks |
| `gatewai_concurrent_job_checks_total` | counter | `service_type`, `user_type`, `result` | Concurrent job limit checks |
| `gatewai_processingtime_checks_total` | counter | `service_type`, `user_type`, `result` | Processing time budget checks |
| `gatewai_quota_resets_total` | counter | `service_type` | Per-consumer quota resets performed via the admin quota-reset endpoint |

### LLM proxy

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_llm_requests_total` | counter | `service_type`, `model`, `backend_model`, `provider`, `user_type`, `status`, `stream` | LLM requests by provider, HTTP status, and whether the request was streamed (`stream`: `true`\|`false`) |
| `gatewai_llm_tokens_total` | counter | `service_type`, `model`, `backend_model`, `user_type`, `type` | Tokens served (`type`: `prompt`\|`completion`), includes cache hits |
| `gatewai_llm_request_duration_seconds` | histogram | `service_type`, `model`, `backend_model`, `provider`, `user_type`, `stream` | End-to-end LLM request latency. Buckets: 0.05s–120s |
| `gatewai_llm_time_to_first_token_seconds` | histogram | `service_type`, `model`, `backend_model`, `provider`, `user_type` | Streaming only: delay from the gateway writing SSE headers to the first chunk received from the backend. Buckets: 0.01s–30s |
| `gatewai_llm_tokens_per_request` | histogram | `service_type`, `model`, `backend_model`, `user_type` | Total tokens (prompt+completion) per request. Buckets: 50–100 000 |
| `gatewai_llm_consumer_tokens_top` | gauge | `consumer`, `user_type`, `type` | Token usage for top-N consumers (refreshed from Redis every 60s). Requires `metricsConfig.topConsumers > 0` |
| `gatewai_llm_fallback_total` | counter | `service_type`, `model`, `fallback` | Requests re-routed to `services[].fallback_model` because the primary model's backends were all circuit-open |
| `gatewai_backend_circuit_open` | gauge | `model`, `backend` | `1` while an LLM backend's circuit breaker is open (skipped), `0` when closed |
| `gatewai_backend_circuit_opens_total` | counter | `model`, `backend` | Number of times a backend's circuit transitioned to open |
| `gatewai_backend_circuit_skipped_total` | counter | `model`, `backend` | Requests that skipped a backend because its circuit was open |

### Backend pools

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_backend_pool_rate_limited_total` | counter | `pool`, `member` | Requests rejected by a pool's rate limit (`member` empty for a pool-wide rejection, set for a per-member one) |
| `gatewai_backend_pool_concurrency_rejected_total` | counter | `pool` | Requests rejected because the pool's shared concurrency budget was exhausted |
| `gatewai_backend_pool_rate_limit_errors_total` | counter | `pool` | Redis errors while checking a pool's rate limit (fail-open — request allowed through) |

See [Service registry](../configure/service-registry.md#backend-pools-backend_pools) for `backend_pools` configuration.

### Usage tracking (top consumers)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_usage_tokens_top` | gauge | `consumer`, `service_type`, `token_type`, `user_type` | Token usage for top-N consumers on non-LLM-proxy services (refreshed from Redis every 60s). `user_type` is the consumer's last-known rate-limit tier for that service, empty if never recorded. Requires `metricsConfig.topConsumers > 0` |
| `gatewai_usage_requests_top` | gauge | `consumer`, `service_type`, `user_type` | Request count for top-N consumers per service type, sync and async alike (refreshed from Redis every 60s). Requires `metricsConfig.topConsumers > 0` |
| `gatewai_usage_processing_time_top` | gauge | `consumer`, `service_type`, `user_type` | Cumulative processing time (seconds) for top-N consumers per service type (refreshed from Redis every 60s). Requires `metricsConfig.topConsumers > 0` |

### Response cache

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_cache_hits_total` | counter | `service_type`, `model` | LLM response cache hits |
| `gatewai_cache_misses_total` | counter | `service_type`, `model` | LLM response cache misses |
| `gatewai_cache_errors_total` | counter | `service_type`, `model`, `operation` | LLM response cache errors |

### Guardrails

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_guardrails_pii_blocked_total` | counter | `service_type`, `model` | Requests blocked by PII guardrail |
| `gatewai_guardrails_evaluations_total` | counter | `service_type`, `model`, `stage` (`input`\|`output`) | Every guardrails-enabled evaluation, regardless of outcome — the denominator for a hit rate against `gatewai_guardrails_total` |
| `gatewai_guardrails_async_total` | counter | `service_type`, `model`, `detector`, `result` (`flagged`\|`blocked`\|`redacted`\|`error`) | Result-stage guardrail detections on async job results |
| `gatewai_guardrails_model_detections_total` | counter | `service_type`, `model`, `stage`, `detector`, `mode` (`sync`\|`async`), `result` (`blocked`\|`flagged`) | Detections from a model-backed guardrail detector |
| `gatewai_guardrails_model_latency_seconds` | histogram | `detector` | Latency of a model-backed guardrail detector call. Buckets: 10ms–5s |
| `gatewai_guardrails_model_errors_total` | counter | `detector`, `reason` (`timeout`\|`unreachable`\|`bad_response`) | Model-backed guardrail detector call failures |
| `gatewai_guardrails_model_cache_total` | counter | `detector`, `result` (`hit`\|`miss`) | Model-backed guardrail verdict-cache lookups |
| `gatewai_guardrails_model_skipped_total` | counter | `detector`, `reason` | Model-backed guardrail calls skipped by a guard (e.g. the input-length gate) |

### Authentication

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_auth_oauth2_duration_seconds` | histogram | `operation` (`jwt`\|`introspection`) | OAuth2 token verification latency. Buckets: 1ms–2.5s |
| `gatewai_auth_oauth2_errors_total` | counter | `operation`, `reason` (`invalid_token`\|`unreachable`) | OAuth2 token verification failures |

### Access control

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_authz_decisions_total` | counter | `service_type`, `model`, `decision` (`allow`\|`deny`) | Authorization decisions evaluated against `policies.rules` |

### Infrastructure

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_s3_errors_total` | counter | `operation` | S3 operation errors |
| `gatewai_s3_operation_duration_seconds` | histogram | `operation` | S3 operation duration. Default Prometheus buckets |
| `gatewai_redis_errors_total` | counter | `operation` | Redis operation errors |
| `gatewai_redis_operation_duration_seconds` | histogram | `operation` | Redis operation duration. Default Prometheus buckets |

---

## Relay

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gatewai_relay_jobs_total` | counter | `service_type`, `status` | Jobs processed by the relay (`status`: `success`\|`failed`) |
| `gatewai_relay_inference_duration_seconds` | histogram | `service_type`, `model` | Inference call duration. Buckets: 0.5s–600s |
| `gatewai_relay_input_size_bytes` | histogram | `service_type` | Input file size. Exponential buckets: 1 KB–256 MB |
| `gatewai_relay_s3_errors_total` | counter | `operation` | Relay S3 operation errors |
| `gatewai_relay_s3_operation_duration_seconds` | histogram | `operation` | Relay S3 operation duration. Default Prometheus buckets |
| `gatewai_relay_redis_publish_errors_total` | counter | — | Redis pub/sub publish errors on job completion |
| `gatewai_relay_redis_done_errors_total` | counter | — | Redis errors when removing job from processing list |
