---
title: Priority routing
---

# Priority routing

Priority routing lets SA (service account) consumers bypass the normal async queue, ensuring their jobs are picked up first by the next available relay pod.

## How it works

The relay's `BLMOVE` always pops from the **left** (head) of `relay:<model>:pending`. The gateway normally appends jobs to the **right** (`RPUSH`). For priority jobs, it appends to the **left** (`LPUSH`) instead — so they land at the head of the queue and are picked up before any normal job already waiting.

```
Normal async:   RPUSH relay:<model>:pending  → job appended at tail
Priority async: LPUSH relay:<model>:pending  → job inserted at head
```

Because the relay pops from the left (`BLMOVE LEFT RIGHT`), priority jobs are always picked up next regardless of how many normal jobs are already in the queue.

## Configuration

```yaml
server:
  priority_header: "X-Priority"   # header name to check on incoming requests
```

When the header is present in a `POST /jobs/{service_type}` request, the job is marked `priority: true` and inserted at the head of the queue. Leave the field empty to disable priority routing.

## Sync requests

`server.priority_header` also gates access to a **reserved sync concurrency pool** via `priority_reserved_sync` (per service, see [Service registry](service-registry.md#reserving-capacity-for-priority-requests-priority_reserved_sync)). This is a distinct mechanism from the async queue-jump above — sync requests have no queue to jump, so priority instead reserves part of the model's `max_concurrent_sync` capacity exclusively for requests carrying the header, rather than reordering anything. Both mechanisms are triggered by the same header, so a single upstream signal (e.g. an APISIX plugin setting `X-Priority` based on the caller's role) drives priority behavior across both the async and sync paths.

## Single-deployment model

Unlike the previous Kafka-based approach (which required a dedicated relay Deployment consuming a separate topic), the Redis LPUSH mechanism works within the **same relay Deployment**. No second relay is needed — the head-of-queue position is sufficient to ensure priority pick-up.

## Consumer identification and isolation

When `server.consumer_header` is set (e.g. `X-Consumer-Username`, injected by APISIX after auth), the gateway:

1. Stores `consumer_name` in the job record (Redis JSON)
2. Maintains `consumer:{name}:jobs` sorted set (score = Unix timestamp, same TTL as job)
3. Exposes `GET /jobs` to list a consumer's jobs (paginated, most-recent-first)
4. Enforces ownership on `GET /jobs/{service_type}/{id}`: if the header is present, the job's `consumer_name` must match — returns `404` on mismatch
5. Increments `gatewai_jobs_by_consumer_total{mode, service_type, model, consumer}` with `mode=async-priority` for priority jobs

```yaml
server:
  consumer_header: "X-Consumer-Username"   # set by APISIX after authentication
```

### Ownership check behaviour

| `consumer_header` configured | Header in request | Result |
|---|---|---|
| no | — | No check — auth-less deployments, all callers trusted |
| yes | absent | No check — admin/internal calls bypass isolation |
| yes | present + matches job | `200 OK` |
| yes | present + mismatch | `404` — no information leak about other consumers' jobs |

### Security note

Brute-force by job ID is not feasible — IDs are UUID v4 (2¹²² combinations). The ownership check adds defence-in-depth for authenticated deployments: even if a consumer somehow obtained another consumer's UUID, the gateway returns `404`.
