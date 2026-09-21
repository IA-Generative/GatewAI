---
title: Helm chart
---

# Helm chart

The gateway is packaged as a Helm chart and published to GitHub Pages.

## Install

```bash
helm repo add GatewAI https://ia-generative.github.io/GatewAI
helm repo update

helm install gatewai-gateway GatewAI/gatewai-gateway -f values.yaml
```

## Upgrade

```bash
helm upgrade gatewai-gateway GatewAI/gatewai-gateway -f values.yaml
```

## Install from local sources

```bash
helm upgrade --install gatewai-gateway ./helm/gateway -f values.yaml
```

## Minimal `values.yaml`

```yaml
image:
  tag: "v0.14.0"

config:
  s3:
    endpoint: "https://s3.fr-par.scw.cloud"
    region: "fr-par"
    bucket: "my-GatewAI-jobs"
  redis:
    addr: "redis:6379"

secrets:
  s3AccessKey: "my-access-key"
  s3SecretKey: "my-secret-key"
```

## Redis HA

The chart deploys Redis with Redis-HA (HAProxy front-end) by default. Redis is required for job state, rate limiting, and result caching.

**Persistence.** All in-flight async state lives in Redis — job records, the relay `pending`/`processing` queues, per-job leases, and the webhook retry queue. The chart enables **AOF** (`appendfsync everysec`, ≤1s loss window) + **RDB** snapshots on a PersistentVolume so this survives a Redis pod restart or master failover:

```yaml
redis-ha:
  redis:
    config:
      appendonly: "yes"
      appendfsync: "everysec"
      save: "900 1 300 10 60 10000"
  persistentVolume:
    enabled: true
    size: 10Gi
```

Disable only for ephemeral/dev clusters. On an existing cluster, turning this on adds a PVC per Redis replica — roll out during a maintenance window.

## Async hardening values

The reaper, durable webhooks, and idempotency features expose these chart values (rendered into `config.yaml`):

```yaml
lifecycle:
  gc:
    maxReapAttempts: 3        # requeues before dead-lettering a lease-expired job

webhooks:
  maxRetries: 3               # webhook attempts before dead-letter
  retryBackoff: "30s"
  maxBackoff: "10m"
  signingSecret: ""           # HMAC secret; prefer "${WEBHOOK_SIGNING_SECRET}" via extraEnvVars

jobs:
  idempotencyTtl: "24h"       # Idempotency-Key → job mapping retention
  expiredMarkerTtl: "168h"    # "job existed" tombstone (410 vs 404 on poll)
```

The relay's lease TTL is set in the relay config, not the gateway chart: `lease_ttl` (default `60s`) — see [Async processing](../relay/async).

## Backend pools

A named, load-balancer-style group of member backends referenced from `services[]` by name via `backendPool: <name>` — instead of every service declaring its own `backends`/`inferenceURL`. Lets several service/model aliases share one physical backend's auth headers, rate limit, and concurrency budget:

```yaml
config:
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

`backendPool` is mutually exclusive with `backends`/`inferenceURL` on the same service. See [Service registry](../configure/service-registry.md#backend-pools-backend_pools) for the full field reference and the [Helm chart README](https://github.com/Qatr-io/GatewAI/blob/main/helm/gateway/README.md) for the complete `values.yaml` schema.

## ConfigMap hot reload

The chart supports `configmap-reload` sidecar to trigger `POST /-/reload` automatically when the ConfigMap changes:

```yaml
configReloader:
  enabled: true
  image: ghcr.io/jimmidyson/configmap-reload:v0.14.0
```

## Ingress

```yaml
ingress:
  enabled: true
  className: nginx
  host: GatewAI.example.com
  tls:
    enabled: true
    secretName: GatewAI-tls
```

## Extra environment variables

```yaml
extraEnvVars:
  - name: ENCRYPTION_KEY
    valueFrom:
      secretKeyRef:
        name: GatewAI-encryption
        key: key
```

## Deploy relay (Deployment + KEDA)

The relay runs as a standalone Deployment alongside the inference container. Each pod processes one job and exits — KEDA creates a new pod when the Redis queue is non-empty.

Example manifests for a Whisper transcription service are in [`examples/relay-transcription/`](https://github.com/Qatr-io/GatewAI/tree/main/examples/relay-transcription/). Copy and adapt them to your cluster, then apply:

```bash
# ConfigMap (relay config.yaml)
kubectl apply -f examples/relay-transcription/relay-cm.yaml

# Deployment (2 containers: inference model + relay)
kubectl apply -f examples/relay-transcription/deployment.yaml

# KEDA ScaledObject (scales on Redis list length)
kubectl apply -f examples/relay-transcription/keda-scaledobject.yaml
```

The KEDA ScaledObject targets `relay:<model>:pending` list length with `listLength: 1` and `minReplicaCount: 0` — pods scale to zero when the queue is empty. Replace all `<placeholder>` values (namespace, Redis address, secret names, image tags) before applying.
