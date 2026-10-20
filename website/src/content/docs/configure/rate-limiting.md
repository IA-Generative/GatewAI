---
title: Rate limiting
---

# Rate limiting

The gateway supports two independent rate-limiting mechanisms, both backed by Redis fixed-window counters:

- **Request-rate limiting** (`rate_limits`) — caps the number of requests per consumer per time window. Applies to all service types (async and sync).
- **Token-budget limiting** (`token_limits`) — caps the total LLM tokens consumed per consumer per time window. Applies to sync LLM requests only (optimistic: tokens are counted after the response is received).

Both checks are **fail-open**: Redis errors are logged and the request is allowed through. Both can be configured simultaneously; a request is rejected if either limit is exceeded.

## Request-rate limiting

### How it works

For each eligible request, the gateway runs an atomic Lua script in Redis:

```lua
local count = redis.call('INCR', key)
if count == 1 then redis.call('EXPIRE', key, window_seconds) end
return count
```

If `count > rate`, the gateway returns **`429 Too Many Requests`** with a `Retry-After` header (seconds until the window resets).

Rate limiting applies at two points:
- **Async jobs** — before `ParseMultipartForm` in `POST /jobs/{service_type}`
- **Sync requests** — after path routing in `POST /v1/*`

### Configuration

```yaml
server:
  consumer_header: "X-Consumer-Username"   # identifies the consumer
  user_type_header: "X-User-Type"          # sa | user | premium | ...

rate_limits:
  audio:
    sa:
      rate: 100
      period: 1m
    user:
      rate: 20
      period: 1m
    "*":                    # fallback: applies when user_type is absent or unlisted
      rate: 10
      period: 1m
  ocr:
    "*":
      rate: 5
      period: 1m
```

Set `rate: 0` to bypass Redis entirely for a specific user type:

```yaml
rate_limits:
  audio:
    unlimited:
      rate: 0        # no limit — Redis is never touched for this user type
    sa:
      rate: 100
      period: 1m
    "*":
      rate: 10
      period: 1m
```

### Key concepts

- **`service_type`** — matches `services[].type` (e.g. `audio`, `ocr`, `llm`); all models of the same type share the counter.
- **`user_type`** — value from `server.user_type_header`; use `"*"` as a catch-all fallback.
- **`rate: 0`** — sentinel for "no limit"; the request is always allowed without incrementing any counter.
- **`period`** — accepts Go duration strings: `30s`, `1m`, `1h`, `24h`.
- **Absent consumer** — requests without a consumer header bypass rate limiting entirely (typically internal traffic).
- **Absent `rate_limits` entry** — no limit is applied for that `(service_type, user_type)` pair.

### Redis key format

```
rl:{consumer}:{service_type}:{user_type}
```

---

## Token-budget limiting

### How it works

After each successful LLM response, the gateway reads the `usage.prompt_tokens` / `usage.completion_tokens` fields from the response body and increments a Redis counter. If the counter exceeds the configured budget for the current window, the **next** request from that consumer is rejected with `429`.

Because token counts are only known after the response, enforcement is **optimistic**: the request that pushes a consumer over their budget is allowed through; subsequent requests within the same window are blocked.

> **The backend response must be OpenAI-compatible.** This only works if the response body the gateway receives (after provider translation) contains a top-level `usage` object shaped like OpenAI's. With `provider: passthrough`, the backend's raw response is used as-is — if it doesn't include an OpenAI-style `usage` object, no tokens are counted and this request never counts against the budget (it also isn't blocked). See [LLM proxy](./llm-proxy) for provider details.

Streaming responses (`stream: true`) and requests with `Cache-Control: no-cache` are excluded from token counting.

### Configuration

Token limits are configured **per service** (i.e. per model), under `services[].token_limits`. This is distinct from `rate_limits`, which is global across all models of a type.

```yaml
services:
  - type: llm
    model: gpt-4o
    provider: openai
    inference_url: "https://api.openai.com"
    token_limits:
      user:
        token_rate: 100000     # max tokens per window for "user" type
        token_period: 24h
      sa:
        token_rate: 1000000
        token_period: 1h
      "*":                     # fallback for unlisted user types
        token_rate: 50000
        token_period: 24h
```

Multiple models can have independent token budgets even if they share the same `type`.

### Sharing a budget across aliases (`budget_group`)

By default each model's token budget is counted separately (keyed by the model name). When several aliases map to one physical backend — e.g. a floating `code`, a pinned `code-1.0`, and a `code-completion` all on one pool — give them the same `budget_group` so a consumer's token budget is counted **once** across all of them, instead of once per alias:

```yaml
services:
  - { type: llm, model: code,            budget_group: coder, token_limits: { "*": { token_rate: 1000000, token_period: 24h } }, ... }
  - { type: llm, model: code-1.0,        budget_group: coder, token_limits: { "*": { token_rate: 1000000, token_period: 24h } }, ... }
  - { type: llm, model: code-completion, budget_group: coder, token_limits: { "*": { token_rate: 1000000, token_period: 24h } }, ... }
```

Members of a group must be the same service type and declare **identical** `token_limits` (config validation rejects a mismatch or a member with none). The shared counter is keyed `trl:{consumer}:model:{budget_group}:{user_type}`, surfaced once in `GET /-/usage` (with a `budget_group` field), and reset by `POST /-/quota/reset?model=<any-alias>`. Leaving `budget_group` unset keeps the per-alias default.

### Key concepts

