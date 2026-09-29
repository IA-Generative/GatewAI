# Changelog

All notable changes to this project are documented here.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).
Versioning: each component is versioned independently — see tag conventions below.

## Tag conventions

| Component | Tag format | Example |
|---|---|---|
| Gateway (binary + Docker) | `gateway/vX.Y.Z` | `gateway/v0.2.5` |
| Relay (binary + Docker) | `relay/vX.Y.Z` | `relay/v0.2.5` |
| Helm chart | auto-tagged by chart-releaser | `gatewai-gateway-0.2.0` |

---

## Gateway

### [Unreleased]

#### Added

- **Realtime WebSocket streaming services** (`services[].realtime`): the gateway now proxies bidirectional WebSocket connections (e.g. real-time speech-to-text) through its own policy layer instead of relying on an opaque APISIX passthrough. A realtime service declares a `realtime` block (WS `path`, optional `backend_path`, audio format, and caps); the gateway enforces model **visibility** and **authz** at the handshake (fail-closed — an unauthorized caller never opens a socket), applies a per-consumer **concurrent-session cap** (429 before upgrade), then relays frames both ways. Streamed audio is metered as **audio-seconds** (`bytes ÷ (sample_rate·bytes_per_sample·channels)`) into per-consumer usage (`TrackProcessingTime`) and Prometheus (`gatewai_realtime_sessions_total`, `gatewai_realtime_active_sessions`, `gatewai_realtime_audio_seconds_total`, `gatewai_realtime_session_duration_seconds`); `max_session_seconds`/`max_audio_seconds` bound a session. When the service configures output-stage checks, the backend transcript is **shadow-scanned** on close (flag-only — a match logs + increments the guardrail flag metric; block/redact are intentionally out of scope for a live stream). Optional GET discovery endpoints (`realtime.info_paths`, e.g. `/v1/config`, `/v1/languages`) are proxied behind the same gates. Adds the `github.com/coder/websocket` dependency.

### [v0.23.1] — 2026-09-28

#### Fixed

- **Streaming requests are now counted in per-consumer token usage.** Previously a streamed `/v1/*` response recorded the *request* but **zero tokens** for its consumer — `serveStream` fed only the Prometheus counter and the token rate-limiter, never the per-consumer stores (`usage:consumer:{type}:tokens:*` via `usageTracker.TrackTokens`, and the top-consumer metrics via `tracker.Track`). Non-streaming and cache-hit responses were counted, so any consumer that streams (e.g. OpenWebUI chat) was silently under-reported in the usage API and per-user dashboards, while the aggregate `gatewai_llm_tokens_total` metric (which does see streaming) has no consumer label to reconcile against. `serveStream` now records the streamed usage into both per-consumer trackers, mirroring the non-streaming path. Additionally, `stream_options.include_usage=true` is now injected whenever per-consumer usage tracking is enabled (previously only when a token limiter was configured), so the backend emits the usage chunk required to count streamed tokens even on deployments without `token_limits`.

#### Added

- **Guardrail trace tagging**: sync/inline guardrail actions (input regex, sync model detectors, output stage) now tag the request trace span with `guardrail.flagged=true` + `guardrail.stage`/`guardrail.action`/`guardrail.detectors`, so flagged traces can be filtered in Tempo/Langfuse next to the prompt. No-op when tracing is off; the async (shadow) detector is covered by flagged-sample records instead (its span has ended by then).

- **Guardrail flagged-sample records** (`guardrails.flagged_samples`, opt-in per service): when an async (shadow) model detector flags, the gateway emits one correlated, PII-**redacted** record — `{event=guardrail.flagged_sample, trace_id, detector, categories, score, service_type, model, consumer, redacted_prompt}` — so shadow-mode detections (e.g. Prompt Guard 2 injection in `mode: async`) can be *reviewed*, not just counted, before flipping to enforce. Covers both async input sites (`/v1/*` LLM proxy and `POST /jobs` submit text). The prompt is redacted before it enters the record via `flagged_samples.redact` groups (default `[pii, secrets]`); `trace_id` is taken from the detached context so the sample still correlates to its (already-ended) trace. Structured slog → when bridged to OTLP logs it lands in the operator's governed sink (Loki/Langfuse) whose retention enforces the sample TTL; no raw prompt is stored.

