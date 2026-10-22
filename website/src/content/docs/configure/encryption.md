---
title: Encryption
---

# Encryption

GatewAI supports transparent AES-256-GCM encryption of job files in S3. When enabled, the gateway encrypts the input file before upload and the relay decrypts it after download — the inference backend always receives plaintext.

## How it works

```
Client
  │  POST /jobs/{service_type} (plaintext file)
  ▼
Gateway
  ├── Encrypt file (AES-256-GCM streaming)
  └── Upload ciphertext → S3

                    Relay
                      ├── Download ciphertext from S3
                      ├── Decrypt (AES-256-GCM streaming)
                      └── POST plaintext → inference backend
```

Both the uploaded input and the result file written back to S3 by the relay are **encrypted** when an encryption key is configured — the relay encrypts on upload, same as the gateway.

## Algorithm

**AES-256-GCM**, streaming over 64 KB plaintext chunks. Each chunk is independently authenticated and sealed so the relay can decrypt without buffering the entire file in memory.

**Wire format:**

```
[8-byte random nonce prefix]
[chunk 0: ciphertext (64 KB + 16-byte GCM tag)]
[chunk 1: ciphertext]
...
[chunk N: ciphertext (≤ 64 KB + 16-byte GCM tag)]
```

The 12-byte GCM nonce for chunk `i` is:

```
nonce_prefix (8 bytes) || big_endian_uint32(i) (4 bytes)
```

An empty plaintext produces one empty chunk (16-byte GCM tag only).

## Configuration

### Generate a key

```bash
openssl rand -hex 32
```

This produces a 64-character hex string representing 32 random bytes.

### Gateway config

```yaml
encryption:
  key: "${ENCRYPTION_KEY}"
```

The key is read from the `ENCRYPTION_KEY` environment variable. Leave empty to disable encryption.

### Relay config

```yaml
encryption:
  key: "${ENCRYPTION_KEY}"
```

**The same key must be set in every relay sidecar.** A mismatch causes decryption failures and failed jobs.

### Helm

Two options for providing the key:

```yaml
# Option A — let the chart create a Secret from a literal value
encryption:
  key: "your-64-char-hex-key"

# Option B — reference an existing Secret (e.g. from External Secrets Operator)
encryption:
  existingSecret: "my-encryption-secret"   # must contain key ENCRYPTION_KEY
```

Leave both empty to disable encryption.

## Disabling encryption

Set `encryption.key` to an empty string (or omit the field) in both gateway and relay configs. Existing encrypted objects in S3 cannot be read once the key is removed — drain or purge the queue before disabling.
