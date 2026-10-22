---
title: Runbooks
---

## GatewayHighErrorRate

**Severity:** warning / critical — **Component:** Gateway

More than 5% (warning) or 20% (critical) of gateway requests return HTTP 5xx errors over the last 5 minutes.

**Impact:** Clients receive errors. Async job submissions or sync inference calls are failing.

**Diagnosis**

```bash
# Error rate by mode and service_type
rate(gatewai_requests_total{status=~"5.."}[5m])

# Gateway pod logs
kubectl logs -l app.kubernetes.io/name=gatewai-gateway -n <namespace> --tail=100

# Status code breakdown
sum by (status, mode, service_type) (rate(gatewai_requests_total[5m]))
```

Common causes: S3 unavailable → check `gatewai_s3_errors_total`. Redis unavailable → check `gatewai_redis_errors_total`. Upstream inference service returning errors (sync mode).

**Mitigation**

1. Check S3 and Redis connectivity from the gateway pod
2. S3 down: async jobs fail at upload — check bucket and credentials
3. Redis down: async submissions fail at enqueue — check Redis HA HAProxy endpoint
4. Scale the gateway deployment if the issue is resource exhaustion

---

## GatewayRateLimitErrors

**Severity:** warning — **Component:** Gateway

The gateway is failing to execute rate-limit checks against Redis. The rate limiter is **fail-open**: Redis errors allow the request through, so consumers are not blocked — but rate limiting is not enforced.

**Impact:** Rate limiting silently disabled for the affected service type. Consumers can exceed their configured limits.

**Diagnosis**

```promql
# Error rate per service type
rate(gatewai_ratelimit_errors_total[5m])
```

```bash
kubectl logs -l app.kubernetes.io/name=gatewai-gateway -n <namespace> --tail=100 | grep "rate limit"
```

Common causes: Redis unavailable or restarting; connection pool exhausted under high load; network partition between gateway pods and Redis.

**Mitigation**

1. Check Redis HA status:
   ```bash
   kubectl get pods -l app=redis-ha -n <namespace>
   kubectl exec -it <haproxy-pod> -- redis-cli -h localhost -p 6380 PING
   ```
2. If Redis is down, other features are also affected (job records, cache, consumer tracking) — follow the Redis recovery procedure
3. Once Redis recovers, the rate limiter resumes automatically (no restart needed)

---

## GatewayRateLimitHighRejectionRate

**Severity:** warning — **Component:** Gateway

More than 20% of rate-limit checks return `rejected` over the last 5 minutes.

**Impact:** Affected consumers receive `429 Too Many Requests` with `Retry-After`. Legitimate traffic may be disrupted if limits are set too low.

**Diagnosis**

```promql
# Rejection rate per service type
sum by (service_type) (rate(gatewai_ratelimit_requests_total{result="rejected"}[5m]))
/ sum by (service_type) (rate(gatewai_ratelimit_requests_total[5m]))

# Which user types are hitting limits?
sum by (service_type, user_type) (rate(gatewai_ratelimit_consumer_hits_total[5m]))
```

Common causes: traffic spike from a legitimate consumer; limits configured too low; aggressive client retries; abuse.

**Mitigation**

1. Identify offending consumers via Redis:
   ```bash
   redis-cli ZREVRANGEBYSCORE llm:consumer:tokens:sa:prompt +inf -inf WITHSCORES LIMIT 0 10
   ```
2. Compare current rates against configured limits in `config.yaml` under `rate_limits`
3. Temporarily raise limits if legitimate consumers are disrupted — update `config.yaml` and `POST /-/reload`
4. If abuse detected — disable the consumer at the APISIX level (revoke API key)

---

## GatewayS3Errors

**Severity:** warning — **Component:** Gateway

The gateway encountered S3 errors (upload, get, or delete) in the last 5 minutes.

**Impact:** Upload errors → async job submissions fail (500 to client). Get errors → completed job results cannot be fetched. Delete errors → orphaned files accumulate (no functional impact, storage cost).

**Diagnosis**

```bash
# Errors by operation
sum by (operation) (increase(gatewai_s3_errors_total[5m]))

# Gateway logs
kubectl logs -l app.kubernetes.io/name=gatewai-gateway -n <namespace> | grep -E "s3 (upload|result fetch) failed"

# Check S3 reachability
kubectl exec -it <gateway-pod> -n <namespace> -- curl -I <s3-endpoint>
```

Common causes: expired or incorrect credentials; bucket missing or wrong region; network issue to S3 endpoint; S3 service outage.

**Mitigation**

1. Verify S3 credentials in the secret (`S3_ACCESS_KEY`, `S3_SECRET_KEY`)
2. Check endpoint and region in gateway config
3. Confirm bucket exists and credentials have read/write access
4. If encryption is enabled, verify `ENCRYPTION_KEY` matches between gateway and relay

---

## RelayInferenceSlow

**Severity:** warning — **Component:** Relay sidecar

The 95th percentile inference duration exceeds the configured threshold (default: 120 s).

