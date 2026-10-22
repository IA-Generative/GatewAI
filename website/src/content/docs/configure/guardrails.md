---
title: Guardrails
---

# Guardrails

GatewAI can scan for **PII** and **secrets** both on the way **in** (the request, before it reaches the backend) and on the way **out** (the model's response, before it returns to the client — output DLP). The input/output stages run on the sync LLM-proxy path (`POST /v1/*`); input scanning also guards async submit text on `POST /jobs`, and a separate **async result** stage (`guardrails.async`) scans completed async job results (transcripts, OCR, …). Beyond regex PII/secrets, guardrails can also run **model-backed detectors** (e.g. prompt-injection) and scan **tool-call arguments**, not just message content. Configured per service.

## Configuration

Guardrails are enabled on a service by declaring a non-empty `checks` list:

```yaml
services:
  - type: llm
    model: "chat-smart"
    provider: passthrough
    guardrails:
      checks: [pii, secrets]   # which detector groups to run
      action: block            # block (default) | redact | flag
```

Guardrails are **opt-in per service** — omit the `guardrails` block (or leave `checks` empty) to disable them. There is no global toggle.

## Stages: input and output

The top-level `checks`/`action` are the **input** stage (scans the request). Add an optional **`output`** block to also scan the model's *response* (output DLP):

```yaml
guardrails:
  checks: [pii, secrets]   # input stage — the request
  action: redact
  output:                  # output stage — the response (optional)
    checks: [pii, secrets]
    action: redact         # block | redact | flag
```

Output actions mirror input but operate on `choices[*].message.content`: `redact` masks matches before the response is returned (and the redacted body is what gets cached), `block` returns `422 {"error":"response blocked by guardrails"}`, `flag` logs only.

:::note
**Streaming output** (`stream: true`) is governed by `output.streaming`:
- `flag` (default) — streams unbuffered; detects and logs without modifying bytes.
- `block` — holds a rolling window of `stream_window_tokens` (default 64) and terminates the stream on a violation.
- `buffer` — holds the rolling window, redacts matched spans, then releases.

The held window means a match up to the window length is never split; a pattern longer than the window may release a prefix before detection — raise `stream_window_tokens` or use a non-streaming request for strict enforcement. (Streamed `delta.tool_calls` argument deltas are not yet scanned.)
:::

## Actions

| Action | Behaviour |
|---|---|
| `block` (default) | Reject the request with `422 Unprocessable Entity`; the backend is never called. |
| `redact` | Replace each match in-place with a placeholder (e.g. `[REDACTED_EMAIL]`) and forward the cleaned body. |
| `flag` | Forward the request unchanged; only emit a log line and a metric. |

## Check groups

`checks` selects which detector groups run. Country groups are **strictly opt-in**.

| Group | Detects |
|---|---|
| `pii` | email, credit card, IBAN, IPv4 address, international (E.164) phone |
| `pii_fr` | French phone, NIR (social security), SIREN, SIRET |
| `pii_us` | US Social Security Number |
| `pii_uk` | UK National Insurance Number |
| `pii_es` | Spanish DNI / NIF |
| `pii_it` | Italian Codice Fiscale |
| `secrets` | AWS access keys, private-key blocks, JWTs, GitHub / Slack / Google tokens |

## How it works

The scanner reads the `messages` array of the OpenAI-compatible JSON body and inspects all text content:

- `messages[*].content` as a string (standard chat payload)
- `messages[*].content[*].text` as content parts (multimodal payloads)

For `redact`, matches are replaced inside that content and the body is re-serialised as valid JSON; all other fields are preserved. Placeholders are not re-matched, so redaction is idempotent.

## HTTP response (block)

```
HTTP/1.1 422 Unprocessable Entity
Content-Type: application/json

{"error": "guardrails violation: email, aws_access_key"}
```

## Metrics

```
gatewai_guardrails_total{service_type, model, stage, action, result}         # stage = input | output; result = blocked | redacted | flagged
gatewai_guardrails_async_total{service_type, model, detector, result}        # async result stage (guardrails.async); result = flagged | blocked | redacted | error
gatewai_guardrails_pii_blocked_total{service_type, model}                    # retained for continuity (block only)
gatewai_guardrails_evaluations_total{service_type, model, stage}             # every evaluation, regardless of outcome — denominator for a hit rate
```

## False positives

:::caution
Purely numeric national-ID patterns — `siren` (9 digits), `siret` (14 digits), `fr_nir`, `us_ssn`, `es_dni` — have materially higher false-positive rates than the others. Enable the relevant country group per service only after reviewing a representative sample of your payloads, and consider `flag` before `block` or `redact`.
:::
