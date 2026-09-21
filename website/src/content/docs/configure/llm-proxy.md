---
title: LLM proxy
---

# LLM proxy

The gateway includes a built-in LLM proxy that forwards JSON requests to any OpenAI-compatible or Anthropic backend, with optional response caching and consumer metrics.

Activated per service by setting `provider` in `config.yaml`. When `provider` is set, JSON requests to that service's paths go through the proxy instead of the bare direct-proxy.

## Request flow

```
Client  POST /v1/chat/completions  {"model": "my-alias", ...}
  │
  ▼
Gateway — LLM proxy
  ├── Cache lookup (Redis, SHA-256 key)     ─── HIT → return cached response
  │                                                    X-Cache: HIT
  ├── MISS → for each backend (weighted-random order):
  │     ├── Rewrite model alias → backend.model (or service-level backend_model)
  │     ├── Inject backend.headers (override service-level inference_headers)
  │     ├── Build provider request (translate if anthropic)
  │     ├── Forward to backend URL
  │     ├── On network error or 5xx → try next backend
  │     └── On 2xx / 4xx → stop retry loop
  ├── Translate response back to OpenAI format
  ├── Emit metrics + track consumer tokens
  ├── Write response to client              X-Cache: MISS
  └── Async cache-fill (5 s timeout, goroutine)
```

Streaming requests (`"stream": true`) bypass cache and response translation. The SSE stream is piped directly to the client with per-chunk flushing. Backend retry is possible before `WriteHeader`; once the SSE stream starts, switching backends is no longer possible.

## Providers

| `provider` | Protocol | Auto-translation |
|---|---|---|
| `openai` | OpenAI Chat Completions API | none |
| `anthropic` | Anthropic Messages API | OpenAI ↔ Messages API |
| `ollama` | Ollama `/api/chat` | none (OpenAI-compatible) |
| `passthrough` | forward verbatim | none |

Use `passthrough` for vLLM, LiteLLM, or any backend that speaks the OpenAI API natively.

> **Token usage requirement:** whichever provider is used, the response the gateway ends up with (after translation, for `anthropic`) must be an OpenAI-compatible JSON body with a top-level `usage.prompt_tokens` / `usage.completion_tokens` object. The gateway parses these two fields to emit token metrics, feed `token_limits`, and populate consumer usage counters ([`GET /usage`](../reference/api)). `provider: passthrough` forwards the backend response unmodified — if that backend does not return an OpenAI-style `usage` object (e.g. a bare completion API), token counting, token-budget limiting, and usage reporting are silently skipped for that response (the request itself is not blocked). Streaming responses need `stream_options.include_usage` support on the backend; the gateway injects this automatically when missing.

### Anthropic translation

When `provider: anthropic` is set, the gateway translates bidirectionally:

- **Request** — OpenAI `messages[]` (including `system` role) → Anthropic `messages[]` + `system` string; `max_tokens` / `max_completion_tokens` → `max_tokens`; `stop` (string or array) → `stop_sequences[]`
- **Response** — Anthropic `content[].text` blocks → OpenAI `choices[0].message.content`; `stop_reason` → `finish_reason`; `input_tokens` / `output_tokens` → `prompt_tokens` / `completion_tokens`

Tool-use fields (`tools`, `tool_choice`) are forwarded as-is on the request side; `tool_use` stop reason maps to `tool_calls` finish reason.

## Model aliases

The `model` field in the service config acts as the client-facing alias. Set `backend_model` (service-level) or a member's `model` (per-backend, via [`backend_pools`](service-registry.md#backend-pools-backend_pools)) to rewrite the `model` field in the request body before forwarding:

```yaml
backend_pools:
  llama3-multi:
    members:
      - url: "http://vllm-v1.default.svc.cluster.local:8000"
        weight: 90
      - url: "http://vllm-v2.default.svc.cluster.local:8000"
        weight: 10
        model: "meta-llama/Meta-Llama-3.1-8B-Instruct"   # overrides backend_model for this backend

services:
  - type: llm
    model: "llama3"            # clients send this
    provider: passthrough
    backend_model: "meta-llama/Meta-Llama-3-8B-Instruct"   # default for all backends
    backend_pool: llama3-multi
```

Cache lookup uses the alias (`model`), so cache keys are stable regardless of which backend served the request or which `backend_model` was sent.

## Multi-backend routing