**Impact:** Async jobs complete later than expected. KEDA may create additional pods if the queue keeps growing.

**Diagnosis**

```promql
# p95 inference duration by service_type
histogram_quantile(0.95,
  sum by (service_type, le) (rate(gatewai_relay_inference_duration_seconds_bucket[10m]))
)

# Input file size distribution
histogram_quantile(0.95,
  sum by (service_type, le) (rate(gatewai_relay_input_size_bytes_bucket[10m]))
)
```

Common causes: large input files; GPU contention; model cold start after scale-from-zero; inference model degraded (OOM, throttling).

**Mitigation**

1. Large inputs → expected behaviour; consider raising the alerting threshold or use `max_file_size_mb`
2. Cold start → increase `queue_pop_timeout` in relay config
3. Check model pod resource limits — GPU memory pressure can slow inference significantly

---

## RelayJobFailureRate

**Severity:** warning / critical — **Component:** Relay sidecar

More than 10% (warning) or 30% (critical) of jobs processed by relay sidecars are failing.

**Impact:** Async jobs complete with status `failed`. Clients are notified via webhook or see `status: failed` when polling.

**Diagnosis**

```bash
# Failure rate by service_type
sum by (service_type) (rate(gatewai_relay_jobs_total{status="failed"}[5m]))
  / sum by (service_type) (rate(gatewai_relay_jobs_total[5m]))

# Relay pod logs
kubectl logs -l serving.kserve.io/inferenceservice=<name> -c kserve-container -n <namespace> --tail=100

# S3 download errors
increase(gatewai_relay_s3_errors_total[5m])
```

Common causes: inference model returning non-2xx; S3 download failure; inference timeout; GPU OOM causing pod restart.

**Mitigation**

1. Check relay logs for specific error (`inference error`, `s3 download failed`)
2. If model is crashing: check GPU memory usage, reduce `containerConcurrency`
3. If timeouts: increase `inference.timeout` in relay config
4. If S3 errors: check credentials and `ENCRYPTION_KEY` consistency between gateway and relay
5. Resubmit failed jobs via `POST /jobs/{service_type}` (results cleaned up after TTL, 72h default)

---

## RelayS3Errors

**Severity:** warning — **Component:** Relay sidecar

Relay sidecars encountered S3 errors (get/put/delete) in the last 5 minutes.

**Impact:** Get errors → relay cannot download input file → job marked `failed`. Put errors → inference result cannot be uploaded → job marked `failed`, result lost. Delete errors → input file not cleaned up (storage cost, no functional impact).

**Diagnosis**

```bash
# Errors by operation
sum by (operation) (increase(gatewai_relay_s3_errors_total[5m]))

# Relay logs
kubectl logs -l serving.kserve.io/inferenceservice=<name> -c kserve-container -n <namespace> | grep -E "s3|S3"

# Check if gateway S3 errors also firing (shared credentials issue)
increase(gatewai_s3_errors_total[5m])
```

Common causes: S3 credentials not injected into relay pod; `ENCRYPTION_KEY` mismatch between gateway and relay; input file deleted before relay could download it; S3 endpoint unreachable from inference pod's network namespace.

**Mitigation**

1. Verify `S3_ACCESS_KEY` and `S3_SECRET_KEY` env vars in the KServe manifest
2. Confirm `ENCRYPTION_KEY` is the same secret in both gateway and relay deployments
3. Check network policies — inference pods must reach the S3 endpoint
4. If input files disappear prematurely: check `redis.pending_max_age` and `lifecycle.job_ttl.pending` vs inference duration

## Dead-letter queues

Two Redis lists collect items that could not be delivered or recovered automatically. They are **not** self-draining — inspect and act on them.

**`webhook:deadletter`** — job-completion webhooks that failed every retry (consumer endpoint down or returning `5xx`). Watch `increase(gatewai_webhook_deliveries_total{result="deadletter"}[1h])` and `gatewai_webhook_retry_queue_depth` (a growing depth means consumers are failing).

```bash
redis-cli LLEN webhook:deadletter
redis-cli LRANGE webhook:deadletter 0 9    # {job_id, service_type, url, attempts, failed_at}
```

The job result is still in S3/Redis (subject to TTL) — clients can poll `GET /jobs/{type}/{id}`. Once the consumer is healthy, re-notify from your own tooling, or `LTRIM`/`DEL` the list after handling.

**`relay:<model>:deadletter`** — async jobs a dead relay pod abandoned that the phase-0 reaper requeued `gc.max_reap_attempts` times without success (persistently crashing on that job). The job is marked `failed`. Watch `increase(gatewai_async_jobs_reaped_total{outcome="deadletter"}[1h])`.

```bash
redis-cli LLEN relay:whisper-large-v3:deadletter
redis-cli LRANGE relay:whisper-large-v3:deadletter 0 -1
```

A non-zero count usually means the job input reliably kills the relay (bad payload, OOM). Investigate the relay logs for those IDs before re-submitting; raise `gc.max_reap_attempts` only if the failures were transient (e.g. node churn), not deterministic.
