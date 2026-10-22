---
title: Relay queue (Redis)
---

# Relay queue (Redis)

## Overview

Async jobs are routed through Redis lists instead of Kafka. The gateway pushes job IDs onto a per-model list; each relay pod pops one job, processes it, and exits.

## Redis key schema

| Key | Type | Operation | Description |
|---|---|---|---|
| `relay:<model>:pending` | list | RPUSH (normal) / LPUSH (priority) | Jobs waiting to be picked up |
| `relay:<model>:processing` | list | BLMOVE destination | Job currently being processed by a pod |
| `relay:<model>:lease:<id>` | string | SET with TTL | Per-job processing lease held by the relay pod, refreshed while it works (see [Crash recovery](#crash-recovery)) |
| `relay:<model>:deadletter` | list | RPUSH | Jobs the reaper could not recover after `gc.max_reap_attempts` requeues |
| `jobs:<model>:completed` | pub/sub channel | PUBLISH | Relay signals completion to the gateway |
| `relay:<model>:cancel` | pub/sub channel | PUBLISH | Gateway signals job cancellation to the relay |

## Priority routing

When `server.priority_header` is set and the header is present in the request, the gateway uses `LPUSH` instead of `RPUSH`. The relay always pops from the left (`BLMOVE LEFT RIGHT`), so priority jobs are dequeued first. No separate Deployment is needed.

## KEDA autoscaling

The relay Deployment is scaled by KEDA using the Redis scaler on `relay:<model>:pending` list length:

```yaml
triggers:
  - type: redis
    metadata:
      address: redis:6379
      listName: relay:whisper-large-v3:pending
      listLength: "1"
```

`minReplicaCount: 0` — pods scale to zero when the queue is empty.

## Relay lifecycle

Each relay pod:
1. Polls `/health` on the local inference service until ready
2. Calls `BLMOVE relay:<model>:pending relay:<model>:processing LEFT RIGHT` with a configurable timeout (`queue_pop_timeout`, default 30s), then acquires a processing **lease** (`relay:<model>:lease:<id>`, TTL `lease_ttl`, default 60s)
3. Exits `0` on timeout (empty queue) or SIGTERM — KEDA will not restart a zero-queue pod
4. Processes the job (download S3 → inference → upload S3 → publish completion), refreshing the lease every `lease_ttl/3` throughout
5. Exits `0` — KEDA creates a fresh pod for the next job; `Done` removes the processing entry **and** the lease

## Crash recovery

A relay pod that dies mid-job (OOM, node loss, SIGKILL past the grace period) never calls `Done`, so its job is left in `relay:<model>:processing`. Because each job carries a short-lived **lease** that the pod refreshes while it works, a dead pod's lease expires within `lease_ttl` (default 60s).

The gateway GC's **phase 0 reaper** (`lifecycle.gc.enabled: true`) scans the processing list and, for any entry with **no live lease**:

- **job record still valid** → requeues it to `relay:<model>:pending` for another pod to pick up (up to `gc.max_reap_attempts` times, default 3);
- **exceeds the attempt cap** → dead-letters it to `relay:<model>:deadletter` and marks the job failed;
- **record already gone / terminal** → drops the stale entry.

The lease check is atomic with the reclaim (Lua), so a healthy worker's job is never requeued, and it is idempotent across gateway replicas. Outcomes are counted in `gatewai_async_jobs_reaped_total{model,outcome}`. This gives async jobs **at-least-once** processing across pod crashes; pair it with an [`Idempotency-Key`](../configure/configuration#idempotency) if the client must also avoid duplicate submissions.

Recovery latency is bounded by the GC interval — lower `lifecycle.gc.interval` for faster requeue.

## Stale processing list

Phase 0 above handles orphans whose job record is still live. Once a record has **expired** (past its TTL), the job can no longer be reprocessed — GC **phase 3** (see [Configuration — `gc`](../configure/configuration#gc)) then removes the leftover queue entry from both `pending` and `processing` so `gatewai_relay_queue_depth` doesn't stay stuck forever. This is a cleanup, not a retry. Swept entries are counted in `gatewai_relay_queue_orphans_swept_total{model,state}`.

To act on a stuck job manually — e.g. if `gc.enabled` is `false`:

```bash
# Inspect processing list
redis-cli LRANGE relay:whisper-large-v3:processing 0 -1

# Move job back to pending to retry it (job record still valid)
redis-cli LMOVE relay:whisper-large-v3:processing relay:whisper-large-v3:pending RIGHT LEFT

# Or drop it without retrying (job record already failed/expired)
redis-cli LREM relay:whisper-large-v3:processing 1 <job-id>
```
