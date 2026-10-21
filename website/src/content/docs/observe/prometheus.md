---
title: Prometheus
---

# Prometheus

The gateway exposes a standard Prometheus metrics endpoint. The relay has no HTTP server and does not expose `/metrics` — its metrics are collected in-process and only relevant if you run a Prometheus push gateway or scrape the pod directly via annotations.

## Scrape endpoint

```
GET /metrics
```

Returns metrics in Prometheus text exposition format. No authentication required (secure at the network level).

## Enable with Helm

### ServiceMonitor

Requires [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator) in your cluster.

```yaml
metrics:
  serviceMonitor:
    enabled: true
    interval: 30s
    scrapeTimeout: 10s
    # Must match your Prometheus Operator's serviceMonitorSelector labels.
    # Example for kube-prometheus-stack:
    additionalLabels:
      release: kube-prometheus-stack
```

The ServiceMonitor is created in the same namespace as the Helm release by default. Override with `metrics.serviceMonitor.namespace` if your Prometheus Operator watches a different namespace.

### PrometheusRule

```yaml
alerting:
  prometheusRule:
    enabled: true
    # Must match your Prometheus Operator's ruleSelector labels.
    additionalLabels:
      release: kube-prometheus-stack
    # Configurable thresholds (defaults shown):
    thresholds:
      gatewayErrorRateWarning: 0.05    # 5%
      gatewayErrorRateCritical: 0.20   # 20%
      syncJobsInFlight: 20
      relayJobFailureRateWarning: 0.10  # 10%
      relayJobFailureRateCritical: 0.30 # 30%
      relayInferenceP95Seconds: 120
      rateLimitRejectionRateWarning: 0.20 # 20%
      jobsPendingFor: 15m
      jobsRunningFor: 1h
```

Add custom rules via `alerting.prometheusRule.extraRules` (appended to the `gatewai-relay` group).

## Consumer metrics

Two options for tracking per-consumer usage (mutually exclusive):

| Option | Helm key | When to use |
|--------|----------|-------------|
| Top-N gauge (Redis sorted set, refresh 60s) | `metricsConfig.topConsumers: 20` | Dynamic/large consumer pools |
| Direct labels on `gatewai_jobs_by_consumer_total` | `metricsConfig.consumerLabels: true` | ≤ 50 known consumers |

Avoid `consumerLabels: true` with large or dynamic consumer sets — it creates unbounded label cardinality.

`metricsConfig.topConsumers` populates four gauges, refreshed from Redis every 60s (see [metrics reference](/reference/metrics)):

- `gatewai_llm_consumer_tokens_top` — LLM-proxy token usage
- `gatewai_usage_tokens_top` — token usage on non-LLM-proxy service types
- `gatewai_usage_requests_top` — request count per service type, **sync and async alike** (the only one covering sync-only consumers — `gatewai_jobs_by_consumer_total` only counts async job submissions)
- `gatewai_usage_processing_time_top` — cumulative processing time per service type, useful to surface the heaviest (not just the most frequent) consumers

All four are visualized in the "Gateway — Consumers" row of the `gatewai.json` dashboard (LLM token panels are also mirrored in `gatewai-llm.json` / `gatewai-audit-trail.json`).

## Dashboards

Four Grafana dashboards are available in [`dashboards/`](https://github.com/IA-Generative/GatewAI/tree/main/dashboards/):

| Dashboard | File | Datasource |
|-----------|------|------------|
| GatewAI — Gateway & Relay | `gatewai.json` | Prometheus + Tempo |
| GatewAI — LLM Proxy | `gatewai-llm.json` | Prometheus + Tempo |
| GatewAI — Audit Trail LLM | `gatewai-audit-trail.json` | Prometheus + Loki |
| GatewAI — Traces | `gatewai-traces.json` | Tempo |

Import via **Dashboards → Import → Upload JSON file**. The Prometheus dashboards include latency panels with [exemplar](/observe/opentelemetry#grafana--tempo-exemplars) support — clicking an exemplar dot jumps to the corresponding trace in Tempo.

For traces, OTLP metrics push (relay), and log forwarding see [OpenTelemetry](/observe/opentelemetry).

## Alerts

All alerts are defined in `helm/gateway/templates/prometheusrule.yaml`. Thresholds are configurable via `alerting.prometheusRule.thresholds`.

### Gateway

| Alert | Severity | Description | Threshold key |
|-------|----------|-------------|---------------|
| `GatewayHighErrorRate` | warning | > X% of requests return 5xx over 5 min | `gatewayErrorRateWarning` |
| `GatewayHighErrorRate` | critical | > X% of requests return 5xx over 5 min | `gatewayErrorRateCritical` |
| `GatewayS3Errors` | warning | Any S3 errors in the last 5 min | — |
| `GatewaySyncJobsInFlightHigh` | warning | Sync connections above threshold for 5 min | `syncJobsInFlight` |
| `GatewayRateLimitHighRejectionRate` | warning | > X% of requests rate-limited per service type | `rateLimitRejectionRateWarning` |
| `GatewayRateLimitErrors` | warning | Redis errors in rate limiter (fail-open) | — |

### Relay

| Alert | Severity | Description | Threshold key |
|-------|----------|-------------|---------------|
| `RelayJobFailureRate` | warning | > X% of relay jobs failing per service type | `relayJobFailureRateWarning` |
| `RelayJobFailureRate` | critical | > X% of relay jobs failing per service type | `relayJobFailureRateCritical` |
| `RelayInferenceSlow` | warning | p95 inference latency above threshold for 10 min | `relayInferenceP95Seconds` |
| `RelayS3Errors` | warning | Any relay S3 errors in the last 5 min | — |
| `RelayJobsPendingTooLong` | warning | Relay `pending` queue non-empty AND zero jobs completed for that model, for the threshold duration | `jobsPendingFor` |
| `RelayJobsRunningTooLong` | warning | Relay `processing` queue non-empty AND zero jobs completed for that model, for the threshold duration | `jobsRunningFor` |

Both alerts also require zero completions (`gatewai_jobs_total`) in the same window — queue depth alone can't tell a genuine stall apart from healthy load (no job-level ID/age is exposed), so they only fire when nothing at all is finishing for that model:
- `RelayJobsRunningTooLong` catches a stuck in-flight job (or crashed relay) rather than firing on any busy model with concurrent jobs.
- `RelayJobsPendingTooLong` catches the relay not picking up jobs at all (down / stuck starting) rather than firing on a relay that's merely overloaded (arrival rate > capacity) but still actively consuming and completing jobs — that's a capacity-pressure situation, worth watching separately via `gatewai_relay_queue_depth{state="pending"}` if desired, but distinct from a starting problem.
