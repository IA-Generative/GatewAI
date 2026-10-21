# GatewAI

API Gateway pour les services d'inférence KServe sur Kubernetes — file async Redis, proxy sync compatible OpenAI, et proxy LLM dans un seul binaire.

| Mode | Endpoint | Quand l'utiliser |
|---|---|---|
| **Async** | `POST /jobs/{service_type}` | Fichiers lourds, traitements longs (>30s), webhook |
| **Sync** | `POST /v1/*` | SDK OpenAI, services faible latence |
| **LLM proxy** | `POST /v1/*` (JSON + `provider`) | OpenAI / Anthropic / Ollama / vLLM avec cache et métriques |

## Architecture

![Architecture overview](docs/schema/overview.png)

## Fonctionnalités

- Registre de services piloté par la config — ajouter un modèle avec un bloc YAML, sans modifier le code
- Hot-reload via `POST /-/reload`
- Routage prioritaire (Redis `LPUSH`)
- Suivi des consommateurs via un header configurable
- Proxy LLM — OpenAI / Anthropic / Ollama / passthrough (vLLM), traduction de provider
- Cache des réponses (Redis, clé SHA-256, header `X-Cache`)
- Routage multi-backend — blue/green, canary, fallback avec sélection pondérée
- Pools de backends — partager l'auth, le rate limit et le budget de concurrence d'un même backend physique entre plusieurs alias service/modèle
- Rate limiting par consommateur — fenêtre fixe, par type de service et type d'utilisateur, limites de requêtes et de tokens
- Guardrails PII + secrets — blocage / redaction / signalement des requêtes LLM contenant des données personnelles (email, téléphone, IBAN, carte bancaire, SIREN/SIRET, NIR…) ou des secrets (clés AWS, JWT, tokens GitHub) ; DLP optionnel sur les réponses du modèle
- Authentification OAuth2 — validation côté gateway des access tokens (JWT via JWKS + opaques via introspection RFC 7662), fail-closed
- Contrôle d'accès — liste blanche par politique, default-deny par modèle/service, quotas rate/token par groupe
- Observabilité OpenTelemetry — tracing distribué, push OTLP (traces/métriques/logs), trace_id dans les logs structurés et les exemplars
- Audit trail — log slog structuré par requête LLM (opt-in)
- Chiffrement AES-256-GCM at-rest pour les objets S3
- Autoscaling KEDA — le relay scale sur la profondeur de la file Redis, scale-to-zero
- Métriques Prometheus — requêtes, latence, tokens, cache, rate limits, top-N consommateurs
- Suivi d'usage par consommateur — `GET /usage` / `GET /-/usage`, compteurs cumulés et par fenêtre pour tous les types de service

## Documentation

Référence de configuration, guide de déploiement, référence API et runbooks :
**[ia-generative.github.io/GatewAI](https://ia-generative.github.io/GatewAI)**