- **Backend pools (`backend_pools`)**: a named, load-balancer-style group of member backends that several `services[]` entries can share via `backend_pool: <name>`, instead of each declaring its own `backends:`/`inference_url:`. Centralizes auth headers (precedence: service `inference_headers` → pool `headers` → member `headers`) and lets every service routed to the same pool share one rate-limit and concurrency budget — configurable at both pool level (`rate_limit`, `max_concurrent`, `priority_reserved_concurrent`) and member level (`rate_limit`, `max_concurrent`). `backend_pool` is mutually exclusive with `backends:`/`inference_url:` on the same service, validated at config load. Concurrency is acquired once per logical client request (shared with per-model semaphores' architecture, but a distinct Redis key namespace); rate limits are checked per backend attempt inside the existing retry loop, so a pool-rate-limited member is skipped in favor of the next one before falling back to an error. New metrics `gatewai_backend_pool_rate_limited_total{pool,member}`, `gatewai_backend_pool_concurrency_rejected_total{pool}`, `gatewai_backend_pool_rate_limit_errors_total{pool}`.

#### Changed

- **Top-consumer usage gauges now carry `user_type`**: `gatewai_usage_tokens_top`, `gatewai_usage_requests_top`, and `gatewai_usage_processing_time_top` gained a `user_type` label, joined from each consumer's last-recorded rate-limit tier for that service type (empty if never recorded), matching what `gatewai_llm_consumer_tokens_top` already exposed for the LLM proxy path — lets top-consumer dashboards break down by tier without a separate join.
- **`GET /v1/models`'s `backend_model` now sources only the service-level `backend_model`**: a per-backend `backends[].model` override (canary/mixed-fleet) is no longer surfaced in the models listing — it still rewrites the outgoing request per backend, but is not exposed as `backend_model`. The `backend_models[]` array (introduced alongside `backend_model` in `v0.21.0`) is removed accordingly.

### [v0.22.0] — 2026-09-07

#### Added

- **Model-backed guardrail detectors** alongside the regex checks: a pluggable `Detector` interface with a regex adapter and a `ModelDetector` that calls a self-hosted classifier/NER endpoint (`services[].guardrails.models[]`). Detectors run in parallel with per-detector metrics, per-mode behaviour (`async`/shadow = observe-only, `sync`/inline = can act), a configurable `threshold`, `timeout`, `on_error` (fail-open/closed), a `max_input_tokens` gate to protect the latency budget, and an optional verdict `cache_ttl`. Wired into the input LLM path (sync + async). Enables a prompt-injection classifier as a guardrail.
- **NER redaction detector** (input stage): model-returned spans are redacted after classification runs on the original text (avoids placeholder false-positives).
- **Output-stage guardrails**: an optional `services[].guardrails.output` block scans/redacts the model response (`choices[*].message.content`). **Streaming enforcement** (`output.streaming`): `flag` (observe), `block` (buffer a window, terminate on a violation), or `buffer` (buffer a window, redact matches, release), with a configurable `stream_window_tokens`.
- **Tool-call argument scanning**: both input and output stages scan/redact `tool_calls[*].function.arguments`, not just message content.
- **Async result-stage guardrails** (`services[].guardrails.async`): scans an async job's *result* (transcript, OCR text, …) in the once-per-job completion handler — `flag` (shadow), `block` (fail the job, suppress its result), or `redact` (redacted sibling object + repoint). For block/redact the client-facing result is gated on the scan (poll/webhook withheld until it finishes), bounded by `scan_timeout` with an `on_timeout` policy (`fail_open`/`fail_closed`). The input stage additionally scans async-submit text params (e.g. a `prompt`) before enqueue.
- **`/v1/completions` guardrails**: the input stage now scans completion prompts, not only chat messages.
- **Per-backend circuit breaker** for the LLM proxy (`circuit_breaker`, opt-in): consecutive failures open a backend's circuit and it is skipped for a cooldown, with a half-open probe; when all of a model's backends are open the request fast-fails `503` instead of a slow `502`. Optional **active health probing** (`circuit_breaker.probe_interval`) opens/closes circuits without waiting for live traffic. A model with all backends open is marked `capabilities.degraded: true` in `GET /v1/models`. Metrics `gatewai_backend_circuit_open{model,backend}`, `_opens_total`, `_skipped_total`.
- **Cross-model fallback** (`services[].fallback_model`, opt-in): a sync request to a model whose backends are all circuit-open is transparently re-routed to a named fallback model (visibility/policies re-checked). Metric `gatewai_llm_fallback_total`.
- **Observability**: new guardrails hit-rate, OAuth2, and streaming-LLM metrics, plus Grafana dashboard panels visualizing them.

#### Changed

- Async job completion work (webhook delivery, rate/token debit, usage tracking, result-stage scan) is now driven by the reliable Redis pub/sub broadcast and deduped by an exactly-once claim, so a lost best-effort completion callback no longer drops a webhook. The relay HTTP callback remains a redundant fast-path onto the same claim.

#### Fixed

- **Streaming redaction**: content-less control chunks (role/usage and the `reasoning_content` deltas some backends interleave between every content token) no longer flush the buffered window, so a match split across content deltas is reassembled and redacted instead of leaking. The incremental release cut also snaps to UTF-8 rune boundaries so multi-byte characters are never split.
- **Async result gate** durability: the scan-timeout deadline is derived from the completion time, gate-arming is fatal for enforcing jobs, and a `fail_closed` timeout durably fails the job.
- Rune-safe truncation of OpenTelemetry span-attribute bodies and generated OpenAPI summaries (no split multi-byte characters).
- Completion `WaitGroup` is guarded against a concurrent add at shutdown now that completion work is driven from two triggers.

### [v0.21.0] — 2026-08-27

#### Added

- **Backend model exposed in `GET /v1/models`**: each model object now carries a `backend_model` field revealing the real model an alias forwards to (from `backend_model` or a per-backend `model` override), so developers can see what runs behind each alias. Omitted when the alias is passed through unchanged. When the backends behind one alias serve distinct real models (canary/mixed fleet), an additional `backend_models` array lists every distinct backend model (the first matches `backend_model`). Default-on — no configuration needed.
- **Per-audience model visibility (`services[].visibility`)**: a service entry may be gated to specific `user_types` and/or `groups`. A restricted model is filtered out of `GET /v1/models` and returns `404` on `/v1/*`, `/jobs/{service_type}`, and `GET /v1/models?model=` for callers outside its audience — a hidden model is indistinguishable from a non-existent one (fail-closed; anonymous callers see only public models). Enables beta-testing a new model through the same API before general release. Composes with `policies` access control (a request must satisfy both). New metric `gatewai_model_hidden_total{service_type, model}`; Helm `services[].visibility.userTypes`/`groups`.
- **Processing-queue crash recovery (lease reaper)**: a new GC phase (`ReapOrphanedProcessingJobs`) requeues async jobs abandoned in `relay:{model}:processing` by a relay pod that died mid-job (OOM, node loss, SIGKILL) without releasing its lease. Jobs with no live lease are requeued to `relay:{model}:pending` up to `lifecycle.gc.max_reap_attempts` times (default 3), then **dead-lettered** to `relay:{model}:deadletter` and marked failed; entries whose job record is already gone/terminal are dropped. The lease check is atomic with the reclaim (Lua), so a healthy worker's job is never requeued, and it is idempotent across gateway replicas. New metric `gatewai_async_jobs_reaped_total{model, outcome}` (`requeued`/`deadletter`/`dropped`). Runs in the existing GC loop; requires `lifecycle.gc.enabled`. Helm: `lifecycle.gc.maxReapAttempts`.
- **HMAC-signed webhooks**: when `webhooks.signing_secret` is set, every outbound webhook carries an `X-Gatewai-Signature: t=<unix>,v1=<hex>` header, where `v1 = HMAC-SHA256(secret, "<t>.<body>")`. Consumers verify the HMAC and reject stale timestamps for replay protection. Empty secret = unsigned (unchanged behaviour). Helm: `webhooks.signingSecret`.
- **Distinct `expired` status on job poll**: `GET /jobs/{service_type}/{id}` now returns **`410 Gone`** with `{"status":"expired"}` when a job whose retention TTL has passed is polled, instead of a generic `404`. A lightweight tombstone (`jobmeta:{id}`, kept `jobs.expired_marker_ttl`, default 7d) lets the gateway tell "existed but timed out / cleaned up" apart from "never existed" (still `404`). Config `jobs.expired_marker_ttl`; Helm `jobs.expiredMarkerTtl`.

#### Changed

- **Durable webhook delivery**: outbound job-completion webhooks now persist their retry state in Redis instead of an in-memory retry goroutine. `Send` makes one inline attempt; on failure the retry is stored in a ZSET (`webhook:retries`) plus a per-job task key and worked by a background loop with exponential backoff — so a gateway restart no longer silently drops pending retries. After `webhooks.max_retries` attempts (default 3) a webhook is **dead-lettered** to the `webhook:deadletter` Redis list. New metrics `gatewai_webhook_deliveries_total{result}` (`delivered`/`deadletter`) and `gatewai_webhook_retry_queue_depth`. New config block `webhooks` (`max_retries`, `retry_backoff`, `max_backoff`); Helm `webhooks.*`.
- **Idempotency keys on async submission**: `POST /jobs/{service_type}` now honours an optional `Idempotency-Key` header. A repeat submission with the same key (scoped per consumer, retained `jobs.idempotency_ttl`, default 24h) returns the original job (`200` + `X-Idempotent-Replay: true`) instead of enqueuing a duplicate inference — preventing wasted GPU calls on client retries. A reused key whose job is no longer retrievable returns `409`. Metric `gatewai_idempotency_requests_total{service_type, outcome}` (`created`/`replayed`/`conflict`); Helm `jobs.idempotencyTtl`.
- **Redis persistence enabled by default (Helm)**: the bundled `redis-ha` subchart now runs with **AOF** (`appendonly yes`, `appendfsync everysec`) + **RDB** snapshots on a PersistentVolume. Async state (job records, relay `pending`/`processing` queues, per-job leases, webhook retry queue) now survives a Redis pod restart or master failover instead of being lost — bounding data loss to ~1s. Values: `redis-ha.redis.config.appendonly`/`appendfsync`, `redis-ha.persistentVolume.enabled`.

### [v0.20.2] — 2026-07-31

#### Added

- **Top-N usage gauges extended**: new `gatewai_usage_requests_top` (request count per top consumer, sync and async alike) and `gatewai_usage_processing_time_top` (cumulative processing time per top consumer) gauges, refreshed from Redis alongside the existing token top-N gauges. Requires `metricsConfig.topConsumers > 0`.

#### Fixed

- **Broken TraceQL queries in the bundled Grafana dashboards** (`gatewai.json`, `gatewai-traces.json`, `gatewai-llm.json`): the intrinsic span name accessor is `name`, not `span.name`; service name must use `resource.service.name`, not bare `service.name`; AND-combination inside `{}` requires `&&`, not a comma. Also added a `model` breakdown to the "Taux de requêtes" panel in `gatewai.json`.

### [v0.20.1] — 2026-07-28

#### Added

- **Configurable sync request body limit**: new `server.max_body_mb` caps the request body on the sync JSON path (`POST /v1/*`) — the route vision requests use for base64-embedded images. `0` or absent = 1 MiB default. Raise it to accept images (base64 inflates binary ~33%, so a 30 MB image needs a cap around 45). Independent of `max_file_size_mb`, which bounds the multipart upload path. Exposed in the Helm chart as `server.maxBodyMB`.

#### Fixed

- **Oversized sync JSON bodies now return a clean `413`** instead of being silently truncated. The path previously used `io.LimitReader`, so a body over the (hardcoded 1 MiB) cap was cut mid-stream — surfacing as a confusing `400 "unexpected end of JSON input"` or corrupted base64 reaching the backend. It now uses `http.MaxBytesReader` and rejects oversized requests with `413 Request Entity Too Large`.

### [v0.19.0] — 2026-07-09

#### Added

- **Generic per-service usage tracking**: request, activity, and token counters are now recorded per consumer for *every* service type, not just LLM. Exposed via `GET /usage` (self-service, current consumer) and `GET /-/usage` (admin, all consumers, filterable), both returning cumulative totals plus the current rate-limit window. Retention is configurable via the new `usage.retention` config field (default: no TTL).
- **Token budget enforcement extended to async jobs**: `token_limits` now applies pre-flight on `POST /jobs/{service_type}` submission (in addition to the existing sync LLM path) and is recorded on async job completion, using the prompt/completion token counts the relay now extracts from the inference result (see Relay `v0.10.0`).
- **`GET /v1/models?model=<name>`**: proxies to the underlying backend to return its native model info (context size, capabilities) instead of only the gateway's OpenAI-compatible model list.

### [v0.20.0] — 2026-07-21

#### Added

- **New admin endpoint `GET /-/usage/report?type=X&period=daily|weekly|monthly&from=YYYY-MM-DD&to=YYYY-MM-DD`**: cross-consumer, calendar-aligned usage totals (requests, jobs, processing time, LLM tokens) for one service type — for finance/BI reporting (e.g. "total tokens for `llm` in March 2026"), not a per-consumer breakdown. Buckets are UTC-aligned; the `[from, to]` range is capped at 400 buckets per request. An optional `total=true` param sums every bucket into a top-level `total` field. Restricted to the `/-/` admin namespace — protect with upstream auth. Requires `server.consumer_header` (same gate as the existing usage-tracking endpoints). LLM proxy requests now also feed the underlying per-consumer token counters, closing a gap where LLM token usage wasn't reflected in `GET /usage` / `GET /-/usage`.
- **New admin endpoint `POST /-/quota/reset?consumer=X&type=Y`**: clears a consumer's rate-limit and token-budget Redis keys for one service type (service-level `rl:`/`trl:`/`ptrl:` across all user types, plus the exact policy-level `rlp:`/`trlp:` keys), so the next request starts a fresh window. Restricted to the `/-/` admin namespace — protect with upstream auth. Only registered when rate limiting (`rate_limits`) is configured. New metric: `gatewai_quota_resets_total{service_type}`.

#### Changed

- **New endpoint `POST /-/relay/jobs/{id}/complete`**: rate-limit debit, usage tracking, and webhook delivery for async jobs are back on the gateway, now triggered by a single targeted HTTP call from the relay (instead of the Redis pub/sub broadcast every replica receives). This avoids the N× duplication a broadcast-triggered side effect would cause on multi-replica deployments, without needing to duplicate the accounting/webhook logic in the relay itself. No authentication on the new endpoint — cluster-internal call only, same trust model as `/health`. The gateway's pub/sub completion handler keeps only the two responsibilities safe to run on every replica: the `gatewai_jobs_total` counter and the sync-wait notification.
- **Breaking / deployment note:** deploy the new gateway image before the new relay image. The endpoint is harmless/unused until the relay starts calling it; deploying relay-first would mean 404s and no side effects for every job until the gateway catches up.
- **New metric `gatewai_relay_queue_depth{model,state}`**: live length of each async model's `relay:{model}:pending`/`relay:{model}:processing` Redis lists, read directly by the gateway on every `/metrics` scrape. Lets the "Queue relay — Profondeur par modèle" Grafana panel work without deploying `redis_exporter`.

#### Fixed

- **Swagger overlay**: `GET /swagger/{type}/{model}` now removes paths not declared in the service's `operations` map instead of leaving them in unmodified — backends that expose extra undocumented paths (e.g. `/v1/models`) no longer leak them into the per-service spec.
- **OpenAPI security scheme**: the generated spec now declares `BearerAuth` (`Authorization: Bearer <token>`) instead of the incorrect legacy `apikey` header scheme, matching actual gateway auth behavior.
- **Helm chart**: numeric fields in `rateLimits` (`rate`, `tokenRate`, `maxConcurrent`, `processingTime`) and per-service `tokenLimits` (`tokenRate`) now render as plain integers via `int64`. Previously values ≥ 1,000,000 were emitted in scientific notation (e.g. `5e+06`), a fragile representation for a downstream `int` field.

#### Documentation

- Documented the OpenAI-compatible `usage` object requirement for token tracking/budgets (`llm-proxy.md`, `rate-limiting.md`, `configuration.md`), the `GET /usage`/`GET /-/usage` endpoints and `usage.retention` config, and `ignore_paths` for OpenTelemetry.
- **Helm chart README**: documented `rateLimits` token windows (`tokenRate`/`tokenPeriod`) and per-model `tokenLimits`, including combining a per-minute cap with a per-day budget across the two independent windows; documented the new `usage.*` value block.

### [v0.18.0] — 2026-06-24

#### Added

- **Authentication (OAuth2)**: optional gateway-side auth via a new top-level `auth` block. `auth.mode: oauth2` validates OAuth2 access tokens (JWT via JWKS discovery; `iss`/`aud`/`exp` + configurable claim mapping for scope/groups/roles/consumer), **fails closed** (`401`/`503`), and strips the client bearer before proxying. `auth.mode: proxy` trusts identity headers set by an upstream reverse proxy (incl. groups/roles). An absent `auth` block preserves the previous header-trust behavior. `/health`, `/metrics`, `/docs`, `/openapi.yaml` are exempt; auth config changes require a restart. Foundation for the access control and quota features below.
- **OAuth2 token introspection** (RFC 7662): `auth.oauth2.validation` (`auto` | `jwt` | `introspection`) adds validation of **opaque** access tokens via the IdP's introspection endpoint (client-credentials auth; results cached up to `cache_ttl`, capped by token `exp`). `auto` verifies JWTs locally and introspects opaque tokens. Fails closed, and provides **live revocation** for any token type.
- **Access control (policies)**: optional default-deny model/service allowlist via a top-level `policies` block (requires `auth.mode`). Rules match caller identity (`groups`/`roles`/`scopes`/`consumers`/`user_types`) and grant `allow_models`/`allow_service_types` (globs); a request with no granting rule gets `403`. Enforced on both sync (`POST /v1/*`) and async (`POST /jobs`) after routing resolves the model; hot-reloadable; absent `policies` = no enforcement. Metric: `gatewai_authz_decisions_total{service_type, model, decision}`.
- **Per-group quotas**: a policy rule may carry an optional `limits` block (`rate`/`period`, `token_rate`/`token_period`) — a per-member request-rate and token budget tiered by the caller's group/role, enforced on the sync LLM path (keyed per consumer; coexists with `rate_limits`/`token_limits`). Per-group concurrent/processing-time limits and async enforcement are planned follow-ups.

#### Fixed

- **OTel trace propagation** on the sync-direct path (`POST /v1/*`): W3C `traceparent` header is now correctly injected into upstream requests.
- **OTLP exporter path filtering**: paths in `opentelemetry.ignore_paths` are now correctly excluded from trace export.
- **Health check** status reporting: fixed incorrect `partial` / `unknown` statuses when probes are absent or run in batch mode; `GET /health` now returns the correct HTTP method default.

### [v0.17.0] — 2026-06-19

#### Added

- **OpenTelemetry observability**: built-in distributed tracing, OTLP metrics push, and structured log forwarding via OTLP/HTTP. Configured under the `opentelemetry:` block; all signals disabled by default, zero overhead when off. Spans cover the full async lifecycle: `gateway.http.server`, `gateway.job.submit`, `gateway.s3.*`, `gateway.redis.enqueue`, `gateway.llm.request`, `gateway.consumer.job_completed`, `gateway.webhook.send`. Trace context is propagated to the relay via the `trace_context` field of the Redis job record (W3C traceparent). See [Span reference](https://qatr-io.github.io/GatewAI/observe/spans).
- **Configurable guardrails pipeline**: each service's `guardrails` block now supports an `action` (`block` → reject `422`, `redact` → mask matches in-place and forward the cleaned body, `flag` → log + metric only) and a `checks` list selecting which detector groups to run. New Prometheus counter `gatewai_guardrails_total{service_type, model, action, result}` (the legacy `gatewai_guardrails_pii_blocked_total` is retained).
- **Multi-country PII detection**: check groups `pii` (universal: email, credit card, IBAN, IPv4, E.164 phone), `pii_fr` (phone FR, NIR, SIREN/SIRET), `pii_us` (SSN), `pii_uk` (NINO), `pii_es` (DNI), `pii_it` (Codice Fiscale).
- **Secrets detection** (`secrets` group): AWS access keys, private-key blocks, JWTs, GitHub/Slack/Google tokens — prevents credentials leaking to external LLM providers.
- **Output (response) DLP**: optional `guardrails.output` block (`checks` + `action`) scans the model *response* before returning it to the client. `redact` masks matches in `choices[*].message.content` (and caches the redacted body), `block` returns `422 {"error":"response blocked by guardrails"}`, `flag` logs only. Non-streaming responses are fully enforced; streaming responses always degrade to `flag` (cannot redact/block mid-stream).
- **Helm: OTel Collector options**: `otlp.enabled` deploys the `opentelemetry-collector` sub-chart (aliased `otlp`); `otlpOperator.enabled` creates an `OpenTelemetryCollector` CR for clusters with the OTel Operator installed (validates CRD presence via `Capabilities.APIVersions`).
- **Grafana dashboards**: new `gatewai-traces.json` dashboard (Tempo datasource — service map, RED metrics via TraceQL, per-operation trace search); exemplar links added to latency panels of `gatewai.json` and `gatewai-llm.json`.

#### Changed

- **Guardrails block status** is now `422 Unprocessable Entity` (was `400`).
- **`gatewai_guardrails_total` gained a `stage` label** (`input` | `output`) — label set is now `{service_type, model, stage, action, result}`. Update any dashboards/alerts that query this counter.
- **Dockerfile**: base image updated to `golang:1.25-alpine` (OTel SDK v1.44 requires Go ≥ 1.25).

#### Breaking Changes

- **`guardrails.pii` boolean removed**: the legacy `guardrails.pii: true` shorthand has been removed. Use `guardrails.checks` (+ optional `guardrails.action`) instead — e.g. `checks: [pii, secrets]` with `action: block`.

#### Fixed

- **LLM proxy nil-pointer panic**: an LLM-proxy request (`POST /v1/*`) would crash the request when rate limiting was disabled — a nil `*ratelimit.Limiter` passed as a `TokenChecker` interface became a non-nil typed-nil, defeating the handler's nil guard. The limiter is now converted to a true nil interface when unset.

---

### [v0.16.1] — 2026-06-16

#### Added

- **Concurrent job limit** (`max_concurrent`): per-consumer, per-service-type, per-user-type cap on the number of async jobs simultaneously in `pending` or `processing` state. Enforced at submission time; returns `429` on breach. Redis key: `cjl:{consumer}:{serviceType}:{userType}`.
- **Processing time budget** (`processing_time` / `processing_period`): per-consumer, per-service-type, per-user-type cap on cumulative inference seconds per time window. Applies to both async and sync calls. The relay propagates `processing_time` from `result.json` through `PublishResult`; the gateway records it on completion. Returns `429` on breach.

#### Fixed

- **TOCTOU race in concurrent job limit**: count-on-the-fly via `LPOS` replaces the previous optimistic counter, preventing over-admission under concurrent submissions.
- **Decrement never fired after hot reload or wildcard user type**: `ConcurrentChecker` now correctly decrements the gauge in all code paths.

---

### [v0.16.0] — 2026-06-12

#### Added

- **Per-model token rate limiting**: `token_limits` field on each service entry in `config.yaml` allows setting independent token budgets per model name (e.g. `chat-smart`, `gpt-4o`). Limits are applied in addition to service-level token limits; either can reject a request. Supports per-user-type differentiation (`premium`, `*` fallback) using the same structure as `rate_limits`. Redis key format: `trl:{consumer}:model:{model}:{userType}`.
- **S3 path-style and SSL insecure options**: `s3.use_path_style` (default `true`) and `s3.ssl_insecure` config fields for S3-compatible endpoints that require path-style URLs or self-signed certificates.

#### Fixed

- **Streaming token counting**: streaming requests (`stream: true`) were checked against the token budget but never contributed to it — the SSE pipe loop had no usage extraction. The stream is now scanned line-by-line; the last `data:` payload is parsed for `usage` after the stream ends, and `AddTokens`/`AddModelTokens` are called accordingly.
- **vLLM streaming usage**: `stream_options.include_usage=true` is now injected into the upstream body for streaming requests when a token limiter is configured, ensuring the backend emits a usage chunk at the end of the stream.
- **S3 checksum headers**: disabled automatic AWS checksum headers (`x-amz-checksum-*`) for S3-compatible endpoints that reject them.

---

### [v0.15.1] — 2026-06-09

#### Added

- **S3 custom CA bundle**: `s3.ca_bundle` config field (env `S3_CA_BUNDLE`) — path to a PEM file with CA certificates for S3 TLS verification. Required when the S3 endpoint uses a private or self-signed CA. Defaults to the system certificate pool when unset.
- **Helm**: `s3.existingCABundle` / `s3.existingCABundleKey` values — mounts a ConfigMap as a volume, sets `S3_CA_BUNDLE` env var automatically in the gateway Deployment.

---

### [v0.15.0] — 2026-06-05

#### Changed

- **Go module renamed**: `kevent/gateway` → `gatewai/gateway`
- **Docker image moved**: `ghcr.io/qatr-io/gatewai/gateway:vX.Y.Z` (first image published under the GatewAI name)
- **Helm chart renamed**: `kevent-gateway` → `gatewai-gateway`
- **CI runners**: all workflows migrated from `actions-runner-controller` to `ubuntu-latest`

---

### [v0.14.1] — 2026-06-05

#### Added

- **Priority queue** (`server.priority_header`): jobs are inserted at the head of the Redis queue (`LPUSH relay:<model>:pending`) when the configured header is present. No dedicated relay Deployment required — the relay's `BLMOVE LEFT RIGHT` naturally dequeues priority jobs first.
- `GatewAI_async_jobs_submitted_total` counter (labels: `service_type`, `model`, `mode=async|async-priority`) — emitted at submission time.

#### Changed

- **Grafana dashboard**: removed Kafka/Knative/relay panels; added "Jobs async — Vue d'ensemble" row and "Relay — K8s" row (kube-state-metrics + redis_exporter). Bargauge/stat panels use raw counter values instead of `rate()`/`increase()`.

#### Removed

- `GatewAI_kafka_publish_duration_seconds` and `GatewAI_kafka_publish_errors_total` Prometheus metrics (Kafka was removed in v0.14.0; these counters were never emitted).

---

### [v0.14.0] — 2026-06-04

#### Changed

- **Kafka removed entirely** (`internal/kafka/` deleted, `kafka-go` dropped): the gateway no longer publishes `InputEvent` to Kafka. Jobs are now pushed directly to the relay Redis queue (`relay:<model>:pending` via `RPUSH` inside `SaveJob`).
- **Redis-based completion notifications**: a new `internal/consumer` package replaces the Kafka `ConsumerManager`. A `Subscriber` listens on `jobs:<model>:completed` Redis pub/sub channels; a `WebhookSender` delivers results to `callback_url` with 3-attempt exponential backoff (2 s → 4 s → 8 s).
- **Sync client disconnect propagation**: cancelling the HTTP client during a sync request now cancels the in-flight inference call. Cancelled jobs are kept in Redis (status `cancelled`) for GC instead of being deleted immediately.
- **Distributed sync semaphore** via Redis — replaces the previous in-process atomic counter.

#### Added

- `job_ttl.cancelled` config field — dedicated TTL for cancelled jobs (default falls back to `global`).

#### Removed

- All Kafka config fields (`kafka.*`, `sync_topic`, `priority_topic`, `input_topic`, `result_topic`) and the `kafka-go` dependency.
- Kafka Secret, env vars, and TLS volume from the Helm chart (`helm/gateway/templates/`).
- `async_workers`, `cold_start_time`, `async_inference_url` config fields (async dispatch is now handled by the relay).
- Prometheus counters tied to Kafka publish operations.

---

### [v0.13.0] — 2026-06-01

#### Removed

**Sync-over-Kafka path** (`internal/handler/sync.go`, `internal/config/`, `internal/metrics/`)
- `sync_topic` config field removed — all `POST /v1/*` requests now always use direct proxy to `inference_url`, regardless of content type
- `syncPriority` mechanism removed — the relay no longer acts as a Knative sidecar with priority deferral; it is now a standalone Kafka pull consumer (see relay v0.6.0)
- Associated Prometheus counters for sync-over-Kafka removed

Sync routing is now always:

| Request | Path |
|---|---|
| Any `POST /v1/*` | Direct proxy to `inference_url` |

---

### [v0.12.1] - 2026-05-29

### [v0.11.0] — 2026-05-27

#### Added

**Unified GC** (`cmd/gateway/gc.go`, `internal/storage/`)
- New background garbage collector — two phases per tick:
  - **Phase 1** (stale-pending sweep): marks pending jobs older than `redis.pending_max_age` as failed and deletes their S3 input files
  - **Phase 2** (S3 orphan cleanup): lists all S3 objects, groups by job ID, and deletes files whose Redis record has expired — covers both `input_ref` and `result_ref` orphans for all exit paths (failed webhooks, `persists_result: true`, never-polled results)
- Redis safeguard: if `Ping` or `MGET` fails, Phase 2 is skipped entirely and an error is logged — prevents mass deletion when Redis is temporarily unavailable
- Fully hot-reload safe — all GC parameters updated via atomics without pod restart

New `lifecycle.gc` config block:

| Field | Default | Description |
|---|---|---|
| `lifecycle.gc.enabled` | `false` | Master switch |
| `lifecycle.gc.interval` | `"15m"` | How often the GC runs |
| `lifecycle.gc.orphan_min_age` | `"5m"` | Minimum object age before orphan check — prevents race conditions on in-flight uploads |

**Lifecycle config block** (`internal/config/`)
- `lifecycle.persists_result`: controls whether Redis records and S3 results survive first consumption (`false` = immediate cleanup, `true` = keep until TTL)
- `lifecycle.job_ttl.*`: per-status Redis TTL (`global`, `completed`, `pending`, `failed`)
- All lifecycle parameters now hot-reload safe (`POST /-/reload`)

#### Changed

- **`redis.pending_max_age_hours` (int, hours) renamed to `redis.pending_max_age` (duration string)** e.g. `"2h"` — consistent with all other duration parameters. See upgrade notes.
- **`lifecycle.job_ttl.success` renamed to `lifecycle.job_ttl.completed`** — aligns with the `JobStatusCompleted` value used throughout the codebase.

#### Fixed

- `GET /jobs/{service_type}/{id}` no longer wipes the Redis record and S3 result on first fetch — clients can re-fetch within the TTL window without hitting a phantom 404
- S3 result file is now deleted after successful webhook delivery when `persists_result: false` — previously only `input_ref` was cleaned up on the webhook path

---

### [v0.10.0] — 2026-05-18

#### Added

**Async job endpoints** (`internal/handler/`, `internal/storage/`)
- `GET /jobs` — list jobs for the authenticated consumer (requires `consumer_header`), paginated (`limit`, `offset`), most-recent-first; includes `queue_position` for pending jobs
- `GET /jobs/{service_type}/{id}` — job status with `queue_position`, `result_ref`, `error`
- `DELETE /jobs/{service_type}/{id}` — cancel a pending job; returns 409 if already processing or terminal
- `POST /-/jobs/purge?older_than=<duration>[&limit=N]` — admin purge of stale pending jobs with S3 cleanup; supports pagination via `limit` + `truncated` response field

**Guardrails PII** (`internal/guardrails/`)
- PII detection for LLM JSON requests: email, phone FR, IBAN, credit card, SIREN/SIRET patterns
- Enabled per service via `guardrails.pii: true` in config
- Blocked requests logged as security events (`slog.Warn`, `level=WARN`)
- New Prometheus counter `GatewAI_guardrails_pii_blocked_total{service_type, model}`

**Audit trail** (`internal/llmproxy/`)
- Structured `slog` log per LLM request: `service_type`, `model`, `backend_model`, `provider`, `consumer`, `user_type`, `status`, `prompt_tokens`, `completion_tokens`, `cache_hit`, `duration_ms`, `backend_url`, `stream`
- Opt-in via `audit_log.enabled: true`; prompt body logging via `audit_log.prompt: true` (PII risk — disabled by default)
- New Grafana dashboard `GatewAI-audit-trail` (JSON in `dashboards/`)

**X-RateLimit headers** (`internal/ratelimit/`, `internal/handler/`)
- `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` on every response (async submit and sync), not only on 429
- `Check()` now returns `CheckResult{Allowed, Limit, Remaining, ResetAfter}` — replaces raw int return

**`/v1/models` capabilities** (`internal/handler/models.go`)
- Response enriched with `service_type`, `provider`, `capabilities` object: `supports_async`, `supports_sync`, `supports_streaming`, `accepted_formats`, `max_file_size_mb`, `operations`
- Capabilities derived live from registry — no static config

#### Fixed

- **Sync orphaned jobs on timeout**: `defer DeleteJob` was registered after the wait-loop return, so 504 timeouts left jobs stuck as `pending` in Redis indefinitely. Defer now fires on all exit paths including timeout.
- **Stale job scan status filter**: `scanStaleJobs` now filters `status == pending` to match `ListStalePendingJobs` / `AdminPurge` semantics.
- Background GC (`SweepStalePendingJobs`): prevented ghost resurrection (job re-marked stale after completing); now cleans S3 input files on sweep.
- Cancel restricted to `pending` only — returns 409 for processing/terminal states.
- New Prometheus counter `GatewAI_async_stale_jobs_swept_total{model}` for GC activity.

---

### [v0.9.0] — 2026-05-12

#### Added

**Queue position** (`internal/storage/`, `internal/handler/`)
- `GET /jobs/{service_type}/{id}` and `GET /jobs` now include `queue_position` (1-indexed) for pending jobs
- Position is tracked via Redis sorted set `queue:{model}` (ZADD on submit, ZREM on complete/delete)
- Atomic cleanup in existing Lua scripts (`updateJobScript`, `deleteJobScript`) — no extra round-trips
- `GET /jobs` batches all ZRANK calls in a single pipeline

**Unlimited rate limit** (`internal/ratelimit/`)
- `rate: 0` in config bypasses Redis entirely and always allows the request — no counter incremented
- Allows per-user-type "no limit" alongside normal fixed-window limits for other types
- Example: `unlimited: {rate: 0}` with `"*": {rate: 10, period: 1h}`

---

### [v0.8.0] — 2026-05-04

#### Added

**Multi-backend routing** (`internal/service/`, `internal/handler/`, `internal/llmproxy/`)
- `backends` list per service replaces or supplements `inference_url`: weighted-random primary selection, automatic retry on 5xx / network error, `weight: 0` for last-resort fallbacks
- Supports blue/green (`weight: 100/0`), canary (`weight: 90/10`), and fallback patterns
- Per-backend `headers` — override service-level `inference_headers` per backend (e.g. separate API keys per endpoint)
- Per-backend `model` — override service-level `backend_model` per backend, enabling model-version canaries (clients send one alias, each backend receives its real model name)
- Full backward compatibility: `inference_url` is transparently normalized to `[{url, weight: 1}]`

**SSE streaming** (`internal/llmproxy/`)
- `stream: true` LLM requests are piped directly to the client without buffering
- Retry loop runs before `w.WriteHeader`; once the stream starts, no backend switch occurs
- Cache and response translation are bypassed for streaming responses

**Rate limiting** (`internal/ratelimit/`)
- Per-consumer Redis fixed-window rate limiting keyed by `{consumer}:{service_type}:{user_type}` — multi-model services share a single counter
- Applies at job submission (async) and sync-direct proxy; returns `429` with `Retry-After` header
- New top-level config block: `rate_limits[service_type][user_type]: {rate: N, period: Xs}`
- New server config: `user_type_header` — HTTP header carrying consumer type (e.g. `X-User-Type`)
- PrometheusRule: `KeventGatewayRateLimitHighRejectionRate` (> 20 %, warning) and `KeventGatewayRateLimitErrors`

**Observability**
- `backend_model` label added to `GatewAI_llm_requests_total`, `GatewAI_llm_request_duration_seconds`, `GatewAI_llm_tokens_total`, `GatewAI_llm_tokens_per_request` — enables per-model-version dashboards during canary deployments
- New Grafana dashboard `GatewAI-llm.json` — LLM proxy metrics (requests, latency, tokens, cache, rate limiting)
- `metrics.top_consumers` and `metrics.consumer_labels` exposed in Helm `values.yaml`

**Updated Prometheus metrics**

| Metric | Labels |
|---|---|
| `GatewAI_llm_requests_total` | `service_type, model, backend_model, provider, user_type, status` |
| `GatewAI_llm_request_duration_seconds` | `service_type, model, backend_model, provider, user_type` |
| `GatewAI_llm_tokens_total` | `service_type, model, backend_model, user_type, type` |
| `GatewAI_llm_tokens_per_request` | `service_type, model, backend_model, user_type` (histogram) |
| `GatewAI_ratelimit_requests_total` | `service_type, user_type, result` |
| `GatewAI_ratelimit_consumer_hits_total` | `service_type, user_type` |
| `GatewAI_ratelimit_errors_total` | `service_type` |

#### Breaking changes
- `GatewAI_llm_*` metrics gain a `backend_model` label (empty string `""` when not configured) — existing dashboards and alerts targeting these metrics need to be updated

---

### [v0.7.0] — 2026-04-28

#### Added

**LLM proxy** (`internal/llmproxy/`, `internal/cache/`)
- Pluggable provider interface with four built-in implementations: `openai`, `anthropic` (full OpenAI ↔ Anthropic Messages API translation), `ollama`, and `passthrough` (vLLM and other OpenAI-compatible backends)
- Model aliases: clients send a short alias in the `model` field; gateway rewrites to `backend_model` before forwarding — e.g. `"gpt-4o"` → `"meta-llama/Meta-Llama-3-8B-Instruct"` for vLLM
- Redis response cache: exact-match keyed on SHA-256 of canonical request body; configurable TTL per service (`response_cache_ttl`); `stream=true` and `Cache-Control: no-cache` (multi-directive) bypass the cache; `X-Cache: HIT / MISS` on every LLM response; async cache-fill (5 s timeout, never blocks the HTTP response)
- Dynamic wildcard routing: operation paths ending with `/*` (e.g. `/v1/*`) register as chi wildcard routes — LLM services can proxy all OpenAI-compatible endpoints without enumerating them in config
- New config fields per service: `provider`, `backend_model`, `response_cache_ttl`

**Rate limiting** (`internal/ratelimit/`)
- Per-consumer Redis fixed-window rate limiting keyed by `{consumer}:{service_type}:{user_type}` — multi-model services share a single counter
- Applies at job submission (async) and sync-direct proxy; returns `429` with `Retry-After` header
- New top-level config block: `rate_limits[service_type][user_type]: {rate: N, period: Xs}`
- New server config: `user_type_header` — HTTP header injected by APISIX after token introspection (e.g. `X-User-Type`); used for rate limiting and metric labelling
- PrometheusRule: `KeventGatewayRateLimitHighRejectionRate` (> 20%, warning) and `KeventGatewayRateLimitErrors`
- New Grafana dashboard section: "Gateway — Rate Limiting" (5 panels including distinct consumer count via PromQL `group` trick)

**Consumer metrics** (`internal/metrics/consumer_tracker.go`)
- `ConsumerTracker` interface backed by Redis sorted sets (`ZINCRBY` on `llm:consumer:tokens:{user_type}:{token_type}`); top-N consumers loaded from Redis and exposed as `GatewAI_llm_consumer_tokens_top{consumer, user_type, type}`
- Enabled via `metrics.top_consumers: N`; refreshed every 60 s and immediately at startup
- `NoopTracker` used when tracking is disabled (zero overhead)

**New Prometheus metrics (10)**

| Metric | Labels |
|---|---|
| `GatewAI_llm_requests_total` | `service_type, model, provider, user_type, status` |
| `GatewAI_llm_request_duration_seconds` | `service_type, model, provider, user_type` |
| `GatewAI_llm_tokens_total` | `service_type, model, user_type, type` |
| `GatewAI_llm_tokens_per_request` | `service_type, model, user_type` (histogram) |
| `GatewAI_llm_consumer_tokens_top` | `consumer, user_type, type` (top-N gauge) |
| `GatewAI_cache_hits_total` | `service_type, model` |
| `GatewAI_cache_misses_total` | `service_type, model` |
| `GatewAI_cache_errors_total` | `service_type, model, op` |
| `GatewAI_ratelimit_requests_total` | `service_type, user_type, result` |
| `GatewAI_ratelimit_consumer_hits_total` | `service_type, user_type` |
| `GatewAI_ratelimit_errors_total` | `service_type` |

#### Fixed
- Cache-fill goroutine is now launched after `w.Write` — cache population never adds latency to the HTTP response
- `Cache-Control: no-cache` parsed with `strings.Contains` to correctly handle multi-directive values (e.g. `no-cache, no-store`)
- `IsLLM()` uses `Provider != ""` only; was incorrectly also triggering on `ResponseCacheTTL > 0`
- `api_key` removed from `ServiceConfig` — credentials belong in `inference_headers` (`Authorization: Bearer …`)
- Consumer `Track()` calls use `context.WithoutCancel` — billing is recorded even when the HTTP client disconnects mid-request

---

### [v0.6.4] — 2026-04-21

#### Changed
- CI: `ci.yml` and `deploy-dev.yml` skip gateway/relay/helm jobs when unrelated files change (`dorny/paths-filter`) — avoids unnecessary builds on pure docs or CI-config commits

---

### [v0.6.3] — 2026-04-15

#### Added
- `inference_headers` — per-service map of HTTP headers injected on every sync-direct proxy request (auth Bearer, API key…). Values support `${VAR}` env expansion. Applies to JSON requests and multipart without `sync_topic` only. Closes #18
- Smoke tests (`tests/smoke/smoke.sh`): automated end-to-end validation against a live gateway — Whisper async, Whisper sync, and Rerank sync-direct
- CI workflow `smoke-tests.yml`: triggered automatically after each dev deploy (`workflow_run`) and on demand via `/smoke-test` PR comment (members/collaborators only); posts pass/fail result as a PR comment
- All CI workflows migrated to organisation runner (`actions-runner-controller`)

#### Fixed
- Sync path routing: configured paths are now registered exactly in chi instead of prefix wildcards (`/prefix/*`). Chi handles `{model}` parameter patterns natively. Fixes 404 on single-segment endpoints (e.g. `POST /rerank`)
- Reserved gateway routes (`/health`, `/metrics`, `/docs`, `/openapi.yaml`, `/jobs`, `/-/`) cannot be overridden by a service config path — silently skipped with a warning log at startup
- CI: removed `-race` flag from `go test` (CGO not available on ARC runner)
- CI: `GATEWAY_URL` moved to secrets; separate `WHISPER_API_KEY` / `RERANK_API_KEY` for each service
- Docs: replaced unresolvable MkDocs cross-directory links (`../../examples/`) with inline code references, fixing the `Deploy docs` CI failure in `--strict` mode

#### Tests
- `cmd/gateway/main_test.go`: `TestReservedGatewayPath` (23 table-driven cases) + `TestReservedPathsNotOverriddenBySync` (chi router integration)

---

### [v0.6.2] — 2026-04-13

#### Fixed
- `Submit` handler: operation validation now occurs before S3 upload and Redis save — an invalid `operation` field no longer creates orphaned resources
- Helm: `checksum/config` annotation is no longer emitted when `configReloader` is enabled, preventing unnecessary rolling restarts on ConfigMap changes

#### Added
- Unit tests: `internal/handler/jobs_test.go`, `internal/crypto/aes_test.go`, `relay/internal/crypto/aes_test.go`, `internal/config/config_test.go`, `internal/handler/misc_test.go`
- `examples/` directory with generic, deployable manifests (KafkaUsers, KafkaSources, InferenceService)
- Documentation: replaced `k8s/` references with `examples/`, removed internal namespace naming

---

### [v0.6.1] — 2026-04-10

#### Added
- Consumer tracking via configurable `server.consumer_header` (e.g. `X-Consumer-Username` set by APISIX):
  - Consumer name stored in job record (`consumer_name` field)
  - Redis sorted set `consumer:{name}:jobs` indexed at submit time (score = creation timestamp)
  - `GET /jobs` endpoint: paginated job list for the authenticated consumer (`?limit=20&offset=0`)
  - `GatewAI_jobs_by_consumer_total{mode, service_type, model, consumer}` Prometheus counter
- Consumer ownership check on `GET /jobs/{service_type}/{id}`: if `consumer_header` is configured and the header is present, the job's `consumer_name` must match — returns 404 on mismatch (no information leak). Auth-less deployments (no header) are unaffected.
- `DeleteJob` now atomically cleans the consumer index via a Lua script (ZREM + DEL in a single round-trip)

#### Changed
- `NewJobHandler` accepts a new `consumerHeader string` parameter
- `NewSyncHandler` accepts a new `consumerHeader string` parameter
- `asyncJobStore` interface gains `ListJobsByConsumer(ctx, consumer, limit, offset)`

---

### [v0.6.0] — 2026-04-10

#### Added
- Redis operation metrics: `GatewAI_redis_operation_duration_seconds` histogram and `GatewAI_redis_errors_total` counter, both labelled by operation (`save_job`, `get_job`, `delete_job`, `update_job_result`)
- Priority routing: requests carrying the configurable `server.priority_header` are published to `services[].priority_topic` (Kafka), routed to `POST /sync` on the relay (sets `syncPriority++`, deferring async jobs)
- MkDocs Material documentation site: architecture, deployment, configuration reference, Kafka/SASL guide, gitflow, releasing guide — deployed automatically to GitHub Pages

#### Changed
- `helm-release.yml`: docs publishing step removed (dedicated `docs.yml` workflow handles it)
- `.github/workflows/docs.yml`: new workflow — builds MkDocs and deploys to `gh-pages` while preserving Helm `index.yaml` and `.tgz` packages

---

### [v0.5.3] — 2026-04-09

#### Fixed
- `POST /-/reload` now returns HTTP 200 instead of 204 — `configmap-reload` sidecar expects 200 and was treating 204 as an error.

---

### [v0.5.2] — 2026-04-09

#### Added
- `POST /-/reload` endpoint: hot-reloads config (service registry, Swagger specs, OpenAPI spec, routing table) without pod restart. Infrastructure (S3, Redis, Kafka connection) is not re-initialised.
- `ConsumerManager.Reconcile`: dynamically adds consumers for new Kafka result topics and stops consumers for removed topics on hot reload.
- `configReloader` sidecar in Helm chart (`ghcr.io/jimmidyson/configmap-reload`): watches the ConfigMap volume and triggers `/-/reload` automatically on ConfigMap update. Disabled by default (`configReloader.enabled: false`).

#### Changed
- Kafka producer and consumer manager are now created whenever `kafka.brokers` is configured, regardless of initial service count — enables adding Kafka services via hot reload without restart.
- `ConsumerManager.Start` now takes the initial `*service.Registry` as parameter (previously stored in the struct).

---

### [v0.5.1] — 2026-04-08

#### Added
- `swagger_headers` field on service config: optional HTTP headers sent when fetching `swagger_url`. Values support `${VAR}` env expansion. Useful for private GitHub release assets (`Accept: application/octet-stream`, `Authorization: Bearer ${GITHUB_TOKEN}`).

---

### [v0.5.0] — 2026-04-03

#### Changed
- `UpdateJobResult` is now atomic: replaced `GetJob + SaveJob` with a Lua script to eliminate the read-modify-write race window under concurrent consumers
- `Producer.writerFor` uses `sync.RWMutex` — hot path (existing writer) acquires only a read lock
- `ConsumerManager` exposes `Wait()` backed by a `sync.WaitGroup`; `main` now drains consumers and in-flight webhooks before `Shutdown`
- Webhook goroutines are tracked in the same `WaitGroup` (no longer fire-and-forget on shutdown)
- `JobHandler` depends on `s3Store`, `asyncJobStore`, `eventProducer` interfaces instead of concrete types

#### Fixed
- S3 input file and Redis record are now cleaned up when Kafka publish fails in both `Submit` and the sync handler (previously orphaned indefinitely)
- `NotifyJobDone` logs Redis publish errors instead of silently discarding them
- Malformed JSON bodies on `POST /v1/*` now return HTTP 400 instead of routing with an empty model
- Per-request `reserved` maps moved to package-level variables

---

### [v0.4.18] — 2026-04-02

#### Changed
- Swagger UI `/docs`: inference endpoints now grouped by service type (`audio`, `ocr`, …) instead of a single "Inference" section — tags are built dynamically from the registry

---

### [v0.4.17] — 2026-04-02

#### Added
- Swagger UI now shows an **Authorize** button for API key authentication
- `securitySchemes: ApiKeyAuth` (header `apikey`) added to the generated OpenAPI spec
- Global security applies to all endpoints; `/health` is explicitly public

---

### [v0.4.16] — 2026-04-02

#### Fixed
- `/docs` `$ref` resolver errors: serve service specs at `/docs/spec/{type}/{model}` (under the `/docs*` gateway prefix) instead of blob URLs — `swagger-client` cannot resolve fragment refs against `blob:` base URLs
- Remove 404 link to non-existent `swagger-ui-standalone-preset.css`

---

### [v0.4.15] — 2026-04-02

#### Fixed
- `/docs` no longer fetches `/swagger/{type}/{model}` from the browser — service specs are now embedded inline in the HTML and exposed as blob URLs, so only `/openapi.yaml` is requested externally

---

### [v0.4.14] — 2026-04-02

#### Fixed
- Swagger UI `/docs` "No layout defined for StandaloneLayout": load `swagger-ui-standalone-preset.js` as a separate script tag and reference it as the global `SwaggerUIStandalonePreset` (not `SwaggerUIBundle.SwaggerUIStandalonePreset`)

---

### [v0.4.13] — 2026-04-02

#### Fixed
- Swagger UI `/docs` showing "no api definition provided": switch from `BaseLayout` to `StandaloneLayout` — the `urls[]` multi-spec dropdown is a Topbar feature exclusive to `StandaloneLayout`

---

### [v0.4.12] — 2026-04-02

#### Added
- `swagger_url` optional field per service — OpenAPI JSON spec fetched from URL (e.g. raw GitHub) at startup and cached in memory
- `GET /swagger/{type}/{model}` — serves the cached spec for a service
- `GET /docs` — Swagger UI now shows a multi-spec dropdown: gateway spec + one entry per service with `swagger_url`
- Fetch failures (URL unreachable, HTTP error, invalid JSON) are logged as warnings and never block startup

---

### [v0.4.11] — 2026-04-01

#### Added
- **Mode sync-direct uniquement** : un service sans `input_topic`/`result_topic` est traité en proxy direct, sans Kafka
  - `POST /v1/*` : proxy direct vers `inference_url` (comportement inchangé)
  - `POST /jobs/{service_type}` : retourne 405 pour ces services
  - Le producer et le consumer Kafka ne sont initialisés que si au moins un service utilise des topics Kafka
- `kafka.brokers` n'est plus obligatoire si aucun service ne configure de topic Kafka
- `config.existingConfigMap` dans le chart Helm : permet de référencer une ConfigMap existante

---

### [v0.4.10] — 2026-03-31

#### Fixed
- `GatewAI_requests_total` now recorded on sync-direct path (`proxyToInference`) with `mode="sync-direct"`

---

### [v0.4.9] — 2026-03-30

#### Added
- Prometheus metrics exposed at `GET /metrics`:
  - `GatewAI_requests_total` (counter, labels: `mode`, `service_type`, `model`, `status`) — all completed requests
  - `GatewAI_request_duration_seconds` (histogram, labels: `mode`, `service_type`, `model`) — end-to-end handler latency
  - `GatewAI_sync_wait_duration_seconds` (histogram, labels: `service_type`, `model`) — time blocked on Redis pub/sub in sync-over-Kafka mode
  - `GatewAI_sync_jobs_in_flight` (gauge) — open sync-over-Kafka connections waiting for relay results
  - `GatewAI_s3_operation_duration_seconds` (histogram, label: `operation`: upload/get/delete) — S3 latency
  - `GatewAI_s3_errors_total` (counter, label: `operation`) — S3 failures
  - `GatewAI_kafka_publish_duration_seconds` (histogram, label: `topic`) — Kafka write latency
  - `GatewAI_kafka_publish_errors_total` (counter, label: `topic`) — Kafka publish failures

---

### [v0.4.8] — 2026-03-24

#### Fixed
- **Root cause of HTTP/2 INTERNAL_ERROR**: `applyDefaults()` checked `WriteTimeout == 0` and applied a 60 s default, silently overriding the `write_timeout: 0s` set in `config.yaml`. Go's HTTP server was killing every connection after 60 s — APISix received an abrupt close and sent `RST_STREAM INTERNAL_ERROR` to the client. Removed the default entirely: 0 = no server-level write timeout, which is correct for an inference gateway.

---

### [v0.4.7] — 2026-03-24

#### Fixed
- Remove `X-Accel-Buffering: no` response header. It triggered APISix streaming proxy mode which breaks APISix plugins (key-auth, response transforms) that need to read the full body — resulting in `curl (92) HTTP/2 stream INTERNAL_ERROR`. Keepalive `\n` writes every 20 s are sufficient to keep the upstream connection alive within `proxy_read_timeout`.

---

### [v0.4.6] — 2026-03-24

#### Fixed
- Sync-over-Kafka: delay HTTP stream commitment to first keepalive tick (20 s) instead of flushing immediately. Fast inferences (< 20 s) now return proper HTTP status codes (422, 504, 500). Long inferences commit the 200 on the first tick to prevent APISix/nginx idle-connection drops; errors after that go in the JSON body.
- All unit tests pass again (`TestSyncHandler_ClientDisconnect`, `TestSyncHandler_InferenceFailure`)

---

### [v0.4.5] — 2026-03-24

#### Fixed
- Sync-over-Kafka: send HTTP 200 + headers immediately on connection open, then write a JSON-whitespace newline every 20 s while waiting for the relay result. Prevents APISix/nginx from dropping the TCP connection during long inferences ("upstream prematurely closed connection while reading response header")
- Remove duplicate `Content-Type` header set at end of `handleMultipartViaKafka`

---

### [v0.4.4] — 2026-03-23

#### Added
- Extra form fields (e.g. `vad_parameters`) are now forwarded end-to-end in all modes: gateway collects non-reserved fields from the multipart form, stores them in `InputEvent.Params`, and the relay injects them into the multipart request sent to the inference API
- `Params` override `extra_fields` from relay config when both define the same key

---

### [v0.3.0] — 2026-03-13

#### Added
- Sync-over-Kafka: `POST /v1/*` multipart requests are now routed through a dedicated priority Kafka topic (`sync_topic`) instead of proxied directly, when `sync_topic` is configured for a service
- `internal/storage/redis.go`: `SubscribeJobDone` / `NotifyJobDone` for Redis pub/sub result notification
- `internal/kafka/consumer.go`: notifies sync waiters via pub/sub when a result arrives
- `SyncTopic` field in `ServiceConfig` and `service.Def`

#### Changed
- `SyncHandler` now requires `s3`, `redis`, and `producer` dependencies (sync-over-Kafka path)
- JSON requests (`/v1/chat/completions`) continue to use direct proxy regardless of `sync_topic`

---

### [v0.2.7] — 2026-03-13

#### Added
- Startup log indicating whether at-rest encryption is enabled (`"S3 storage initialised" encryption=true/false`)

---

### [v0.2.6] — 2026-03-12

#### Fixed
- S3 upload failing with `request stream is not seekable` when encryption is enabled — replaced `PutObject` with `s3manager.Uploader` (multipart) which handles non-seekable `io.Pipe` streams natively

#### Changed
- Removed provider-specific references (Scaleway) from log messages and code comments

---

### [v0.2.5] — 2026-03-11

#### Added
- AES-256-GCM at-rest encryption for S3 objects (`internal/crypto/aes.go`) — chunked streaming, no extra dependencies
- `encryption.key` config field; empty = encryption disabled
- `model` field in `POST /jobs/{service_type}` multipart body — selects inference backend when multiple models share a type
- `Job.Model` and `InputEvent.Model` fields in data model

#### Changed
- Job routes restructured: `POST /jobs/{service_type}`, `GET /jobs/{service_type}/{id}`
- `openai_path` (string) → `openai_paths` (list) — each model can expose multiple OpenAI-compatible paths
- `inference_url` is now a base URL; the original request path is appended at runtime
- Kafka topics renamed from `jobs.<type>.*` to `jobs.<model>.*`
- `registry.RouteAsync(serviceType, model)` replaces `registry.Route(serviceType)` — supports multi-model types
- `MaxFileSizeForType` used for `MaxBytesReader` before multipart parse (maximum across all models for the type)
- Docker image moved to `ghcr.io/qatr-io/gatewai/gateway`

### [v0.2.4] — 2026-01-XX

#### Added
- SASL/TLS Kafka authentication (`internal/kafka/auth.go`)
- `kafka.sasl` and `kafka.tls` config sections
- Fix: `buildTransport` returns typed nil — must not assign directly to `RoundTripper` interface

---

## Relay

### [Unreleased]

### [v0.12.0] — 2026-08-27

#### Added

- **Per-job processing lease**: on `Pop` the relay writes a short-lived lease key `relay:{model}:lease:{jobID}` and refreshes it every `lease_ttl/3` (config `lease_ttl`, default 60s) for the lifetime of the job; `Done` deletes it. If the pod dies mid-job the lease expires and the gateway reaper requeues the job (see Gateway). Enables crash recovery for jobs that previously stayed orphaned in the processing list forever.

### [v0.11.3] — 2026-07-28

#### Changed

- **`gatewai_relay_inference_duration_seconds` now has a `model` label** (in addition to the existing `service_type`), so p95/p50 inference duration can be broken down per model, not just per service type. Updated the Grafana dashboard panel and docs accordingly. Existing PromQL queries that `sum by (le, service_type)` without also grouping by `model` will now double-count across models with the same `service_type` — update them to include `model` in the `by (...)` clause.

### [v0.11.2] — 2026-07-27

#### Fixed

- **OTLP metrics push was a no-op**: `opentelemetry.metrics.enabled: true` started a fully-wired OTLP metric export pipeline (exporter, `MeterProvider`, `PeriodicReader`), but nothing ever fed it — the relay's actual metrics (`gatewai_relay_jobs_total`, `gatewai_relay_inference_duration_seconds`, `gatewai_relay_s3_errors_total`, etc.) live in the `client_golang`/`promauto` default registry, which was never scraped (relay has no `/metrics` endpoint) nor bridged to the OTel SDK. For the one-shot relay Job pod this meant those metrics never reached any backend. Fixed by adding `go.opentelemetry.io/contrib/bridges/prometheus` as a `sdkmetric.Producer` on the metrics `PeriodicReader`, so the existing Prometheus collectors are gathered and exported over OTLP on every flush (including the final flush on shutdown). No config or metric-name changes — this only makes the already-documented `metrics.enabled: true` behavior (see [OpenTelemetry docs](https://qatr-io.github.io/GatewAI/observe/opentelemetry/#relay-one-shot-jobs-and-otlp-push)) actually work.

### [v0.11.1] — 2026-07-23

#### Fixed

- **Configurable inference health-check URL**: the startup readiness check always derived the health-check URL as `inference.base_url + "/health"`, with no way to point it at a different path or host/port than the one used for actual inference requests. Added an optional `inference.health_url` config field (env `INFERENCE_HEALTH_URL`) that overrides the derived URL when set.

### [v0.11.0] — 2026-07-21

#### Changed

- **Gateway completion callback**: after persisting a job's result to Redis, the relay now makes a single bounded HTTP call (`POST /-/relay/jobs/{id}/complete`, 5s timeout, no retry) to the gateway instead of debiting rate-limit/usage budgets and delivering the webhook itself. New required config: `gateway.base_url` (env `GATEWAY_BASE_URL`), the gateway's in-cluster Service URL. New metric `gatewai_relay_gateway_callback_errors_total` for callback failures — the job's own result is unaffected, only the debit/usage/webhook side effects are skipped for that job.

### [v0.10.0] — 2026-07-09

#### Added

- **Prompt/completion token extraction**: the relay now extracts `prompt_tokens`/`completion_tokens` from the inference backend's result JSON (when present) and plumbs them through the result-publishing pipeline (`PublishResult`, Redis job record) so the gateway can account for async job token usage against `token_limits` and the new per-service usage tracking (see Gateway `v0.19.0`).

### [v0.9.0] — 2026-06-24

#### Added

- **Configurable log level**: `log_level` config field (or `LOG_LEVEL` env var) sets the relay's structured log verbosity (`debug` | `info` | `warn` | `error`). Debug mode emits detailed per-operation logs for all traced OTel spans.
- **OTel spans for S3 and Redis operations**: new child spans under `relay.process_job` — `relay.s3.download`, `relay.s3.upload`, `relay.s3.delete`, `relay.redis.get_job`, `relay.redis.publish_result` — enabling per-operation latency visibility in distributed traces.

#### Fixed

- **`relay.redis.get_job` span timing**: span creation deferred until the traceparent is extracted from the job record, preventing incorrect parent-less root spans.
- **Trace fields in relay logs**: `trace_id` and `span_id` are now correctly injected into all relay structured log entries during job processing.

---

### [v0.8.0] — 2026-06-19

#### Added

- **OpenTelemetry observability**: distributed tracing and OTLP push (traces, metrics, logs) via the `opentelemetry:` config block. Spans: `relay.process_job` (restores gateway trace context from `job.trace_context`) and `relay.inference_call` (injects W3C `traceparent` header into the HTTP call to the inference API). OTLP metrics push is especially useful since the relay runs as a one-shot k8s Job and cannot be Prometheus-scraped; a deferred 10-second shutdown ensures `ForceFlush` completes before pod exit.
- **OTel SDK upgraded to v1.44.0** (requires Go 1.25); Dockerfile updated to `golang:1.25-alpine`.

---

### [v0.7.3] — 2026-06-16

#### Added

- **Processing time propagation**: `processing_time` field from `result.json` is now extracted and included in `PublishResult`, allowing the gateway to account for inference duration in processing time budgets.

---

### [v0.7.2] — 2026-06-12

#### Added

- **S3 path-style and SSL insecure options**: `s3.use_path_style` (default `true`) and `s3.ssl_insecure` config fields for S3-compatible endpoints that require path-style URLs or self-signed certificates.

#### Fixed

- **S3 checksum headers**: disabled automatic AWS checksum headers (`x-amz-checksum-*`) for S3-compatible endpoints that reject them.

---

### [v0.7.1] — 2026-06-09

#### Added

- **S3 custom CA bundle**: `s3.ca_bundle` config field (env `S3_CA_BUNDLE`) — path to a PEM file with CA certificates for S3 TLS verification. Required when the S3 endpoint uses a private or self-signed CA. Defaults to the system certificate pool when unset.

---

### [v0.7.0] — 2026-06-05

#### Changed

- **Go module renamed**: `kevent/relay` → `gatewai/relay`
- **Docker image moved**: `ghcr.io/qatr-io/gatewai/relay:vX.Y.Z` (first image published under the GatewAI name)

---

### [v0.6.2] — 2026-06-05

#### Changed

- **Prometheus metrics**: replaced `GatewAI_relay_kafka_publish_errors_total` (removed with Kafka) by two Redis-specific counters:
  - `GatewAI_relay_redis_publish_errors_total` — failures publishing the completion notification to `jobs:<model>:completed` (Redis pub/sub)
  - `GatewAI_relay_redis_done_errors_total` — failures removing the job from `relay:<model>:processing` after completion

---

### [v0.6.1] — 2026-06-04

#### Changed

- **Kafka consumer replaced by Redis queue** (`relay/internal/queue/`): relay now pops jobs from `relay:<model>:pending` via `BLMOVE` instead of consuming a Kafka topic. Eliminates the `kafka-go` dependency and Kafka credentials from the relay config.
- **One job per pod lifecycle**: relay processes a single job then exits cleanly, letting Kubernetes restart it for the next job. `queue_pop_timeout` (default `30s`) controls how long the relay waits for a job before exiting with code 0.

#### Added

- `queue_pop_timeout` config field — duration the relay waits on the Redis queue before exiting (e.g. `"30s"`). Exit code 0 when the queue is empty after the timeout.

#### Fixed

- `context.Canceled` from inference is now propagated correctly instead of publishing a failed result event — avoids spurious failures on clean shutdowns.
- Context propagated to the inference HTTP call (`relay/internal/adapter/`) — cancellation now reaches the in-flight request.

#### Removed

- `PodAnnotator` / pod-deletion-cost mechanism — no longer needed since the relay processes one job and exits; Kubernetes does not need to deprioritise pods mid-inference.
- All Kafka config fields (`kafka.*`, `input_topic`, `consumer_group`) — replaced by `redis.addr` / `redis.queue_pop_timeout`.

---

### [v0.6.0] — 2026-06-01

#### Changed

- **Rewritten as Kafka pull consumer** (`relay/internal/kafka/`): relay now pulls jobs directly from the Kafka input topic using a `kafka-go` consumer group instead of being invoked by Knative KafkaSource. Eliminates the KafkaSource and Knative dependency from the inference deployment.

#### Added

- `kafka.input_topic` and `kafka.consumer_group` config fields.
- `kafka.Consumer` pull-based reader with SASL/TLS support (reuses `buildDialer` from auth.go).
- `PodAnnotator`: sets `controller.kubernetes.io/pod-deletion-cost` during inference to deprioritise the pod for deletion under load.
- Graceful shutdown: waits for the in-flight job to complete before exiting.
- Readiness gate: relay waits for the local inference service to become healthy before consuming the first message.

#### Fixed

- Defers now run on Kafka fetch errors instead of calling `os.Exit` directly.

---

### [v0.5.4] — 2026-05-28

#### Fixed

- **Graceful shutdown**: relay process now waits for all in-flight jobs to complete before exiting (`WaitIdle()`). Previously `main()` returned as soon as `srv.Shutdown()` timed out (default 30 s), killing goroutines mid-inference and losing the result.
- **Result persistence after queue-proxy timeout**: S3 upload and Kafka publish of the inference result now run on a context detached from the HTTP request (`context.WithoutCancel`). A Knative queue-proxy timeout that cancelled the request context no longer silently discards the result.
- **Liveness probe false positive during inference**: `/health` now returns 200 immediately when a job is in progress, skipping the upstream inference model health check. Previously the model's busy response caused repeated liveness failures, triggering a SIGTERM after `failureThreshold × periodSeconds` (~150 s with the default KServe probe config) while inference was still running.

---

### [v0.5.3] — 2026-05-18

Version bump aligned with gateway v0.11.0 release. No relay code changes.

---


### [v0.5.2] — 2026-05-18

#### Fixed

- **S3 NoSuchKey permanent failure**: `GetObject` errors wrapping `*s3types.NoSuchKey` are now detected via `errors.As` (new `storage.IsNotFound` helper). A permanent failure ResultEvent is published immediately so the gateway stops waiting; KafkaSource does not retry. Previously the relay returned a transient error on every 404, causing an infinite retry loop.
- **Inference context detach**: the inference HTTP call now uses `context.Background()` instead of the Knative request context. Knative's `timeoutSeconds` was cancelling in-flight inference calls, triggering an immediate KafkaSource retry on a job that was still running.
- **`timeout: 0s`** now disables the inference HTTP client timeout entirely (previously treated as an invalid value).

---

### [v0.5.1] — 2026-04-03

#### Fixed
- Proxy metrics (`GatewAI_relay_proxy_requests_total`, `GatewAI_relay_proxy_duration_seconds`) now carry the correct `service_type` label, derived automatically from `result_topic` (`jobs.<type>.results` → `<type>`). The `SERVICE_TYPE` env var is no longer required in any ServingRuntime.

---

### [v0.5.0] — 2026-04-03

#### Fixed
- `publishFailure` returns an error — when Kafka is unavailable after an inference failure, the error propagates so KafkaSource retries the job (previously the job was stuck in `pending` indefinitely)
- `newInferenceProxy` exits with `os.Exit(1)` on invalid `inference.base_url` instead of silently falling back to a hardcoded address
- `io.ReadAll` replaces `bytes.Buffer` in `decodeInputEvent`

---

### [v0.4.7] — 2026-03-31

#### Added
- `GatewAI_relay_proxy_requests_total` (counter, labels: `service_type`, `status`) — sync-direct requests proxied to the local model
- `GatewAI_relay_proxy_duration_seconds` (histogram, label: `service_type`) — sync-direct proxy latency

---

### [v0.4.6] — 2026-03-30

#### Added
- Prometheus metrics exposed at `GET /metrics`:
  - `GatewAI_relay_jobs_total` (counter, labels: `service_type`, `status`: completed/failed) — job outcomes
  - `GatewAI_relay_inference_duration_seconds` (histogram, label: `service_type`) — inference API call latency
  - `GatewAI_relay_input_size_bytes` (histogram, label: `service_type`) — input file size distribution
  - `GatewAI_relay_sync_priority` (gauge) — number of sync jobs in progress on this pod (non-zero defers async)
  - `GatewAI_relay_deferred_total` (counter) — async jobs returned 503 due to sync priority
  - `GatewAI_relay_s3_operation_duration_seconds` (histogram, label: `operation`: get/put/delete) — S3 latency
  - `GatewAI_relay_s3_errors_total` (counter, label: `operation`) — S3 failures
  - `GatewAI_relay_kafka_publish_errors_total` (counter) — result-event publish failures

---

### [v0.4.5] — 2026-03-30

#### Fixed
- Sync-direct requests (gateway in direct-proxy mode, no `syncTopic`) arriving on paths like `/v1/audio/transcriptions` were caught by the relay's `"/"` catch-all and rejected with 400 "missing job_id". KafkaSource always POSTs to exactly `"/"`, so any other path is now reverse-proxied transparently to the local inference model (`inference.base_url`).

---

### [v0.4.4] — 2026-03-24

#### Fixed
- Use `context.Background()` when publishing failure result events and deleting the input file after inference errors. Prevents silent loss of result notifications when the Knative request context is cancelled (e.g. `timeoutSeconds` exceeded), which would leave the gateway waiting indefinitely

---

### [v0.4.3] — 2026-03-23

#### Added
- `InputEvent.Params` forwarded into the multipart request to the inference API
- `Params` (from request) merged with `extra_fields` (from config), request params take precedence

---

### [v0.3.0] — 2026-03-13

#### Added
- `ServeHTTPSync` endpoint (`POST /sync`): priority CloudEvent handler that sets an in-pod `syncPriority` flag for the duration of the job
- `syncPriority atomic.Int32` field on `Relay`

#### Changed
- `ServeHTTP` (async handler): returns `503 Service Unavailable` when a sync job is in progress, causing KafkaSource to retry with backoff — giving sync jobs first access to the GPU
- Removed `SyncProxy` (`/v1/*` direct proxy): sync requests now arrive via the dedicated KafkaSource → `/sync` path

---

### [v0.2.7] — 2026-03-13

#### Added
- Startup log indicating whether at-rest encryption is enabled (`"S3 storage initialised" encryption=true/false`)

---

### [v0.2.6] — 2026-03-12

#### Fixed
- S3 upload failing with `request stream is not seekable` when encryption is enabled — replaced `PutObject` with `s3manager.Uploader` (multipart)

#### Changed
- Removed provider-specific references (Scaleway) from code comments

---

### [v0.2.5] — 2026-03-11

#### Added
- AES-256-GCM at-rest decryption/encryption for S3 downloads/uploads (`internal/crypto/aes.go`)
- `encryption.key` config field
- `InputEvent.Model` field support
- `result_topic` auto-derived from active model: `jobs.<model>.results` when left empty

#### Changed
- Docker image moved to `ghcr.io/qatr-io/gatewai/relay`

### [v0.2.4] — 2026-01-XX

#### Added
- SASL/TLS Kafka authentication (`internal/kafka/auth.go`)
- `KAFKA_SASL_USERNAME` / `KAFKA_SASL_PASSWORD` / `KAFKA_CA_CERT_PATH` env vars

---

## Helm chart (gatewai-gateway)

### [0.23.1] — 2026-09-28

#### Changed
- `appVersion` / `image.tag` → `v0.23.1`

---

### [0.23.0] — 2026-09-15

#### Added
- `backendPools` — named groups of member backends referenced from `services[].backendPool` instead of `inferenceURL`/`backends`, sharing a rate limit and/or concurrency budget (and default auth headers) across multiple service/model aliases pointed at the same physical backend. See `values.yaml` and the README's "Backend pools" section for the full schema.

#### Changed
- `appVersion` / `image.tag` → `v0.23.0`

---

### [0.22.0] — 2026-09-07

#### Changed
- `version` → `0.22.0`, `appVersion` / `image.tag` → `v0.22.0`

---

### [0.20.3] — 2026-07-31

#### Changed
- `appVersion` / `image.tag` → `v0.20.2`

---

### [0.20.2] — 2026-07-28

#### Added
- `server.maxBodyMB` — caps the request body (MiB) on the sync JSON path (`POST /v1/*`); oversized requests get a clean `413`. `0` = 1 MiB default.

#### Changed
- `appVersion` / `image.tag` → `v0.20.1`

---

### [0.20.1] — 2026-07-27

#### Added
- Two `PrometheusRule` alerts in the `gatewai-relay` group, both requiring the relay queue to be non-empty *and* zero `gatewai_jobs_total` completions for that model over the same window (since `gatewai_relay_queue_depth` has no per-job ID/age, and either metric alone would false-positive under normal load): `RelayJobsPendingTooLong` (`pending` queue, `thresholds.jobsPendingFor`, default `15m` — the relay isn't picking up jobs at all, as opposed to being merely overloaded but still consuming) and `RelayJobsRunningTooLong` (`processing` queue, `thresholds.jobsRunningFor`, default `1h` — a job is stuck rather than the model just being busy).

#### Changed
- All shipped `PrometheusRule` alerts renamed to drop the legacy `Kevent` prefix (e.g. `KeventGatewayHighErrorRate` → `GatewayHighErrorRate`), matching the runbook naming already used in the docs.

---

### [0.19.0] — 2026-07-09

#### Added
- `usage.retention` config field — usage-tracking sorted-set TTL.

#### Changed
- `appVersion` / `image.tag` → `v0.19.0`

---

### [0.15.2] — 2026-06-09

#### Fixed
- `s3.existingCABundle` : le volume est maintenant monté depuis un `Secret` (`secret.secretName`) au lieu d'un `ConfigMap`.

---

### [0.5.11] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.18`

---

### [0.5.10] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.17`

---

### [0.5.9] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.16`

---

### [0.5.8] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.15`

---

### [0.5.7] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.14`

---

### [0.5.6] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.13`

---

### [0.5.5] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.12`

---

### [0.5.4] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.11` (already released)

---

### [0.5.3] — 2026-04-01

#### Added
- `config.existingConfigMap` : référencer une ConfigMap existante au lieu de laisser le chart en créer une
- `input_topic` / `result_topic` conditionnels dans le template ConfigMap (services sync-direct)

#### Changed
- `appVersion` / `image.tag` → `v0.4.11`

---

### [0.5.2] — 2026-04-01

#### Added
- `config.existingConfigMap` option (intégré dans 0.5.3)

---

### [0.5.1] — 2026-03-31

#### Changed
- `image.tag` bumped to `v0.4.10` (gateway sync-direct metrics fix)
- `appVersion` bumped to `v0.4.10`

---

### [0.5.0] — 2026-03-30

#### Added
- `metrics.serviceMonitor.enabled` — crée un `ServiceMonitor` (Prometheus Operator) pointant sur `GET /metrics` du gateway
- Valeurs disponibles : `namespace`, `interval` (défaut: 30s), `scrapeTimeout` (défaut: 10s), `additionalLabels`, `relabelings`, `metricRelabelings`

---

### [0.3.0] — 2026-03-13

#### Added
- `services[].syncTopic` — optional priority Kafka topic for sync-over-Kafka routing
- ConfigMap template now renders `sync_topic` when set

#### Changed
- `appVersion` and `image.tag` updated to `0.3.0`

---

### [0.2.3] — 2026-03-13

#### Changed
- `appVersion` updated to `0.2.7`
- `image.tag` default updated to `v0.2.7`

---

### [0.2.2] — 2026-03-13

#### Fixed
- ConfigMap template missing `encryption.key: "${ENCRYPTION_KEY:-}"` — the env var was injected in the pod but never referenced in `config.yaml`, so encryption was always disabled regardless of key configuration

---

### [0.2.1] — 2026-03-12

#### Changed
- `appVersion` updated to `0.2.6`
- `image.tag` default updated to `v0.2.6`

---

### [0.2.0] — 2026-03-11

#### Added
- `encryption.key` / `encryption.existingSecret` — AES-256-GCM key injection (Option A: chart creates Secret, Option B: External Secrets)
- `gatewai-gateway.encryptionSecretName` helper in `_helpers.tpl`
- `README.md` for the chart

#### Changed
- `services[].openai_path` → `services[].openaiPaths` (list)
- `services[].inferenceURL` is now a base URL
- Topic fields renamed to model-based convention (`jobs.<model>.*`)
- `appVersion` updated to `0.2.5`

### [0.1.0] — 2026-01-XX

#### Added
- Initial chart: Deployment, Service, Ingress, ConfigMap, Secret
- Redis HA subchart (dandydeveloper/redis-ha)
- S3 credentials: `s3.accessKey/secretKey` or `s3.existingSecret`
- Kafka SASL: `kafka.sasl.password` or `kafka.sasl.existingSecret`
- Kafka TLS: `kafka.tls.existingCACertSecret`
