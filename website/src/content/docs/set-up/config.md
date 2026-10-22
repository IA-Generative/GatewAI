---
title: Configuration
---

# Configuration

Both the gateway and the relay load a YAML config file at startup. The path defaults to `config.yaml` in the working directory and can be overridden with the `CONFIG_PATH` environment variable.

## Environment variable expansion

All string values in `config.yaml` support env var expansion before YAML parsing:

```yaml
s3:
  endpoint: "${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"  # with default
  access_key: "${S3_ACCESS_KEY}"                            # required
```

Syntax:

| Form | Behaviour |
|------|-----------|
| `${VAR}` | Replaced with the value of `VAR`. Empty string if unset |
| `${VAR:-default}` | Replaced with `VAR` if set and non-empty, otherwise `default` |

## Hot reload

```
POST /-/reload
```

Reloads `config.yaml` from disk without restarting the gateway. If the new config is invalid, the old config stays active and the endpoint returns `500`.

Service registry, rate limits, LLM proxy settings, and guardrails are all reloaded. Active connections are not interrupted.

## Gateway config reference

### `server`

```yaml
server:
  addr: ":8080"               # default
  read_timeout: 120s          # default
  write_timeout: 0            # no timeout by default (safe for long inference)
  idle_timeout: 120s          # default
  consumer_header: "X-Consumer-Username"  # set by APISIX after auth
  user_type_header: "X-User-Type"         # set by OPA (sa | user | ...)
  priority_header: "X-Priority"           # triggers LPUSH instead of RPUSH
```

`consumer_header` enables per-consumer job ownership, metrics, and job listing. `user_type_header` enables per-user-type rate limits. Both are optional.

### `s3`

```yaml
s3:
  endpoint: "https://s3.fr-par.scw.cloud"
  region: "fr-par"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket: "my-gatewai-jobs"
  use_path_style: true   # required for most S3-compatible providers
  ca_bundle: ""          # path to PEM file for private CA
  ssl_insecure: false    # disable TLS verification (dev only)
```

### `redis`

```yaml
redis:
  addr: "redis:6379"
  password: ""
  db: 0
  pending_max_age: "2h"   # jobs stuck in pending longer than this are swept by GC
```

### `lifecycle`

```yaml
lifecycle:
  persists_result: false    # true = keep result in S3/Redis until TTL; false = delete on first read
  job_ttl:
    global: "72h"           # fallback TTL for all statuses
    completed: "24h"        # override per status
    pending: ""
    failed: "48h"
    cancelled: ""
  gc:
    enabled: true
    interval: "15m"         # how often the GC runs
    orphan_min_age: "5m"    # min age before an S3 object is considered orphaned
```

### `encryption`

```yaml
encryption:
  key: "${ENCRYPTION_KEY}"  # hex-encoded 32-byte AES-256 key; empty = disabled
```

See [Encryption](../configure/encryption) for details.

### `metrics`

```yaml
metrics:
  top_consumers: 20      # expose top-N consumers via gauge; 0 = disabled
  consumer_labels: false # per-consumer labels on counter (< 50 consumers only)
```

### `rate_limits`

```yaml
rate_limits:
  llm:
    sa:    { rate: 100, period: 1m }
    user:  { rate: 20,  period: 1m, token_rate: 100000, token_period: 1h }
    "*":   { rate: 10,  period: 1m }   # fallback
```

See [Rate limiting](../configure/rate-limiting) for details.

---

## Relay config reference

The relay config file (`relay/config.yaml` by default, overridden by `CONFIG_PATH`) covers the model it serves, connectivity (Redis, S3, the local inference backend, the gateway callback), and the processing lease. There is no `services:` list — one relay serves one `model`.

```yaml
model: "whisper-diarization"   # the model this relay serves (queue relay:<model>:pending)
lease_ttl: "60s"               # per-job processing lease TTL; refreshed every lease_ttl/3

redis:
  addr: "redis:6379"
  password: ""

s3:
  endpoint: "https://s3.fr-par.scw.cloud"
  region: "fr-par"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket: "my-gatewai-jobs"
  use_path_style: true

encryption:
  key: "${ENCRYPTION_KEY}"   # must match the gateway key

# Local inference backend. The OpenAI path is supplied per-job by the gateway
# (Job.inference_url) and appended to base_url at runtime.
inference:
  base_url: "http://127.0.0.1:9000"
  timeout: "300s"
  ready_timeout: "10m"

# Completion callback: after persisting the result the relay calls
# POST <base_url>/-/relay/jobs/{id}/complete so usage + webhooks fire once per job.
gateway:
  base_url: "http://gatewai:8080"

queue_pop_timeout: "30s"   # BLMOVE timeout; relay exits 0 on timeout (KEDA scale-to-zero)
```

The `encryption.key` must be identical in the gateway and every relay.