- **`token_rate`** — maximum total tokens (input + output) allowed per `token_period`. Set to `0` to disable.
- **`token_period`** — duration string: `1h`, `24h`, etc.
- **`user_type`** — same header as request-rate limiting (`server.user_type_header`); `"*"` is the catch-all fallback.
- **Absent consumer** — anonymous requests are never token-limited.
- **Streaming** — token counting is skipped; streaming requests always pass the token check.

### Redis key format

```
trl:{consumer}:{service_type}:{user_type}
```

---

## Concurrent job limiting (async)

### How it works

Before saving a job, the gateway counts the consumer's current `pending` + `processing` jobs (from the consumer sorted set) plus any in-flight submission slots. If the total reaches the limit, the request is rejected with `429`.

The slot is reserved atomically on submit and released immediately after the job is saved to Redis — so this limit reflects active jobs, not requests per second.

### Configuration

```yaml
rate_limits:
  audio:
    user:
      max_concurrent: 5      # at most 5 jobs pending+processing at once
    sa:
      max_concurrent: 20
    "*":
      max_concurrent: 3
```

`max_concurrent: 0` (default) disables the check entirely.

### Key concepts

- **Async only** — applies to `POST /jobs/{service_type}` only; sync (`/v1/*`) requests are not affected.
- **Absent consumer** — anonymous requests are never concurrent-limited.
- No `period` field: this is a live count, not a fixed window.

### Response headers

```
X-Concurrent-Limit: 5
X-Concurrent-Remaining: 2
```

### Redis key format

```
jc:{consumer}:{service_type}:{user_type}
```

---

## Processing-time budget (async)

### How it works

After each async job completes, the relay reports the actual inference duration (in seconds). The gateway accumulates that value into a Redis counter per consumer per window. If the accumulated seconds exceed the budget before the window expires, subsequent job submissions are rejected with `429`.

Unlike request-rate limiting, this tracks **inference cost** rather than request count. A consumer that submits one 10-minute transcription consumes 600 s of budget — equivalent to 600 one-second jobs.

### Configuration

```yaml
rate_limits:
  audio:
    user:
      processing_time: 3600      # max 1 hour of inference per window
      processing_period: 24h
    sa:
      processing_time: 36000
      processing_period: 24h
    "*":
      processing_time: 600
      processing_period: 1h
```

### Key concepts

- **Async only** — applies to `POST /jobs/{service_type}` only.
- **Optimistic**: the job that pushes a consumer over budget is allowed through; subsequent submissions within the same window are blocked.
- **Absent consumer** — anonymous requests are never processing-time-limited.
- `processing_time` is in **seconds** (integer); `processing_period` accepts Go duration strings.

### Response headers

```
X-ProcessingTime-Limit: 3600
X-ProcessingTime-Remaining: 1234
```

### Redis key format

```
ptrl:{consumer}:{service_type}:{user_type}
```

---

## Summary

| Mechanism | Field(s) | Scope | Redis key prefix |
|---|---|---|---|
| Request rate | `rate` + `period` | async + sync | `rl:` |
| Token budget (global) | `token_rate` + `token_period` | sync LLM only | `trl:` |
| Token budget (per model) | `services[].token_limits` | sync LLM only | `trl:` |
| Concurrent jobs | `max_concurrent` | async only | `jc:` |
| Processing-time budget | `processing_time` + `processing_period` | async only | `ptrl:` |

All checks are **fail-open** (Redis errors are logged, request allowed). All checks except `max_concurrent` are **optimistic** (the triggering request passes; subsequent ones are blocked).

> **Reading current-window values:** Use `GET /usage` to see the current-window counters for your consumer alongside cumulative totals. The `window.reset_at` field shows when each counter resets. See [API reference](../reference/api) for details.

---

## Response

When any limit is exceeded:

```http
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Retry-After: 42

{"error": {"message": "rate limit exceeded", "type": "http_429"}}
```

`Retry-After` is the number of seconds until the Redis key expires (i.e. until the window resets).

---

## Prometheus metrics

| Metric | Labels | Description |
|---|---|---|
| `GatewAI_ratelimit_requests_total` | `service_type, user_type, result` | Request-rate checks (`allowed` / `rejected`) |
| `GatewAI_ratelimit_consumer_hits_total` | `service_type, user_type` | Consumers that hit their request-rate limit |
| `GatewAI_ratelimit_errors_total` | `service_type` | Redis errors during request-rate check |
| `GatewAI_token_ratelimit_checked_total` | `service_type, user_type, result` | Token-budget checks (`allowed` / `rejected`) |
| `GatewAI_token_ratelimit_errors_total` | `service_type` | Redis errors during token-budget check |

### Example queries

```promql
# Request rejection rate per service
sum by (service_type) (rate(GatewAI_ratelimit_requests_total{result="rejected"}[5m]))
/
sum by (service_type) (rate(GatewAI_ratelimit_requests_total[5m]))

# Token budget rejection rate per service
sum by (service_type) (rate(GatewAI_token_ratelimit_checked_total{result="rejected"}[5m]))
/
sum by (service_type) (rate(GatewAI_token_ratelimit_checked_total[5m]))
```

## PrometheusRules

Two alerting rules are shipped with the Helm chart:

| Alert | Threshold | Severity |
|---|---|---|
| `GatewayRateLimitHighRejectionRate` | > 20% rejections over 5 min | warning |
| `GatewayRateLimitErrors` | any Redis error | warning |

See the runbooks: [GatewayRateLimitHighRejectionRate](../manage/runbooks#gatewayratelimithighrejectionrate) and [GatewayRateLimitErrors](../manage/runbooks#gatewayratelimiterrors).
