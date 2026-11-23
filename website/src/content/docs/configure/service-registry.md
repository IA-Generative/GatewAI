---
title: Service registry
---

# Service registry

The gateway is entirely config-driven. Adding a new model or service type requires only a YAML block in `config.yaml` — no Go code change.

## Service entry fields

```yaml
services:
  - type: audio                        # service type (used in /jobs/{service_type})
    model: "whisper-large-v3"          # OpenAI "model" field value
    default: true                      # fallback when request omits model field

    # Sync / OpenAI-compatible
    operations:
      transcription:
        - "/v1/audio/transcriptions"   # all paths indexed; first used for async
      translation:
        - "/v1/audio/translations"
    inference_url: "http://..."        # backend base URL (path appended at runtime)

    # File validation (async mode only)
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

    # Backend authentication — sync-direct only (optional)
    inference_headers:
      Authorization: "Bearer ${WHISPER_API_KEY}"
```

## Deprecating a model (`deprecated`)

```yaml
services:
  - type: audio
    model: "whisper-large-v2"
    deprecated: true                    # informational only — routing is unaffected
```

Setting `deprecated: true` on a service entry doesn't change routing or availability — it's purely a signal to API consumers, surfaced two ways:

- `GET /v1/models` returns `capabilities.deprecated: true` for that model.
- The generated OpenAPI spec (`/openapi.yaml`, `/docs`) marks the relevant operation with the standard [`deprecated: true`](https://swagger.io/docs/specification/v3_0/paths-and-operations/#deprecated-operations) field, which Swagger UI renders with a strikethrough.

Deprecation is applied at the finest granularity the spec allows:

- The per-model swagger doc served at `/docs/spec/{type}/{model}` (when `swagger_url` is configured) always marks its operations deprecated.
- A sync path shared by multiple models (e.g. several models behind the same `/v1/*` route) is only marked `deprecated: true` at the operation level once **every** model on that path is deprecated — otherwise still-active models on the same path would inherit a misleading warning. Until then, the deprecated model(s) are called out by name in the `model` field's description instead.

## Model resolution order

When a request omits the `model` field:

1. Single model registered for the type/path → auto-selected
2. Model marked `default: true` → used as fallback
3. Error listing available models

## Operations

The `operations` map replaces the old flat `openai_paths` list. Each key is an operation name; the value is a list of URL paths.

- **All paths** are indexed for sync routing (`POST /v1/*`)
- **First path** of the selected operation is embedded in the `InputEvent.InferenceURL` for async jobs
- The `operation` form field selects which operation to use for async submission (required when a model has multiple operations)

```bash
# Async: specify operation when model has multiple
curl -X POST /jobs/audio \
  -F file=@audio.wav \
  -F operation=translation

# Sync: operation is implicit from the URL path
curl -X POST /v1/audio/translations -F file=@audio.wav
```

## Multiple models per type

Multiple service entries may share the same `type` with different `model` values:

```yaml
services:
  - type: audio
    model: "whisper-large-v3"
    default: true
    ...

  - type: audio
    model: "whisper-large-v3-turbo"
    ...
```

The gateway routes by the `model` field in the request. The `default: true` flag designates the fallback.

## Path patterns with `{model}`

Paths can embed the model name directly in the URL:

```yaml
operations:
  infer:
    - "/v2/models/{model}/infer"
```

The gateway extracts the model name from the URL segment and routes accordingly. No `model` field is required in the request body.

## Backend authentication (`inference_headers`)

For sync-direct services whose backend requires authentication, add `inference_headers`:

```yaml
- type: reranker
  model: "bge-reranker-v2-m3"
  operations:
    rerank:
      - "/rerank"
  inference_url: "http://GatewAI-reranker-predictor.svc.cluster.local"
  inference_headers:
    Authorization: "Bearer ${RERANKER_API_KEY}"
```

Headers are injected on every outgoing request to the backend. Values support `${VAR}` expansion. Config headers override client headers with the same name.

:::note
`inference_headers` only applies to **sync-direct** proxy and **LLM proxy**. Async jobs processed by the relay are unaffected.
:::

## LLM proxy services

When `provider` is set, JSON requests bypass the standard direct proxy and go through the built-in LLM proxy:

```yaml
- type: llm
  model: "gpt-4o"
  provider: openai            # openai | anthropic | ollama | passthrough
  backend_model: ""           # optional: rewrites model field sent to the backend
  response_cache_ttl: 3600    # seconds; 0 = disabled
  operations:
    chat:
      - "/v1/*"               # wildcard: matches all paths under /v1/
  inference_url: ""           # empty = provider default (e.g. https://api.openai.com)
  inference_headers:
    Authorization: "Bearer ${OPENAI_API_KEY}"
```

`backend_model` rewrites the `model` field in the JSON body before forwarding — useful for vLLM which expects HuggingFace model IDs. The cache key always uses the alias (`model` from config), not the backend name.

The real backend model is **surfaced to clients** in `GET /v1/models` as a `backend_model` field on the model object, so developers can see what actually runs behind each alias:

```json
{
  "id": "next-gen",
  "object": "model",
  "service_type": "llm",
  "provider": "passthrough",
  "backend_model": "meta-llama/Meta-Llama-3-8B-Instruct"
}
```

`backend_model` is omitted when the alias is forwarded unchanged (no rewrite configured). It only ever reflects the **service-level** `backend_model` — a per-backend `backends[].model` override (e.g. for a canary rollout) still rewrites the outgoing request per backend, but is not surfaced in `GET /v1/models`.

See [LLM proxy](llm-proxy.md) for full documentation.

## Model visibility (`visibility`)

A model can be gated to a specific audience so it is only discoverable and usable by chosen callers — useful for **beta-testing a new model through the same API** before opening it to everyone.

```yaml
services:
  - type: llm
    model: "next-gen"                 # stable public alias
    provider: passthrough
    backend_model: "llama-3.3-70b"    # real model, surfaced in /v1/models
    operations:
      chat:
        - "/v1/chat/completions"
    inference_url: "http://vllm.svc"
    visibility:
      user_types: ["beta"]            # callers whose user type is "beta"…
      groups: ["ai-team"]             # …OR who belong to the "ai-team" group
```

A model with a `visibility` block is **restricted**: it is only visible to callers who match at least one entry. Matching is a union across both fields:

- `user_types` — matched against the caller's user type, read from `server.user_type_header` (bridged automatically in `proxy` and `oauth2` auth modes).
- `groups` — matched against the authenticated `Principal`'s groups (requires an `auth` mode that resolves groups).

A model **without** a `visibility` block is public — visible to everyone, including anonymous callers.

Enforcement is uniform and **fail-closed**: a caller outside the audience (including one with no identity) is treated as if the model does not exist —

- `GET /v1/models` omits it from the list, and `GET /v1/models?model=<name>` returns `404`.
- `POST /v1/*` and `POST /jobs/{service_type}` return `404` when they resolve to a hidden model.

A hidden model is therefore **indistinguishable from a non-existent one** — there is no `403` that would leak its existence.

Visibility composes with [access control](../configure/access-control) (`policies`): a request must satisfy **both** the model's visibility *and* any authz allow-rule. Visibility gates discovery; `policies` gates authorization. The Prometheus counter `gatewai_model_hidden_total{service_type, model}` increments whenever a request is rejected because the model was hidden from the caller.

## Sync concurrency cap (`max_concurrent_sync`)

To protect a backend from being overwhelmed by simultaneous sync requests, set `max_concurrent_sync` on the service entry:

```yaml
services:
  - type: llm
    model: "gpt-4o"
    provider: openai
    inference_url: "https://api.openai.com"
    max_concurrent_sync: 10   # at most 10 parallel sync calls for this model
    operations:
      chat:
        - "/v1/*"
```

The limit is enforced by a **shared Redis semaphore** (`gateway:semaphore:sync:{model}`) — the cap applies across all gateway replicas, not per pod. When all slots are busy, the gateway returns **`503 Service Unavailable`** immediately (no queuing).

- `0` (default) — no limit
- **Fail-open**: Redis errors are logged and the request is allowed through
- **Scope**: sync-direct and LLM-proxy requests only (`/v1/*`); async jobs are unaffected

The semaphore TTL is 30 minutes — a safety net that resets the counter if a gateway replica crashes while holding a slot.

### Reserving capacity for priority requests (`priority_reserved_sync`)

`priority_reserved_sync` carves out part of `max_concurrent_sync` exclusively for requests carrying the `server.priority_header` (the same header used for async queue-jump priority — see [priority routing](priority-routing.md)):

```yaml
services:
  - type: llm
    model: "gpt-4o"
    max_concurrent_sync: 10
    priority_reserved_sync: 3   # 3 of the 10 slots are reserved for priority requests
```

- `0` (default) — no reservation; priority requests compete in the shared pool like everyone else
- Non-priority requests only ever draw from the shared pool (`10 - 3 = 7` slots here)
- Priority requests try the reserved pool (`gateway:semaphore:sync:{model}:priority`) first, then fall back to the shared pool once the reserved pool is full — so a priority request is never worse off than a normal one
- This is capacity reservation, not queuing: a request still gets an immediate `200`/`503`, it just has a better chance of the former

## Backend pools (`backend_pools`)

A named, load-balancer-style group of member backends that several `services[]` entries can share via `backend_pool: <name>`, instead of each declaring its own `backends:`/`inference_url:`. Useful when the same physical backend serves more than one model alias — the pool centralizes auth headers and shares one rate-limit / concurrency budget across every service routed to it.

```yaml
backend_pools:
  vllm-llama3:
    members:
      - url: "http://vllm-1.default.svc.cluster.local:8000"
        weight: 3
        headers:
          Authorization: "Bearer ${VLLM_1_TOKEN}"
        rate_limit: { rate: 20, period: 1s }   # optional per-member cap
        max_concurrent: 10                      # optional per-member cap
      - url: "http://vllm-2.default.svc.cluster.local:8000"
        weight: 1
    headers:                                    # pool-level default headers (lowest precedence)
      X-Pool-Auth: "${VLLM_SHARED_KEY}"
    rate_limit: { rate: 50, period: 1s }        # pool-wide shared budget across all members
    max_concurrent: 30                          # pool-wide shared concurrency budget
    priority_reserved_concurrent: 5             # mirrors services[].priority_reserved_sync

services:
  - type: llm
    model: "gpt-4o"
    backend_pool: vllm-llama3     # mutually exclusive with backends:/inference_url:
    provider: openai
    operations:
      chat:
        - "/v1/chat/completions"
  - type: llm
    model: "llama3-chat"
    backend_pool: vllm-llama3     # same pool, different alias — shares its budget
    provider: openai
    operations:
      chat:
        - "/v1/chat/completions"
```

- **Mutually exclusive** with `backends:`/`inference_url:` on the same service — set exactly one, checked at config validation.
- **Header precedence** (lowest → highest): service `inference_headers` → `backend_pools[].headers` → `backend_pools[].members[].headers`.
- **Concurrency** is acquired once per logical client request — the same one-request-one-slot semantics as `max_concurrent_sync` above — via a dedicated `gateway:semaphore:pool:` Redis key namespace distinct from per-model semaphores.
- **Rate limiting** is checked per backend attempt inside the retry loop: pool-wide and per-member budgets are both enforced (AND), and a rate-limited member is skipped in favor of the next one before falling back to an error.
- **Circuit breaker** state is unaffected — it's already keyed by backend URL, so services sharing a pool already share breaker health.
- Pool rate-limit/concurrency exhaustion is deliberately **not** reflected in `GET /v1/models`'s `capabilities.degraded` — it's transient backpressure, not a degraded backend; that stays circuit-breaker-only.

See the [Helm chart](../set-up/helm.md#backend-pools) for the equivalent `values.yaml` schema and the [metrics reference](../reference/metrics.md#backend-pools) for `gatewai_backend_pool_*`.

## Input adapter (`input_adapter`)

Some backends (vision / OCR models, for instance) speak **only** `/v1/chat/completions`. An `input_adapter` is a config-driven stage that turns a client's **multipart file upload** into an OpenAI chat-completions JSON body, then hands it to the regular JSON / [LLM proxy](llm-proxy.md) pipeline. Quota (`token_limits`), the response cache, guardrails and metrics therefore apply unchanged — no per-model code and no custom route.

```yaml
services:
  - type: ocr
    model: lighton-ocr
    provider: passthrough          # REQUIRED when input_adapter is set
    inference_url: "http://localhost:9000"
    operations:
      ocr:
        - "/v1/chat/completions"   # a chat-completions path is REQUIRED
    input_adapter:
      type: multipart_to_chat
      prompt_field: prompt          # client multipart form field -> user message text
      default_prompt: "document parsing."   # used when the form field is absent/empty
      prompt_last: false            # true -> file before prompt (see note below)
      max_tokens_field: max_tokens  # optional client form field
      max_tokens_default: 4096
      max_tokens_cap: 8192          # clamp; 0 = no cap
      extra_json_fields:            # top-level JSON fields merged into the body
        skip_special_tokens: false
        temperature: 0
```

The uploaded file is base64-encoded into a `data:<content-type>;base64,...` `image_url` part of a single user message (the standard OpenAI vision shape), alongside the text prompt.

By default the message content is ordered `[text, image]` (prompt first). Set **`prompt_last: true`** to emit `[image, text]` instead — some backends only activate a mode when the file precedes the instruction (e.g. LightOnOCR-3-4B only emits bounding boxes / "grounding" output when the prompt comes *after* the image).

The configured `prompt_field` and `max_tokens_field` are surfaced as form fields in the generated OpenAPI spec (`/openapi.yaml`, `/docs`), with the `default_prompt` shown as each adapter-backed model's default — so callers can discover them from Swagger UI.

**Requirements** — config load fails with a clear error if either is missing:

- `provider` must be set (the adapter rides the provider pipeline).
- The service must expose a `/v1/chat/completions` operation path.

**Sync** (`POST /v1/*` multipart) and **async** (`POST /jobs/{type}`) are both supported:

- **Sync** — the upload size is bounded by the service's `max_file_size_mb` (not `server.max_body_mb`); an oversize upload returns `413`.
- **Async** — the gateway stores the transformed JSON in object storage and the relay posts it verbatim to the backend. This is automatic; nothing is configured on the relay (see [Relay async flow](../relay/async.md)).
- **Per-consumer token quota** (`token_limits`) applies on both paths. For async jobs, usage is read from the backend's `usage` block at completion.

:::caution
Scope is deliberately narrow: **one uploaded file becomes one chat request.** There is no PDF handling, no multi-page expansion and no file conversion inside GatewAI — this keeps the gateway dependency-free. Convert PDFs to images client-side (or in a separate service) before uploading.
:::

:::note
Input-stage regex [guardrails](guardrails.md), if configured on an adapter service, scan the base64 blob too — wasteful, with no useful matches. Avoid body-regex guardrails on adapter services and rely on the output / async stages instead.
:::

## Hot reload

The service registry is reloaded atomically via `POST /-/reload`. The HTTP router is swapped with the new registry. Infrastructure (S3, Redis) is not re-initialised.
