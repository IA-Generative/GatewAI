---
title: OpenTelemetry
---

# OpenTelemetry

GatewAI ships built-in OpenTelemetry instrumentation for distributed tracing, OTLP metrics push, and structured log forwarding. All signals are exported via OTLP/HTTP to an [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) or any OTLP-compatible backend.

## How it works

```
Gateway                        Relay
  │                              │
  ├─ Traces (OTLP/HTTP)          ├─ Traces (OTLP/HTTP)
  ├─ Metrics (OTLP push)         ├─ Metrics (OTLP push, useful for k8s Jobs)
  └─ Logs (slog bridge)          └─ Logs (slog bridge)
         │                              │
         └──────────────────────────────┘
                       │
               OTel Collector
                       │
           ┌───────────┼───────────┐
          Tempo      Prometheus  Loki
```

The relay is a one-shot Kubernetes Job (one pod = one inference job). It cannot be Prometheus-scraped, making **OTLP metrics push** particularly valuable for relay-side observability.

### Trace propagation across processes

Traces span the entire request lifecycle — from client HTTP call through gateway, Redis queue, relay, and into the inference API:

```
Client → Gateway (submit span)
           │
           │  W3C traceparent stored in job.trace_context (Redis)
           ▼
         Relay (process_job span — child of gateway span)
           │
           │  W3C traceparent injected as HTTP header
           ▼
        Inference API (can create child spans if OTel-instrumented)
           │
           ▼
         Gateway consumer (webhook.send span — linked back to original trace)
```

The `traceparent` is carried in the Redis job record (`trace_context` field), not an HTTP header, since communication between gateway and relay is asynchronous.

## Quick start

Enable the `opentelemetry` block in both gateway and relay configs and point it at an OTel Collector:

```yaml
# config.yaml (gateway) or relay/config.yaml (relay)
opentelemetry:
  enabled: true
  exporter:
    endpoint: "http://otel-collector:4318"   # OTLP/HTTP receiver
  traces:
    enabled: true
```

## Configuration

All settings are under the `opentelemetry:` key. The relay uses the same structure.

```yaml
opentelemetry:
  enabled: false            # master switch; false = no-op providers, zero overhead

  exporter:
    endpoint: "http://otel-collector:4318"  # base OTLP/HTTP endpoint for all signals
    insecure: false                          # skip TLS verification (dev only)
    headers:                                 # static headers sent with every export
      Authorization: "Bearer ${OTEL_TOKEN}"

  traces:
    enabled: true
    endpoint: ""            # override exporter.endpoint for traces only (optional)
    headers: {}             # merged with exporter.headers
    sample_ratio: 1.0       # 1.0 = always sample; 0.1 = 10% TraceID-ratio sampling
    ignore_paths:           # path prefixes excluded from tracing (prefix-based)
      - /health             # default list when unset
      - /metrics
      - /docs
      - /openapi.yaml

  # OTLP metrics push — additive to Prometheus scraping.
  # Required for the relay (k8s Job pods cannot be scraped).
  metrics:
    enabled: false
    endpoint: ""            # override exporter.endpoint for metrics only (optional)
    headers: {}
    interval: "60s"         # export interval; default 60s

  # Forward structured slog logs via OTLP.
  # Replaces slog.Default() with the OTel slog bridge when enabled.
  logs:
    enabled: false
    endpoint: ""            # override exporter.endpoint for logs only (optional)
    headers: {}
```

### Per-signal endpoint override

Each signal can point to a different endpoint. This is useful when traces, metrics, and logs go to separate collectors or backends:

```yaml
opentelemetry:
  enabled: true
  exporter:
    endpoint: "https://ingest.example.com:4318"
    headers:
      Authorization: "Bearer ${INGEST_TOKEN}"
  traces:
    enabled: true
    # uses exporter.endpoint and exporter.headers
  metrics:
    enabled: true
    endpoint: "https://metrics.example.com:4318"   # different endpoint
    headers:
      X-Scope-OrgID: "my-tenant"                   # merged with exporter.headers
```

### Sampling

| `sample_ratio` | Behaviour |
|---|---|
| `1.0` (default) | All traces sampled (AlwaysSample) |
| `0.0 < ratio < 1.0` | TraceID-ratio-based sampling |
| `0.0` | AlwaysSample (same as 1.0) |

For production with high request volumes, start with `sample_ratio: 0.1` (10%) and adjust based on storage cost.

### Ignoring paths

`ignore_paths` lists path prefixes that are excluded from tracing. A request is skipped when its path equals an entry or starts with `entry + "/"`.

Default (applied when `ignore_paths` is absent from config):

```
/health  /metrics  /docs  /openapi.yaml
```

Override to add or remove entries — the default list is replaced entirely when you set this key:

```yaml
opentelemetry:
  traces:
    enabled: true
    ignore_paths:
      - /health
      - /metrics
      - /docs
      - /openapi.yaml
      - /internal    # add any extra prefix
```

:::tip
The match is prefix-based: listing `/docs` skips `/docs`, `/docs/spec/audio/whisper`, etc.
:::

