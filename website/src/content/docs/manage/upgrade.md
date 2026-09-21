---
title: Upgrade guide
---

# Upgrade guide

## Breaking changes

Check this table before upgrading across multiple versions — each release's own breaking changes (if any) are also documented in [CHANGELOG.md](https://github.com/Qatr-io/GatewAI/blob/main/CHANGELOG.md).

### Already released

| Component | Version | Change |
|---|---|---|
| Gateway | v0.23.0 | `GET /v1/models`'s `backend_model` now reflects only the service-level `backend_model` — a per-backend override is no longer surfaced there (it still rewrites the outgoing request to that backend). The `backend_models[]` array (added in v0.21.0) was removed. |
| Gateway | v0.20.0 | Deploy the new gateway image **before** the new relay image — the relay's `POST /-/relay/jobs/{id}/complete` call is a no-op against an older gateway until it's upgraded. |
| Gateway | v0.17.0 | `guardrails.pii` boolean removed — use `guardrails.checks` (+ optional `guardrails.action`) instead. |
| Gateway | v0.15.0 | Go module and Helm chart renamed: `kevent/gateway` → `gatewai/gateway`, `kevent-gateway` → `gatewai-gateway`. |
| Gateway | v0.14.0 | Kafka removed entirely — jobs are pushed directly to the relay's Redis queue. `sync_topic` and the `syncPriority` mechanism were removed the same week (v0.13.0). |
| Gateway | v0.11.0 | `redis.pending_max_age_hours` (int, hours) renamed to `redis.pending_max_age` (duration string, e.g. `"2h"`); `lifecycle.job_ttl.success` renamed to `lifecycle.job_ttl.completed`. |
| Gateway | v0.8.0 | `GatewAI_llm_*` metrics gained a `backend_model` label — update PromQL `by`/`without` clauses and dashboards. |
| Gateway | v0.7.0 | `api_key` removed from the service config — use `inference_headers` (`Authorization: Bearer …`) instead. |
| Relay | v0.7.0 | Go module renamed: `kevent/relay` → `gatewai/relay`. |
| Relay | v0.6.1 | `PodAnnotator` / pod-deletion-cost mechanism removed — no longer needed since the relay exits after one job. |
| Helm chart | 0.20.1 | All shipped `PrometheusRule` alerts renamed to drop the legacy `Kevent` prefix (e.g. `KeventGatewayHighErrorRate` → `GatewayHighErrorRate`) — update any Alertmanager routes/silences matching the old names. |

### Upcoming

- **`services[].backends` removal**: the inline per-service `backends:` list is deprecated as of v0.23.0 in favor of the top-level [`backend_pools`](../configure/service-registry.md#backend-pools-backend_pools) block + `services[].backend_pool`. It's still fully supported today, but will be removed in a future release — migrate new and existing services to `backend_pools`/`backend_pool` now to avoid a forced change later.

## Gateway

```bash
# 1. Pull the latest chart
helm repo update

# 2. Review breaking changes
# — the table above, and https://github.com/Qatr-io/GatewAI/blob/main/CHANGELOG.md

# 3. Upgrade
helm upgrade gatewai-gateway GatewAI/gatewai-gateway -f values.yaml

# 4. Verify rollout
kubectl rollout status deployment/gatewai-gateway
kubectl logs -l app=gatewai-gateway --tail=50
```

## Relay

The relay image tag is set directly on the inference Deployment, not managed by the Helm chart.

```bash
# Update the relay container image tag in your Deployment manifest
# then apply:
kubectl set image deployment/whisper-large-v3 \
  relay=ghcr.io/qatr-io/gatewai/relay:<new-tag>

kubectl rollout status deployment/whisper-large-v3
```

Drain the queue before upgrading the relay if the new version changes the job payload format.

## Config changes

Config changes that don't require a restart can be applied via hot reload:

```bash
# Update the ConfigMap
kubectl edit configmap gatewai-gateway

# Trigger reload (if configmap-reload sidecar is not enabled)
kubectl exec deploy/gatewai-gateway -- \
  curl -s -X POST http://localhost:8080/-/reload
```

Enable `configReloader` in `values.yaml` to trigger reloads automatically on ConfigMap changes:

```yaml
configReloader:
  enabled: true
```

## Rollback

```bash
# Gateway
helm rollback gatewai-gateway

# Relay
kubectl rollout undo deployment/whisper-large-v3
```