Services can route to multiple backends with weighted-random primary selection and automatic fallback, via a [backend pool](service-registry.md#backend-pools-backend_pools):

```yaml
backend_pools:
  chat-primary:
    members:
      - url: "http://vllm-primary.default.svc.cluster.local:8000"
        weight: 100          # weight > 0 → eligible for primary selection
        model: "meta-llama/Meta-Llama-3-8B-Instruct"
        headers:
          Authorization: "Bearer ${PRIMARY_TOKEN}"
      - url: "http://vllm-fallback.default.svc.cluster.local:8000"
        weight: 0            # weight = 0 → tried only if all weight>0 members fail
        model: "meta-llama/Meta-Llama-3-8B-Instruct"
        headers:
          Authorization: "Bearer ${FALLBACK_TOKEN}"

services:
  - type: llm
    model: "chat"
    provider: passthrough
    backend_pool: chat-primary
```

**Routing rules:**
- One member is selected by weighted-random among `weight > 0` members.
- On network error or 5xx response, the next member is tried (remaining `weight > 0` members sorted by descending weight, then `weight = 0` members).
- On 4xx, the loop stops immediately — client errors are not retried.
- A member's `headers` are applied after `inference_headers` (and the pool's own `headers`), acting as the highest-precedence override.
- A member's `model` overrides the service-level `backend_model` for that specific backend.

> A service may still declare an inline `backends:` list instead of `backend_pool` — it's accepted for backward compatibility but deprecated in favor of `backend_pools`, which additionally lets several services share one rate-limit/concurrency budget. Prefer `backend_pools` for new services.

## Response caching

Responses are cached in Redis, keyed on SHA-256 of a canonical subset of the request body:

**Cacheable fields** (included in key): `model`, `messages`, `system`, `temperature`, `top_p`, `stop`, `tools`, `tool_choice`

**Excluded fields** (ignored, do not bust the cache): `user`, `metadata`, any unknown fields

**Bypass conditions:**
- `stream: true` in the request body
- `Cache-Control: no-cache` (or any directive containing `no-cache`) in the request header
- Non-2xx response from the backend
- `response_cache_ttl: 0` (disabled)

Cache-fill runs in a background goroutine after the response is written to the client — it never adds latency to the HTTP response. The goroutine has a 5-second timeout.

### Configuration

```yaml
- type: llm
  model: "gpt-4o"
  provider: openai
  response_cache_ttl: 3600   # seconds; 0 = disabled
  operations:
    chat:
      - "/v1/*"              # wildcard: all OpenAI-compatible paths
  inference_url: ""          # empty = defaults to https://api.openai.com
  inference_headers:
    Authorization: "Bearer ${OPENAI_API_KEY}"
```

## Dynamic wildcard routing

Operation paths ending with `/*` register as chi wildcard routes:

```yaml
operations:
  chat:
    - "/v1/*"
```

This matches `/v1/chat/completions`, `/v1/embeddings`, `/v1/models`, and any other path under `/v1/`. Exact paths always take priority over wildcards in chi's radix tree, so other services with explicit paths (e.g. `/v1/audio/transcriptions`) are unaffected.

## Consumer metrics

When `metrics.top_consumers` is set to a positive integer and `server.consumer_header` is configured, token usage is tracked per consumer in Redis sorted sets:

```
Key: llm:consumer:tokens:{user_type}:{prompt|completion}
Member: consumer name
Score: cumulative token count
```

A background goroutine refreshes the Prometheus `GatewAI_llm_consumer_tokens_top` gauge every 60 seconds (and immediately at startup), exposing the top-N consumers by token usage. This avoids unbounded label cardinality — only the top-N appear in Prometheus, not all 100k+ consumers.

For monitoring all consumers independently, query Redis directly:

```bash
redis-cli ZREVRANGEBYSCORE llm:consumer:tokens:sa:prompt +inf -inf WITHSCORES
```

## Prometheus metrics

| Metric | Labels | Description |
|---|---|---|
| `GatewAI_llm_requests_total` | `service_type, model, backend_model, provider, user_type, status` | Request count |
| `GatewAI_llm_request_duration_seconds` | `service_type, model, backend_model, provider, user_type` | Latency histogram |
| `GatewAI_llm_tokens_total` | `service_type, model, backend_model, user_type, type` | Token counter (`prompt` / `completion`) |
| `GatewAI_llm_tokens_per_request` | `service_type, model, backend_model, user_type` | Token distribution histogram |
| `GatewAI_llm_consumer_tokens_top` | `consumer, user_type, type` | Top-N consumer token gauge |
| `GatewAI_cache_hits_total` | `service_type, model` | Cache hit counter |
| `GatewAI_cache_misses_total` | `service_type, model` | Cache miss counter |
| `GatewAI_cache_errors_total` | `service_type, model, op` | Cache error counter (`key`/`get`/`set`) |

### Example queries

```promql
# Cache hit ratio per model
sum by (model) (rate(GatewAI_cache_hits_total[5m]))
/
sum by (model) (rate(GatewAI_llm_requests_total[5m]))

# p99 token count per request
histogram_quantile(0.99, sum by (le, model) (
  rate(GatewAI_llm_tokens_per_request_bucket[1h])
))

# Top consumers by prompt tokens (from the top-N gauge)
topk(10, GatewAI_llm_consumer_tokens_top{type="prompt"})
```
