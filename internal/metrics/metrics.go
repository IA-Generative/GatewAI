package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"
)

// ObserveWithExemplar records value on h. When ctx holds an active sampled span,
// the trace_id is attached as a Prometheus exemplar so Grafana can link the data
// point directly to the corresponding trace in Tempo.
func ObserveWithExemplar(ctx context.Context, h prometheus.Observer, value float64) {
	type exemplarObserver interface {
		ObserveWithExemplar(value float64, labels prometheus.Labels)
	}
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		if eo, ok := h.(exemplarObserver); ok {
			eo.ObserveWithExemplar(value, prometheus.Labels{
				"trace_id": span.SpanContext().TraceID().String(),
			})
			return
		}
	}
	h.Observe(value)
}

var (
	// RequestsTotal counts all completed requests labelled by mode (async/sync),
	// service_type, model, and HTTP status code.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_requests_total",
		Help: "Total number of requests handled by the gateway.",
	}, []string{"mode", "service_type", "model", "status"})

	// RequestDuration measures end-to-end handler latency.
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_request_duration_seconds",
		Help:    "End-to-end request duration in seconds.",
		Buckets: []float64{.1, .5, 1, 5, 10, 30, 60, 120, 300},
	}, []string{"mode", "service_type", "model"})

	// S3OperationDuration measures latency for each S3 operation (upload/get/delete).
	S3OperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_s3_operation_duration_seconds",
		Help:    "S3 operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// S3ErrorsTotal counts S3 operation failures.
	S3ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_s3_errors_total",
		Help: "Total number of S3 operation errors.",
	}, []string{"operation"})

	// RedisOperationDuration measures latency for each Redis operation.
	RedisOperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_redis_operation_duration_seconds",
		Help:    "Redis operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// RedisErrorsTotal counts Redis operation failures.
	RedisErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_redis_errors_total",
		Help: "Total number of Redis operation errors.",
	}, []string{"operation"})

	// JobsByConsumerTotal counts submitted jobs per consumer, labelled by
	// service_type, model, and consumer name (from the configurable consumer header).
	// Only incremented when consumer_header is configured and the header is present.
	JobsByConsumerTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_jobs_by_consumer_total",
		Help: "Total number of jobs submitted per consumer.",
	}, []string{"mode", "service_type", "model", "consumer"})

	// RateLimitRequestsTotal counts rate-limit evaluations, labelled by
	// service_type, user_type (from the configurable user_type_header), and result
	// ("allowed" or "rejected"). Consumer name is intentionally omitted to keep
	// cardinality low; use RateLimitConsumerHitsTotal for per-consumer analysis.
	// Only populated when rate_limits is configured.
	RateLimitRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_ratelimit_requests_total",
		Help: "Total number of requests evaluated by the rate limiter, by outcome.",
	}, []string{"service_type", "user_type", "result"})

	// RateLimitConsumerHitsTotal counts rate-limit evaluations per consumer,
	// enabling `count by (user_type) (group by (...) (...))` in PromQL to get
	// the number of distinct consumers per user_type.
	// Only populated when both rate_limits and consumer_header are configured.
	RateLimitConsumerHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_ratelimit_consumer_hits_total",
		Help: "Total rate-limit evaluations per consumer (enables distinct consumer count per user_type via PromQL group).",
	}, []string{"service_type", "user_type", "consumer"})

	// RateLimitErrorsTotal counts Redis errors during rate-limit evaluation (fail-open).
	RateLimitErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_ratelimit_errors_total",
		Help: "Total number of Redis errors during rate-limit checks (requests are allowed on error).",
	}, []string{"service_type"})

	// TokenRatelimitCheckedTotal counts token budget checks by result (allowed|rejected).
	TokenRatelimitCheckedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_token_ratelimit_checked_total",
		Help: "Token rate limit checks by result (allowed|rejected).",
	}, []string{"service_type", "user_type", "result"})

	// TokenRatelimitErrorsTotal counts Redis errors during token rate limit operations (fail-open).
	TokenRatelimitErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_token_ratelimit_errors_total",
		Help: "Redis errors during token rate limit checks or updates (requests are allowed on error).",
	}, []string{"service_type"})

	// ConcurrentJobChecksTotal counts concurrent job limit evaluations.
	// Labels: service_type, user_type, result (allowed|rejected).
	ConcurrentJobChecksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_concurrent_job_checks_total",
		Help: "Total number of concurrent job limit checks, by outcome.",
	}, []string{"service_type", "user_type", "result"})

	// ProcessingTimeChecksTotal counts processing time budget evaluations.
	// Labels: service_type, user_type, result (allowed|rejected).
	ProcessingTimeChecksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_processingtime_checks_total",
		Help: "Total number of processing time budget checks, by outcome.",
	}, []string{"service_type", "user_type", "result"})

	// LLM proxy + cache metrics
	CacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_cache_hits_total",
		Help: "LLM response cache hits.",
	}, []string{"service_type", "model"})

	CacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_cache_misses_total",
		Help: "LLM response cache misses.",
	}, []string{"service_type", "model"})

	CacheErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_cache_errors_total",
		Help: "LLM response cache errors.",
	}, []string{"service_type", "model", "operation"}) // operation: get|set|key

	// user_type label = "sa" | "user" | "" (when UserTypeHeader not configured).
	LLMTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_llm_tokens_total",
		Help: "Tokens served by LLM requests (prompt+completion, includes cache hits).",
	}, []string{"service_type", "model", "backend_model", "user_type", "type"})

	LLMRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_llm_requests_total",
		Help: "Total LLM requests by provider, user_type, stream, and HTTP status.",
	}, []string{"service_type", "model", "backend_model", "provider", "user_type", "status", "stream"})

	LLMRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_llm_request_duration_seconds",
		Help:    "End-to-end LLM request latency.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"service_type", "model", "backend_model", "provider", "user_type", "stream"})

	// LLMTimeToFirstToken measures the delay between the gateway writing SSE
	// response headers to the client and the first chunk of the backend's
	// stream being received, for streaming LLM requests only.
	LLMTimeToFirstToken = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_llm_time_to_first_token_seconds",
		Help:    "Time from SSE stream start to the first chunk received from the backend, for streaming LLM requests.",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2, 5, 10, 30},
	}, []string{"service_type", "model", "backend_model", "provider", "user_type"})

	// BackendCircuitOpen is 1 while a backend's circuit is open (being skipped),
	// 0 when closed. Per model+backend.
	BackendCircuitOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gatewai_backend_circuit_open",
		Help: "1 when an LLM backend's circuit is open (skipped), 0 when closed.",
	}, []string{"model", "backend"})

	// BackendCircuitOpensTotal counts how many times a backend's circuit opened.
	BackendCircuitOpensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_backend_circuit_opens_total",
		Help: "Number of times an LLM backend's circuit transitioned to open.",
	}, []string{"model", "backend"})

	// BackendCircuitSkippedTotal counts requests that skipped a backend because
	// its circuit was open.
	BackendCircuitSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_backend_circuit_skipped_total",
		Help: "Requests that skipped an LLM backend because its circuit was open.",
	}, []string{"model", "backend"})

	// LLMFallbackTotal counts requests re-routed from a degraded model to its
	// configured fallback model (all primary backends circuit-open).
	LLMFallbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_llm_fallback_total",
		Help: "Requests routed to a fallback model because the primary's backends were all circuit-open.",
	}, []string{"service_type", "model", "fallback"})

	// BackendPoolRateLimitedTotal counts backend attempts skipped because a
	// backend_pools rate limit was exhausted. member is empty for a pool-wide
	// rejection, or the backend URL for a per-member rejection.
	BackendPoolRateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_backend_pool_rate_limited_total",
		Help: "Backend attempts skipped because a backend_pools rate limit (pool-wide or per-member) was exhausted.",
	}, []string{"pool", "member"})

	// BackendPoolConcurrencyRejectedTotal counts requests rejected because a
	// backend_pools concurrency limit (max_concurrent) was saturated.
	BackendPoolConcurrencyRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_backend_pool_concurrency_rejected_total",
		Help: "Requests rejected because a backend_pools concurrency limit was saturated.",
	}, []string{"pool"})

	// BackendPoolRateLimitErrorsTotal counts Redis errors during backend_pools
	// rate-limit checks (fail-open — the attempt is allowed through on error).
	BackendPoolRateLimitErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_backend_pool_rate_limit_errors_total",
		Help: "Redis errors during backend_pools rate-limit checks (attempts are allowed on error).",
	}, []string{"pool"})

	// LLMTokensPerRequest is a histogram of tokens per request, enabling p50/p95/p99
	// analysis by user_type. Useful to detect large contexts and capacity planning.
	LLMTokensPerRequest = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_llm_tokens_per_request",
		Help:    "Distribution of total tokens (prompt+completion) per LLM request.",
		Buckets: []float64{50, 100, 250, 500, 1000, 2000, 5000, 10000, 32000, 100000},
	}, []string{"service_type", "model", "backend_model", "user_type"})

	// LLMConsumerTokensTop exposes the top-N consumers by token usage, refreshed
	// periodically from a Redis sorted set. Only populated when metrics.top_consumers > 0.
	LLMConsumerTokensTop = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gatewai_llm_consumer_tokens_top",
		Help: "Token usage for top consumers (refreshed from Redis sorted set).",
	}, []string{"consumer", "user_type", "type"})

	// UsageTokensTop exposes the top-N consumers by token usage for non-LLM-proxy
	// service types, refreshed periodically from a Redis sorted set. user_type
	// is joined from the usage:consumer:{service_type}:usertype hash (the
	// rate-limit tier the consumer was last evaluated under for that service)
	// and left empty when no tier has been recorded yet. Only populated when
	// metrics.top_consumers > 0.
	UsageTokensTop = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gatewai_usage_tokens_top",
		Help: "Token usage for top consumers on non-LLM-proxy services (refreshed from Redis sorted set).",
	}, []string{"consumer", "service_type", "token_type", "user_type"})

	// UsageRequestsTop exposes the top-N consumers by request count per service
	// type (covers sync and async alike, since usage.UsageTracker.TrackRequest
	// is called on both paths), refreshed periodically from a Redis sorted set.
	// user_type is joined as for UsageTokensTop. Only populated when
	// metrics.top_consumers > 0.
	UsageRequestsTop = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gatewai_usage_requests_top",
		Help: "Request count for top consumers per service type (refreshed from Redis sorted set).",
	}, []string{"consumer", "service_type", "user_type"})

	// UsageProcessingTimeTop exposes the top-N consumers by cumulative
	// processing time (seconds) per service type, refreshed periodically from
	// a Redis sorted set. user_type is joined as for UsageTokensTop. Only
	// populated when metrics.top_consumers > 0.
	UsageProcessingTimeTop = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gatewai_usage_processing_time_top",
		Help: "Cumulative processing time (seconds) for top consumers per service type (refreshed from Redis sorted set).",
	}, []string{"consumer", "service_type", "user_type"})

	// GuardrailsPiiBlockedTotal counts requests rejected by the PII guardrail,
	// before they reach the LLM backend.
	GuardrailsPiiBlockedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_pii_blocked_total",
		Help: "Total requests blocked by the PII guardrail.",
	}, []string{"service_type", "model"})

	// GuardrailsTotal counts guardrails evaluations that matched, by stage, action and result.
	// Labels: service_type, model, stage ("input"|"output"), action, result.
	GuardrailsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_total",
		Help: "Total guardrails matches by stage, action and result (blocked|redacted|flagged).",
	}, []string{"service_type", "model", "stage", "action", "result"})

	// GuardrailsEvaluationsTotal counts every guardrails-enabled evaluation
	// (input or output stage), regardless of outcome, so GuardrailsTotal can be
	// read as a hit rate against this denominator.
	GuardrailsEvaluationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_evaluations_total",
		Help: "Total guardrails-enabled evaluations by stage, regardless of outcome.",
	}, []string{"service_type", "model", "stage"})

	// GuardrailsModelLatency observes the latency of a model-backed guardrail
	// detector call, by detector name. Buckets span the sub-second budget.
	GuardrailsModelLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_guardrails_model_latency_seconds",
		Help:    "Latency of a model-backed guardrail detector call, by detector.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1, 2, 5},
	}, []string{"detector"})

	// GuardrailsModelErrorsTotal counts model-backed guardrail detector failures,
	// by detector and reason ("timeout"|"unreachable"|"bad_response").
	GuardrailsModelErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_model_errors_total",
		Help: "Model-backed guardrail detector failures, by detector and reason.",
	}, []string{"detector", "reason"})

	// GuardrailsModelDetectionsTotal counts model-backed guardrail detections that
	// fired, by detector, mode ("sync"|"async") and result ("blocked"|"flagged").
	// Kept separate from GuardrailsTotal so the regex metric's labels are unchanged.
	GuardrailsModelDetectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_model_detections_total",
		Help: "Model-backed guardrail detections by detector, mode and result (blocked|flagged).",
	}, []string{"service_type", "model", "stage", "detector", "mode", "result"})

	// GuardrailsAsyncTotal counts result-stage guardrail detections on async job
	// results, by detector and result ("flagged"|"blocked"|"redacted"|"error").
	// The async completion path runs once per job, so this is not per-replica
	// inflated (unlike pub/sub broadcast metrics). In the shadow slice only
	// "flagged" and "error" are emitted.
	GuardrailsAsyncTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_async_total",
		Help: "Result-stage async guardrail detections by detector and result (flagged|blocked|redacted|error).",
	}, []string{"service_type", "model", "detector", "result"})

	// GuardrailsModelCacheTotal counts verdict-cache lookups by detector and
	// result ("hit"|"miss").
	GuardrailsModelCacheTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_model_cache_total",
		Help: "Model-backed guardrail verdict-cache lookups by detector and result (hit|miss).",
	}, []string{"detector", "result"})

	// GuardrailsModelSkippedTotal counts model-detector calls skipped by a guard
	// (e.g. the input-length gate), by detector and reason.
	GuardrailsModelSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_guardrails_model_skipped_total",
		Help: "Model-backed guardrail calls skipped by a guard, by detector and reason.",
	}, []string{"detector", "reason"})

	AsyncStaleJobsSweptTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_stale_jobs_swept_total",
		Help: "Total number of pending jobs marked failed and cleaned up by the stale-job GC.",
	}, []string{"model"})

	// ModelHiddenTotal counts requests that targeted a visibility-gated model the
	// caller isn't in the audience for — returned as 404 (hidden), by model.
	ModelHiddenTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_model_hidden_total",
		Help: "Requests to a visibility-restricted model rejected as 404 (not in audience).",
	}, []string{"service_type", "model"})

	RelayQueueOrphansSweptTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_relay_queue_orphans_swept_total",
		Help: "Total relay queue entries removed by the GC because their job record no longer exists in Redis.",
	}, []string{"model", "state"})
	// AsyncJobsReapedTotal counts jobs abandoned in the processing list (relay
	// pod died, lease expired) that the reaper acted on. outcome is one of
	// "requeued", "deadletter", or "dropped" (job record already gone/terminal).
	AsyncJobsReapedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_reaped_total",
		Help: "Total number of orphaned processing jobs handled by the lease reaper, by outcome.",
	}, []string{"model", "outcome"})
	// WebhookDeliveriesTotal counts terminal webhook outcomes. result is
	// "delivered" (2xx–4xx received) or "deadletter" (failed after all retries).
	WebhookDeliveriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_webhook_deliveries_total",
		Help: "Total webhook deliveries by terminal outcome (delivered|deadletter).",
	}, []string{"result"})

	// WebhookRetryQueueDepth is the number of webhooks pending retry in the
	// persistent Redis retry queue (ZSET webhook:retries).
	WebhookRetryQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gatewai_webhook_retry_queue_depth",
		Help: "Number of webhooks currently scheduled for retry in Redis.",
	})
	// IdempotencyRequestsTotal counts async submissions carrying an
	// Idempotency-Key, by outcome: "created" (new job), "replayed" (returned an
	// existing job), or "conflict" (key reused but the job is gone → 409).
	IdempotencyRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_idempotency_requests_total",
		Help: "Async submissions with an Idempotency-Key, by outcome (created|replayed|conflict).",
	}, []string{"service_type", "outcome"})

	AsyncJobsSubmittedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_submitted_total",
		Help: "Total async jobs accepted (202) by service type and model.",
	}, []string{"service_type", "model"})

	AsyncJobsCancelledTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_cancelled_total",
		Help: "Total async jobs cancelled by the client (DELETE /jobs/{type}/{id}).",
	}, []string{"service_type", "model"})

	AsyncJobsCancelledWhileProcessingTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_cancelled_while_processing_total",
		Help: "Total async jobs cancelled while the relay was processing them (GPU interrupted).",
	}, []string{"service_type", "model"})

	AsyncJobsPurgedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_async_jobs_purged_total",
		Help: "Total async jobs deleted by the admin purge endpoint.",
	}, []string{"model"})

	QuotaResetsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_quota_resets_total",
		Help: "Total per-consumer quota resets performed via the admin quota-reset endpoint.",
	}, []string{"service_type"})

	// AuthzDecisionsTotal counts authorization decisions by result and service type.
	AuthzDecisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_authz_decisions_total",
		Help: "Authorization decisions by result and service type.",
	}, []string{"service_type", "model", "decision"}) // decision = allow | deny

	// JobsTotal counts async jobs reaching a terminal outcome, by service type,
	// model and status (completed|failed). Incremented on the gateway side
	// (Manager.onComplete) rather than the relay so it stays reliable
	// regardless of relay deployment shape — an ephemeral per-event Kubernetes
	// Job may exit before Prometheus scrapes it, but the gateway is always a
	// stable, long-running scrape target.
	JobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_jobs_total",
		Help: "Total number of async jobs reaching a terminal outcome, by service type, model and status (completed|failed).",
	}, []string{"service_type", "model", "status"})

	// AuthOAuth2Duration measures OAuth2 token verification latency, by
	// operation ("jwt" = local JWKS verification, "introspection" = RFC 7662
	// call to the authorization server).
	AuthOAuth2Duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_auth_oauth2_duration_seconds",
		Help:    "OAuth2 token verification latency by operation (jwt|introspection).",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"operation"})

	// AuthOAuth2ErrorsTotal counts OAuth2 token verification failures by
	// operation and reason. reason is "invalid_token" for a rejected/malformed
	// JWT or an "inactive" introspection result, and "unreachable" for an
	// infra-level failure talking to the JWKS/introspection endpoint.
	AuthOAuth2ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_auth_oauth2_errors_total",
		Help: "OAuth2 token verification failures by operation and reason (invalid_token|unreachable).",
	}, []string{"operation", "reason"})

	// RealtimeSessionsTotal counts realtime (WebSocket) sessions by outcome:
	// "completed" (clean close), "rejected" (handshake gate: quota/concurrency),
	// "audio_cap" (max_audio_seconds tripped mid-session), or "error" (backend
	// dial or transport failure).
	RealtimeSessionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_realtime_sessions_total",
		Help: "Realtime WebSocket sessions by service_type, model and outcome (completed|rejected|audio_cap|error).",
	}, []string{"service_type", "model", "outcome"})

	// RealtimeActiveSessions is the current number of open realtime sessions.
	RealtimeActiveSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gatewai_realtime_active_sessions",
		Help: "Currently open realtime WebSocket sessions.",
	}, []string{"service_type", "model"})

	// RealtimeAudioSecondsTotal counts seconds of audio streamed through realtime
	// sessions (the quota-metered unit), derived from the client's PCM byte rate.
	RealtimeAudioSecondsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gatewai_realtime_audio_seconds_total",
		Help: "Seconds of audio streamed through realtime sessions.",
	}, []string{"service_type", "model", "user_type"})

	// RealtimeSessionDuration measures realtime session wall-clock lifetime.
	RealtimeSessionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gatewai_realtime_session_duration_seconds",
		Help:    "Realtime WebSocket session duration in seconds.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"service_type", "model"})
)