## Helm

```yaml
opentelemetry:
  enabled: true
  exporter:
    endpoint: "http://{{ .Release.Name }}-opentelemetry-collector:4318"
    insecure: true
  traces:
    enabled: true
    sampleRatio: 1.0
    # ignorePaths: []   # overrides the default [/health, /metrics, /docs, /openapi.yaml]
  metrics:
    enabled: false
  logs:
    enabled: false
```

### Optional OTel Collector sub-chart

The Helm chart includes an optional `opentelemetry-collector` sub-chart dependency. Enable it to deploy a collector alongside the gateway:

```yaml
opentelemetry-collector:
  enabled: true
  mode: deployment
  config:
    receivers:
      otlp:
        protocols:
          http:
            endpoint: "0.0.0.0:4318"
    exporters:
      otlp/tempo:
        endpoint: "http://tempo:4317"
      prometheusremotewrite:
        endpoint: "http://prometheus:9090/api/v1/write"
    service:
      pipelines:
        traces:
          receivers: [otlp]
          exporters: [otlp/tempo]
        metrics:
          receivers: [otlp]
          exporters: [prometheusremotewrite]
```

Add the Helm repo before installing:

```bash
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
helm repo update
```

## Backends

GatewAI exports standard OTLP — any OTLP-compatible backend works. Common setups:

### Grafana stack (Tempo + Mimir + Loki)

```yaml
# OTel Collector config
exporters:
  otlp/tempo:
    endpoint: "http://tempo:4317"
  prometheusremotewrite:
    endpoint: "http://mimir:9090/api/v1/write"
    headers:
      X-Scope-OrgID: "gatewai"
  loki:
    endpoint: "http://loki:3100/loki/api/v1/push"

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp/tempo]
    metrics:
      receivers: [otlp]
      exporters: [prometheusremotewrite]
    logs:
      receivers: [otlp]
      exporters: [loki]
```

### Jaeger

```yaml
exporters:
  otlp/jaeger:
    endpoint: "http://jaeger-collector:4317"
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp/jaeger]
```

### Datadog

```yaml
exporters:
  datadog:
    api:
      site: "datadoghq.com"
      key: "${DD_API_KEY}"
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [datadog]
    metrics:
      receivers: [otlp]
      exporters: [datadog]
```

## Grafana + Tempo: exemplars

When traces are enabled, Prometheus histogram panels link directly to individual traces via **exemplars**. This lets you click a latency spike and jump straight to the trace that caused it.

### Configure the Prometheus datasource

In Grafana, add an exemplar trace ID destination to your Prometheus datasource:

```yaml
# Grafana datasource provisioning (grafana/provisioning/datasources/prometheus.yaml)
datasources:
  - name: Prometheus
    type: prometheus
    url: http://prometheus:9090
    jsonData:
      exemplarTraceIdDestinations:
        - name: traceID
          datasourceUid: tempo   # must match your Tempo datasource UID
```

Once configured, exemplar dots appear on latency panels. Clicking a dot opens the trace in Tempo Explore.

### Grafana dashboards

Four dashboards are available in [`dashboards/`](https://github.com/IA-Generative/GatewAI/tree/main/dashboards/):

| Dashboard | UID | Datasource |
|-----------|-----|------------|
| GatewAI — Gateway & Relay | `gatewai-gw-relay-v1` | Prometheus + Tempo |
| GatewAI — LLM Proxy | `gatewai-llm` | Prometheus + Tempo |
| GatewAI — Audit Trail LLM | `gatewai-audit-trail` | Prometheus + Loki |
| **GatewAI — Traces** | `gatewai-traces-v1` | **Tempo** |

Import via **Dashboards → Import → Upload JSON file**. The Traces dashboard requires a Tempo datasource variable named `datasource_tempo`.

#### Service map

The `gatewai-traces-v1` dashboard includes a service map panel that visualises the gateway → relay → inference topology. This requires the **Tempo Metrics Generator** to be enabled:

```yaml
# Tempo config
metrics_generator:
  registry:
    external_labels:
      source: tempo
  storage:
    path: /tmp/tempo/generator/wal
  processor:
    service_graphs:
      wait: 10s
    span_metrics: {}
```

Enable the processors in the Tempo config:

```yaml
overrides:
  defaults:
    metrics_generator:
      processors: [service-graphs, span-metrics]
```

## Relay: one-shot jobs and OTLP push

The relay runs as a Kubernetes Job — one pod processes one inference job and exits. Prometheus cannot scrape it reliably. OTLP metrics push ensures relay metrics reach your observability backend even when the pod terminates before a scrape window.

Enable in `relay/config.yaml`:

```yaml
opentelemetry:
  enabled: true
  exporter:
    endpoint: "http://otel-collector.monitoring:4318"
  traces:
    enabled: true
  metrics:
    enabled: true     # push instead of scrape
    interval: "30s"   # flush before the pod exits (~30s before job completion)
```

The relay defers OTel shutdown with a 10-second timeout to force-flush all buffered spans and metrics before the process exits.
