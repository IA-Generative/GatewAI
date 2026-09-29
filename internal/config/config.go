package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"gatewai/gateway/internal/telemetry"
)

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	S3         S3Config         `yaml:"s3"`
	Redis      RedisConfig      `yaml:"redis"`
	Lifecycle  LifecycleConfig  `yaml:"lifecycle"`
	Services   []ServiceConfig  `yaml:"services"`
	Encryption EncryptionConfig `yaml:"encryption"`
	Metrics    MetricsConfig    `yaml:"metrics"`
	AuditLog   AuditLogConfig   `yaml:"audit_log"`
	Health     HealthConfig     `yaml:"health"`
	// RateLimits maps service type → user type → limit.
	// User type "*" is the fallback applied when the user_type_header is absent
	// or the specific type has no entry.
	// Leave empty to disable rate limiting.
	RateLimits map[string]map[string]RateLimitConfig `yaml:"rate_limits"`
	Otel       telemetry.OtelConfig                  `yaml:"opentelemetry"`
	Auth       AuthConfig                            `yaml:"auth"`
	// Policies configures identity-based access control. Nil means no enforcement.
	Policies *PoliciesConfig `yaml:"policies"`
	// BackendPools defines named, reusable groups of inference backends, keyed
	// by pool name. Referenced from services[].backend_pool. Lets several
	// services (e.g. different model aliases) share one physical backend's
	// rate limit / concurrency budget instead of duplicating it per service.
	BackendPools map[string]BackendPoolConfig `yaml:"backend_pools"`
	Usage        UsageConfig                  `yaml:"usage"`
	Webhooks     WebhookConfig                `yaml:"webhooks"`
	Jobs         JobsConfig                   `yaml:"jobs"`
	// CircuitBreaker guards LLM-proxy backends: after a run of consecutive
	// failures a backend's circuit opens and it is skipped until a cooldown,
	// so a dead backend is not hammered on every request. Opt-in (default off).
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// CircuitBreakerConfig tunes the per-backend circuit breaker for the LLM proxy.
type CircuitBreakerConfig struct {
	// Enabled activates the breaker. Default false.
	Enabled bool `yaml:"enabled"`
	// FailureThreshold is the number of consecutive failures (network error or
	// 5xx) that opens a backend's circuit. A single success resets the count.
	// Default 5.
	FailureThreshold int `yaml:"failure_threshold"`
	// Cooldown is how long a circuit stays open before a half-open probe is
	// allowed through. Default "30s".
	Cooldown string `yaml:"cooldown"`
	// ProbeInterval, when set (e.g. "10s"), runs an active per-replica health
	// probe against each LLM backend's health path, feeding the breaker so an
	// idle/dead backend opens and a recovered one closes without waiting for live
	// request traffic. Empty/"0s" = passive only (default). Uses each service's
	// health.path/health.timeout; backends with health.disabled are skipped.
	ProbeInterval string `yaml:"probe_interval"`
}

// WebhookConfig tunes durable outbound webhook delivery. Retries are persisted
// in Redis (ZSET webhook:retries + per-job task keys) so a gateway restart does
// not drop pending retries; final failures are dead-lettered to webhook:deadletter.
type WebhookConfig struct {
	// MaxRetries is the total number of delivery attempts (including the first,
	// inline one) before a webhook is dead-lettered. 0 or absent = 3.
	MaxRetries int `yaml:"max_retries"`
	// RetryBackoff is the base delay before the first retry; each subsequent
	// retry doubles it, capped by MaxBackoff. Default "30s".
	RetryBackoff string `yaml:"retry_backoff"`
	// MaxBackoff caps the exponential backoff. Default "10m".
	MaxBackoff string `yaml:"max_backoff"`
	// SigningSecret, when set, signs every outbound webhook with an HMAC-SHA256
	// header `X-Gatewai-Signature: t=<unix>,v1=<hex>` computed over "<t>.<body>",
	// letting consumers verify authenticity and reject replays (stale t).
	// Supports ${VAR} expansion. Empty = unsigned (backward compatible).
	SigningSecret string `yaml:"signing_secret"`
}

// MaxRetriesOrDefault returns the configured attempt cap, defaulting to 3.
func (w WebhookConfig) MaxRetriesOrDefault() int {
	if w.MaxRetries > 0 {
		return w.MaxRetries
	}
	return 3
}

// RetryBackoffDuration returns the base retry backoff, defaulting to 30s.
func (w WebhookConfig) RetryBackoffDuration() time.Duration {
	if d := parseDuration(w.RetryBackoff); d > 0 {
		return d
	}
	return 30 * time.Second
}

// MaxBackoffDuration returns the backoff cap, defaulting to 10m.
func (w WebhookConfig) MaxBackoffDuration() time.Duration {
	if d := parseDuration(w.MaxBackoff); d > 0 {
		return d
	}
	return 10 * time.Minute
}

// JobsConfig tunes async job submission behaviour.
type JobsConfig struct {
	// IdempotencyTTL is how long an Idempotency-Key → job mapping is remembered,
	// so a client retry with the same key returns the original job instead of
	// starting a duplicate inference. 0 or absent = 24h.
	IdempotencyTTL string `yaml:"idempotency_ttl"`
	// ExpiredMarkerTTL is how long a lightweight "this job existed" tombstone is
	// kept after submission. While it lives, polling a job whose record TTL has
	// passed returns 410 (status "expired") instead of 404. Should exceed the
	// job record TTL. 0 or absent = 168h (7 days).
	ExpiredMarkerTTL string `yaml:"expired_marker_ttl"`
}

// ExpiredMarkerTTLDuration returns the job-existence tombstone TTL, default 168h.
func (j JobsConfig) ExpiredMarkerTTLDuration() time.Duration {
	if d := parseDuration(j.ExpiredMarkerTTL); d > 0 {
		return d
	}
	return 168 * time.Hour
}

// IdempotencyTTLDuration returns the idempotency-key retention, defaulting to 24h.
func (j JobsConfig) IdempotencyTTLDuration() time.Duration {
	if d := parseDuration(j.IdempotencyTTL); d > 0 {
		return d
	}
	return 24 * time.Hour
}

// PoliciesConfig controls which principals may access which services and models.
// Default posture is deny-all; rules grant access explicitly.
type PoliciesConfig struct {
	// Default is the baseline decision when no rule matches.
	// Valid values: "" (treated as "deny"), "deny", "allow".
	// "allow" disables enforcement entirely (useful for gradual rollout).
	Default string       `yaml:"default"`
	Rules   []PolicyRule `yaml:"rules"`
}

// PolicyRule grants access to a set of models/service types for principals
// whose identity satisfies Match. ALL specified Match fields must pass (AND).
type PolicyRule struct {
	Match PolicyMatch `yaml:"match"`
	// AllowModels is a list of glob patterns matched against the model alias.
	// Empty means the rule grants nothing (must list "*" to allow all models).
	AllowModels []string `yaml:"allow_models"`
	// AllowServiceTypes is a list of glob patterns matched against the service type.
	// Empty means no service-type constraint (the model match alone is sufficient).
	AllowServiceTypes []string `yaml:"allow_service_types"`
	// Limits is an optional rate/token budget applied per-consumer when this rule
	// grants access. Nil means no additional policy-level limiting.
	// Use Rate+Period for request-count limiting and TokenRate+TokenPeriod for
	// token budget limiting; both can be set simultaneously.
	Limits *RateLimitConfig `yaml:"limits"`
}

// PolicyMatch defines the principal attributes required for a rule to fire.
// Fields are ANDed: all non-empty fields must match.
// An empty PolicyMatch{} matches every principal (including nil/anonymous).
type PolicyMatch struct {
	// Groups: principal must belong to at least one of these groups.
	Groups []string `yaml:"groups"`
	// Roles: principal must hold at least one of these roles.
	Roles []string `yaml:"roles"`
	// Scopes: principal must have at least one of these OAuth2 scopes.
	Scopes []string `yaml:"scopes"`
	// Consumers: principal.Consumer must be one of these values.
	Consumers []string `yaml:"consumers"`
	// UserTypes: principal.UserType must be one of these values.
	UserTypes []string `yaml:"user_types"`
}

// AuthConfig selects and configures the authentication mode.
// Mode "" (empty) is legacy/none — fully backward compatible.
// Mode "oauth2" validates JWT Bearer tokens via JWKS.
// Mode "proxy" trusts identity headers injected by an upstream proxy.
type AuthConfig struct {
	Mode   string           `yaml:"mode"` // "" | "oauth2" | "proxy"
	OAuth2 OAuth2AuthConfig `yaml:"oauth2"`
	Proxy  ProxyAuthConfig  `yaml:"proxy"`
}

// OAuth2AuthConfig holds configuration for OAuth2 JWT validation.
type OAuth2AuthConfig struct {
	Issuer        string                   `yaml:"issuer"`
	JWKSURL       string                   `yaml:"jwks_url"`
	Audiences     []string                 `yaml:"audiences"`
	Claims        ClaimMapConfig           `yaml:"claims"`
	Validation    string                   `yaml:"validation"` // "" | "auto" | "jwt" | "introspection"
	Introspection *IntrospectionAuthConfig `yaml:"introspection"`
}

// IntrospectionAuthConfig holds configuration for RFC 7662 token introspection.
type IntrospectionAuthConfig struct {
	Endpoint     string `yaml:"endpoint"` // optional; else discovered via introspection_endpoint
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	CacheTTL     string `yaml:"cache_ttl"` // e.g. "60s"; caps how long an introspection result is cached
}

// ClaimMapConfig maps Principal fields to JWT claim names.
type ClaimMapConfig struct {
	Subject  string `yaml:"subject"`
	Consumer string `yaml:"consumer"`
	Scopes   string `yaml:"scopes"`
	Groups   string `yaml:"groups"`
	Roles    string `yaml:"roles"`
}

// ProxyAuthConfig holds the header names for proxy-injected identity.
type ProxyAuthConfig struct {
	ConsumerHeader string `yaml:"consumer_header"`
	UserTypeHeader string `yaml:"user_type_header"`
	GroupsHeader   string `yaml:"groups_header"`
	RolesHeader    string `yaml:"roles_header"`
	ScopesHeader   string `yaml:"scopes_header"`
}

// HealthConfig holds global defaults for backend health probing (GET /health?verbose=true).
type HealthConfig struct {
	// Timeout is the default HTTP timeout for backend health probes. Default: 5s.
	// For models that scale to 0 (Knative), set this higher at the service level.
	Timeout string `yaml:"timeout"`
	// Interval controls how often the background health batch runs. Default: 30s.
	// The /health endpoint always reads the latest cached result — no live probing.
	Interval string `yaml:"interval"`
}

func (h HealthConfig) TimeoutDuration() time.Duration  { return parseDuration(h.Timeout) }
func (h HealthConfig) IntervalDuration() time.Duration { return parseDuration(h.Interval) }

// ServiceHealthConfig controls how the gateway probes one service's backend.
type ServiceHealthConfig struct {
	// Disabled skips health probing for this service. Default: false.
	Disabled bool `yaml:"disabled"`
	// Timeout overrides the global health.timeout for this service.
	// Especially useful for models that scale to 0 (Knative cold start).
	// Example: "30s" for a model behind a scale-to-zero deployment.
	Timeout string `yaml:"timeout"`
	// Path is the HTTP path probed on the backend. Default: "/health".
	Path string `yaml:"path"`
	// Method is the HTTP method used for health probes. Default: "GET".
	// Use "HEAD" for backends that return no body but support HEAD on their health endpoint.
	Method string `yaml:"method"`
}

func (h ServiceHealthConfig) TimeoutDuration() time.Duration { return parseDuration(h.Timeout) }

// AuditLogConfig controls structured per-request audit logging for LLM requests.
// Disabled by default to avoid unexpected log volume.
type AuditLogConfig struct {
	// Enabled activates a structured slog record for every LLM request.
	Enabled bool `yaml:"enabled"`
	// Prompt includes the raw request body in the log entry when true.
	// Opt-in only — the body may contain PII.
	Prompt bool `yaml:"prompt"`
}

// RateLimitConfig defines the allowed rate for a (service, user-type) pair.
// Set Rate+Period for request-count limiting, TokenRate+TokenPeriod for token
// budget limiting. Both can coexist; either can be omitted (0 = disabled).
type RateLimitConfig struct {
	Rate        int    `yaml:"rate"`         // max requests per Period (0 = disabled)
	Period      string `yaml:"period"`       // e.g. "1m", "1h", "24h"
	TokenRate   int    `yaml:"token_rate"`   // max total tokens per TokenPeriod (0 = disabled)
	TokenPeriod string `yaml:"token_period"` // e.g. "1h", "24h"
	// MaxConcurrent caps the number of async jobs in pending+processing state
	// per consumer per service type. 0 = disabled.
	MaxConcurrent int `yaml:"max_concurrent"`
	// ProcessingTime caps total inference processing seconds per consumer per
	// service type within ProcessingPeriod. 0 = disabled.
	ProcessingTime   int    `yaml:"processing_time"`
	ProcessingPeriod string `yaml:"processing_period"` // e.g. "1h", "24h"
}

// MetricsConfig controls optional high-cardinality metric features.
type MetricsConfig struct {
	// TopConsumers sets how many top consumers to expose in Prometheus via a
	// Redis sorted-set backed GaugeVec, refreshed every 60s. 0 = disabled.
	TopConsumers int `yaml:"top_consumers"`
	// ConsumerLabels enables direct per-consumer Prometheus labels.
	// Only suitable for deployments with a small, bounded number of consumers
	// (< 50). Disabled by default — use TopConsumers for large deployments.
	ConsumerLabels bool `yaml:"consumer_labels"`
}

type EncryptionConfig struct {
	// Key is a hex-encoded 32-byte AES-256 key. Empty = encryption disabled.
	Key string `yaml:"key"`
}

type ServerConfig struct {
	Addr         string        `yaml:"addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
	// PriorityHeader is the HTTP header injected by APISIX on SA consumer
	// requests to signal high-priority processing. When a request carries this
	// header, the job mode is set to "async-priority" for relay-side prioritisation.
	PriorityHeader string `yaml:"priority_header"`
	// ConsumerHeader is the HTTP header used to identify the API consumer.
	// Typically set by APISIX after authentication (e.g. "X-Consumer-Username").
	// When configured:
	//   - the consumer name is stored in the job record and tracked in a Redis
	//     sorted set for listing via GET /jobs
	//   - gatewai_jobs_by_consumer_total metric is incremented per consumer
	//   - GET /jobs/{service_type}/{id} enforces ownership: if the header is
	//     present, the job's consumer_name must match or 404 is returned
	// Leave empty in deployments without upstream authentication.
	ConsumerHeader string `yaml:"consumer_header"`
	// UserTypeHeader is the HTTP header carrying the consumer type (e.g. "sa" or
	// "user"), typically set by the APISIX Lua plugin after token introspection
	// (e.g. "X-User-Type"). Used for rate limiting (matched against
	// rate_limits[type][user_type]) and for LLM consumer metric labelling.
	// "*" is the fallback when the header is absent. Leave empty to disable
	// per-type differentiation.
	UserTypeHeader string `yaml:"user_type_header"`
	// MaxBodyMB caps the request body size (in MiB) on the sync JSON path
	// (POST /v1/*). This path carries base64-embedded images for vision models,
	// which can exceed the default. Requests larger than the cap are rejected
	// with 413 Request Entity Too Large (not silently truncated).
	// 0 or absent = 1 MiB default. Does not affect the multipart file-upload
	// path, which is bounded per-service by max_file_size_mb.
	MaxBodyMB int `yaml:"max_body_mb"`
}

// S3Config holds S3-compatible object storage credentials and settings.
type S3Config struct {
	Endpoint  string `yaml:"endpoint"` // e.g. https://s3.fr-par.scw.cloud
	Region    string `yaml:"region"`   // e.g. fr-par
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
	// CABundle is an optional path to a PEM file containing one or more CA
	// certificates used to verify the S3 endpoint's TLS certificate.
	// Required when the endpoint uses a private or self-signed CA.
	// Empty (default) uses the system certificate pool.
	CABundle string `yaml:"ca_bundle"`
	// UsePathStyle forces path-style URLs (https://endpoint/bucket/key) instead
	// of virtual-hosted style (https://bucket.endpoint/key).
	// Required for most S3-compatible providers. Default: true.
	UsePathStyle bool `yaml:"use_path_style"`
	// SSLInsecure disables TLS certificate verification for the S3 endpoint.
	// Use only in development or when the endpoint uses a self-signed cert
	// and ca_bundle is not available.
	SSLInsecure bool `yaml:"ssl_insecure"`
}

type RedisConfig struct {
	Addr          string `yaml:"addr"`
	Password      string `yaml:"password"`
	DB            int    `yaml:"db"`
	PendingMaxAge string `yaml:"pending_max_age"` // duration string, e.g. "2h"; empty = disabled
}

func (r RedisConfig) PendingMaxAgeDuration() time.Duration { return parseDuration(r.PendingMaxAge) }

// LifecycleConfig controls job record and S3 result retention.
type LifecycleConfig struct {
	// PersistsResult controls whether S3 results and Redis records are kept after
	// the job result is first consumed (GET /jobs/{type}/{id} or webhook delivery).
	// false (default): cleanup is immediate on first consumption.
	// true: records persist until their TTL expires naturally.
	PersistsResult bool         `yaml:"persists_result"`
	JobTTL         JobTTLConfig `yaml:"job_ttl"`
	GC             GCConfig     `yaml:"gc"`
}

// JobTTLConfig holds per-status TTLs for Redis job records.
// Per-status values take precedence over Global; 0 means no TTL configured.
// When all values are 0, an internal 2h safety net applies for orphaned records.
type JobTTLConfig struct {
	Global    string `yaml:"global"`    // fallback for all statuses
	Completed string `yaml:"completed"` // override for completed jobs
	Pending   string `yaml:"pending"`   // override for pending/processing jobs
	Failed    string `yaml:"failed"`    // override for failed jobs
	Cancelled string `yaml:"cancelled"` // override for cancelled jobs
}

func parseDuration(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return 0
}

func (j JobTTLConfig) GlobalDuration() time.Duration    { return parseDuration(j.Global) }
func (j JobTTLConfig) PendingDuration() time.Duration   { return parseDuration(j.Pending) }
func (j JobTTLConfig) CompletedDuration() time.Duration { return parseDuration(j.Completed) }
func (j JobTTLConfig) FailedDuration() time.Duration    { return parseDuration(j.Failed) }
func (j JobTTLConfig) CancelledDuration() time.Duration { return parseDuration(j.Cancelled) }

// GCConfig controls the unified background garbage collector.
type GCConfig struct {
	Enabled      bool   `yaml:"enabled"`        // master switch; default false
	Interval     string `yaml:"interval"`       // tick frequency; default "15m"
	OrphanMinAge string `yaml:"orphan_min_age"` // min S3 object age before orphan check; default "5m"
	// MaxReapAttempts caps how many times the processing-queue reaper requeues an
	// abandoned job (relay pod died, lease expired) before dead-lettering it to
	// relay:{model}:deadletter and marking it failed. 0 or absent = 3.
	MaxReapAttempts int `yaml:"max_reap_attempts"`
}

// MaxReapAttemptsOrDefault returns the configured requeue cap, defaulting to 3.
func (g GCConfig) MaxReapAttemptsOrDefault() int {
	if g.MaxReapAttempts > 0 {
		return g.MaxReapAttempts
	}
	return 3
}

func (g GCConfig) IntervalDuration() time.Duration     { return parseDuration(g.Interval) }
func (g GCConfig) OrphanMinAgeDuration() time.Duration { return parseDuration(g.OrphanMinAge) }

// UsageConfig controls per-consumer usage tracking retention.
type UsageConfig struct {
	// Retention is the duration sorted-set keys are kept before expiry.
	// Empty or "0" means no TTL (all-time accumulation).
	// Accepts Go duration strings: "24h", "720h", "8760h".
	// Note: "d" suffix is not a valid Go duration — use "h" (e.g. "8760h" for 365 days).
	Retention string `yaml:"retention"`
}

func (u UsageConfig) RetentionDuration() time.Duration { return parseDuration(u.Retention) }

// BackendConfig describes one backend in a multi-backend list.
type BackendConfig struct {
	URL     string            `yaml:"url"`
	Weight  int               `yaml:"weight"`  // relative weight; 0 = fallback-only
	Headers map[string]string `yaml:"headers"` // headers added to every request to this backend; override service-level inference_headers
	Model   string            `yaml:"model"`   // real model name sent to this backend; overrides service-level backend_model
}

// BackendPoolMember is one backend within a named pool, with optional
// per-member rate limit / concurrency caps layered on top of the pool-wide
// budget (both must pass).
type BackendPoolMember struct {
	BackendConfig `yaml:",inline"`
	// RateLimit caps this member's own request rate. Only Rate+Period are
	// meaningful at this scope; other RateLimitConfig fields are rejected by
	// validation. 0/absent = no member-level cap (pool-wide cap still applies).
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	// MaxConcurrent caps simultaneous in-flight requests to this member.
	// 0 = no member-level cap (pool-wide cap still applies).
	MaxConcurrent int `yaml:"max_concurrent"`
}

// BackendPoolConfig is a named, reusable group of inference backends,
// referenced by services[].backend_pool. Members share the pool's headers
// (lowest precedence, overridden by member headers) and, optionally, a
// pool-wide rate limit / concurrency budget on top of any per-member caps.
type BackendPoolConfig struct {
	Members []BackendPoolMember `yaml:"members"`
	// Headers are default headers applied to every member; overridden by a
	// member's own headers with the same name, and by service-level
	// inference_headers on the sync-direct path.
	Headers map[string]string `yaml:"headers"`
	// RateLimit caps the combined request rate across all members of the
	// pool. Only Rate+Period are meaningful at this scope. 0/absent = no
	// pool-wide cap.
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	// MaxConcurrent caps the combined in-flight requests across all members
	// of the pool. 0 = no pool-wide cap.
	MaxConcurrent int `yaml:"max_concurrent"`
	// PriorityReservedConcurrent reserves this many of MaxConcurrent's slots
	// exclusively for requests carrying server.priority_header, mirroring
	// services[].priority_reserved_sync. 0 (default) = no reservation.
	PriorityReservedConcurrent int `yaml:"priority_reserved_concurrent"`
}

// ServiceConfig declares a single inference service type.
// New services are added here (config.yaml) — no Go code required.
type ServiceConfig struct {
	Type string `yaml:"type"`
	// Sync / OpenAI-compatible mode (optional).
	// Model is the value of the "model" field in the OpenAI payload used to
	// route the request to the correct InferenceService backend.
	Model string `yaml:"model"`
	// Default marks this model as the default for its service type.
	// Used when a request omits the model field and multiple models are configured.
	// At most one entry per type should be marked as default.
	Default bool `yaml:"default"`
	// Operations maps operation names to their URL paths.
	// e.g. {"transcription": ["/v1/audio/transcriptions"], "translation": ["/v1/audio/translations"]}
	// Multiple paths per operation are all indexed for sync routing; the first is used for async.
	Operations   map[string][]string `yaml:"operations"`
	InferenceURL string              `yaml:"inference_url"` // InferenceService cluster URL (single backend, legacy)
	// Backends is a list of inference backends for this service. When set, takes
	// precedence over inference_url and enables blue/green, canary, and fallback routing.
	// weight > 0 = eligible for primary selection (weighted random).
	// weight = 0 = fallback-only (tried only if all weight>0 backends fail).
	Backends []BackendConfig `yaml:"backends"`
	// BackendPool references a named pool from the top-level backend_pools
	// block instead of declaring backends inline. Mutually exclusive with
	// both backends and inference_url.
	BackendPool   string   `yaml:"backend_pool"`
	AcceptedExts  []string `yaml:"accepted_exts"`
	MaxFileSizeMB int64    `yaml:"max_file_size_mb"`
	// MaxConcurrentSync limits the number of simultaneous sync proxy calls for this model.
	// 0 (default) means no limit. When exceeded, the handler returns 503.
	MaxConcurrentSync int `yaml:"max_concurrent_sync"`
	// PriorityReservedSync reserves this many of MaxConcurrentSync's slots
	// exclusively for requests carrying server.priority_header. The remainder
	// is the shared pool used by everyone (priority requests fall back to it
	// too, once their reserved slots are full). 0 (default) = no reservation;
	// priority requests compete in the shared pool like everyone else.
	PriorityReservedSync int `yaml:"priority_reserved_sync"`
	// SwaggerURL is an optional URL to an OpenAPI JSON spec for this service.
	// Fetched once at startup; served at GET /swagger/{type}/{model}.
	// Failures are logged as warnings and do not block startup.
	SwaggerURL string `yaml:"swagger_url"`
	// SwaggerHeaders are optional HTTP headers sent when fetching SwaggerURL.
	// Values support ${VAR} env expansion (same as the rest of config.yaml).
	// Example for a private GitHub release asset:
	//   swagger_headers:
	//     Accept: application/octet-stream
	//     Authorization: "Bearer ${GITHUB_TOKEN}"
	SwaggerHeaders map[string]string `yaml:"swagger_headers"`
	// InferenceHeaders are optional HTTP headers injected on every request
	// forwarded to the inference backend (sync-direct proxy only).
	// Values support ${VAR} env expansion (same as the rest of config.yaml).
	// These headers override anything sent by the client with the same name.
	// Example:
	//   inference_headers:
	//     Authorization: "Bearer ${RERANKER_API_KEY}"
	//     X-Api-Key: "${BACKEND_KEY}"
	InferenceHeaders map[string]string `yaml:"inference_headers"`
	// BackendModel is the real model name sent to the backend. Allows clients to use
	// a short alias (model field) while the gateway rewrites it to the backend's
	// expected identifier (e.g. "meta-llama/Meta-Llama-3-8B-Instruct" for vLLM).
	// Only applied when Provider is set. Empty means the alias is forwarded as-is.
	BackendModel string `yaml:"backend_model"`
	// FallbackModel is another model (on the same sync path) to route to when this
	// model's backends are all circuit-open (requires circuit_breaker.enabled).
	// Opt-in; empty = no fallback. The caller must also be allowed to use it
	// (visibility/policies are re-checked against the fallback).
	FallbackModel string `yaml:"fallback_model"`
	// Provider selects the LLM backend protocol. When set, JSON requests are routed
	// through the LLM proxy handler instead of the bare direct proxy.
	// Valid values: "openai", "anthropic", "ollama", "passthrough". Empty = legacy direct proxy.
	Provider string `yaml:"provider"`
	// ResponseCacheTTL is the TTL in seconds for caching LLM responses in Redis.
	// 0 = caching disabled. Only applied when Provider is set.
	ResponseCacheTTL int `yaml:"response_cache_ttl"`
	// Retries is the number of additional full backend-cycle attempts when all
	// backends return a network error or a 5xx response. 0 = no retry (default).
	// Only applies to the sync-direct proxy path (not async relay flows).
	// A 500ms exponential backoff is applied between each cycle.
	Retries    int              `yaml:"retries"`
	Guardrails GuardrailsConfig `yaml:"guardrails"`
	// TokenLimits defines per-user-type token budgets for this model's LLM requests.
	// Maps user type → RateLimitConfig (only token_rate and token_period are used).
	// "*" acts as fallback when the user_type_header is absent or unmatched.
	// Applied independently from rate_limits; both can reject the same request.
	TokenLimits map[string]RateLimitConfig `yaml:"token_limits"`
	// Health controls backend probing for this service (GET /health?verbose=true).
	Health ServiceHealthConfig `yaml:"health"`
	// Deprecated marks this model as deprecated: surfaced in GET /v1/models
	// (capabilities.deprecated) and as `deprecated: true` on the corresponding
	// operations in the generated OpenAPI spec (per-model swagger docs always;
	// shared sync paths only when every model on that path is deprecated).
	// Purely informational — does not affect routing or availability.
	Deprecated bool `yaml:"deprecated"`
	// Visibility optionally restricts this model to specific audiences. When either
	// list is non-empty the model is hidden from GET /v1/models for callers who
	// don't match, and routing to it (sync or async) returns 404. Empty/absent =
	// public (visible to everyone). Enables beta-testing a model on the same API.
	// Requires auth.mode (or server.user_type_header) so the caller can be identified.
	Visibility VisibilityConfig `yaml:"visibility"`
	// Realtime configures a bidirectional WebSocket streaming service (e.g. real-time
	// speech-to-text). When set, the gateway proxies a WebSocket connection to the
	// backend instead of a request/response call, applying auth/visibility/authz at
	// the handshake and (later stages) session quotas, usage metering, and transcript
	// guardrails. Mutually exclusive with the request/response fields' semantics —
	// a realtime service is routed as a WS GET on Path, not a sync POST.
	Realtime *RealtimeConfig `yaml:"realtime"`
}

// RealtimeConfig configures a WebSocket streaming service (real-time transcription).
type RealtimeConfig struct {
	// Path is the client-facing WebSocket route (e.g. "/v1/audio/stream").
	Path string `yaml:"path"`
	// BackendPath is the path dialed on the backend. Empty = same as Path.
	BackendPath string `yaml:"backend_path"`
	// Audio describes the wire format of the client's binary frames, used to meter
	// streamed audio-seconds for quotas. Defaults suit 16 kHz s16le mono PCM.
	Audio RealtimeAudioConfig `yaml:"audio"`
	// MaxSessionSeconds bounds a single connection's wall-clock lifetime (0 = no cap).
	MaxSessionSeconds int `yaml:"max_session_seconds"`
	// MaxAudioSeconds bounds the total audio one connection may stream (0 = no cap).
	MaxAudioSeconds int `yaml:"max_audio_seconds"`
	// MaxConcurrentPerConsumer caps simultaneous realtime sessions per consumer
	// (0 = no cap). Enforced at the handshake.
	MaxConcurrentPerConsumer int `yaml:"max_concurrent_per_consumer"`
	// InfoPaths are plain HTTP GET discovery endpoints on the backend (e.g.
	// "/v1/config", "/v1/languages") proxied through the gateway so clients can
	// query the audio contract / supported languages behind the same policy layer.
	InfoPaths []string `yaml:"info_paths"`
}

// RealtimeAudioConfig describes the client's binary audio frame format.
type RealtimeAudioConfig struct {
	SampleRate     int `yaml:"sample_rate"`      // samples per second (default 16000)
	BytesPerSample int `yaml:"bytes_per_sample"` // bytes per sample (default 2 = s16le)
	Channels       int `yaml:"channels"`         // channel count (default 1 = mono)
}

// VisibilityConfig gates a model to specific user types and/or groups.
// A caller sees (and may use) the model if their user type is in UserTypes OR
// they belong to a group in Groups. Both empty = public.
type VisibilityConfig struct {
	UserTypes []string `yaml:"user_types"`
	Groups    []string `yaml:"groups"`
}

// GuardrailsStageConfig controls PII/secrets detection for one stage (input or output).
type GuardrailsStageConfig struct {
	// Action applied when a check matches: "block" (default), "redact", or "flag".
	Action string `yaml:"action"`
	// Checks selects which guardrail groups to run:
	// pii, pii_fr, pii_us, pii_uk, pii_es, pii_it, secrets.
	Checks []string `yaml:"checks"`
	// Streaming controls how the output stage acts on STREAMING responses:
	//   "flag"   (default) — observe only; content streams through unbuffered.
	//   "block"  — buffer a window, and terminate the stream on a violation.
	//   "buffer" — buffer a window, redact matches, then release it.
	// block/buffer trade first-token latency for enforcement — opt-in per service.
	Streaming string `yaml:"streaming"`
	// StreamWindowTokens is the buffer size (in ~tokens) held before a window is
	// scanned and released, for Streaming block/buffer. Larger = better detection
	// (a match is less likely to straddle a boundary) but higher latency to first
	// visible token. 0 = default (64).
	StreamWindowTokens int `yaml:"stream_window_tokens"`
}

// GuardrailsConfig controls PII/secrets detection for a service's LLM requests.
// Configure via Action and Checks (top-level, applied to input) or via the
// Input/Output sub-keys for stage-specific configuration.
type GuardrailsConfig struct {
	// Action applied when a check matches: "block" (default), "redact", or "flag".
	Action string `yaml:"action"`
	// Checks selects which guardrail groups to run:
	// pii, pii_fr, pii_us, pii_uk, pii_es, pii_it, secrets.
	Checks []string `yaml:"checks"`
	// Models configures model-backed detectors run alongside the regex checks on
	// the input (request) stage. Empty = regex only.
	Models []GuardrailModelConfig `yaml:"models"`
	// Output configures output-stage DLP guardrails (applied to LLM responses).
	// When nil, output guardrails are disabled.
	Output *GuardrailsStageConfig `yaml:"output"`
	// Async configures result-stage guardrails applied to ASYNC job results
	// (transcripts, OCR text, ...) in the once-per-job completion path. Unlike
	// the input path (whose async submit carries only an uploaded file), the
	// scannable content of an async job is its result text, so this stage runs
	// against the job's result object after completion. Like the input stage it
	// supports both regex checks and model-backed detectors. When nil, async
	// result guardrails are disabled.
	Async *GuardrailsAsyncConfig `yaml:"async"`
	// FlaggedSamples enables emitting a correlated, PII-redacted record when an
	// ASYNC (shadow) model detector flags, so shadow detections can be reviewed —
	// not just counted — before flipping the detector to enforce. When nil, no
	// samples are recorded.
	FlaggedSamples *FlaggedSamplesConfig `yaml:"flagged_samples"`
}

// FlaggedSamplesConfig controls the correlated flagged-sample record emitted on an
// async guardrail flag. The record carries {trace_id, detector, score, model,
// consumer, redacted_prompt}; the prompt is redacted before it enters the record.
type FlaggedSamplesConfig struct {
	// Enabled turns on flagged-sample recording for this service.
	Enabled bool `yaml:"enabled"`
	// Redact selects the guardrail groups used to strip PII from the sampled prompt
	// (same group names as checks: pii, pii_fr, secrets, ...). Empty defaults to
	// [pii, secrets] — the universal, country-agnostic set. Prefer over-redaction:
	// this copy is for review, never delivery.
	Redact []string `yaml:"redact"`
}

// GuardrailsAsyncConfig controls result-stage detection for async job results.
// Detectors run out-of-band in the gateway's once-per-job completion handler;
// in this slice they run in shadow (observe-only, emitting metrics) regardless
// of Action, and enforcement (block/redact) is a later slice.
type GuardrailsAsyncConfig struct {
	// Action applied when a check matches: "block", "redact", or "flag"
	// (default flag/shadow — observe only).
	Action string `yaml:"action"`
	// Checks selects which regex guardrail groups to run on the result text.
	Checks []string `yaml:"checks"`
	// Models configures model-backed detectors run on the result text.
	Models []GuardrailModelConfig `yaml:"models"`
	// ScanTimeout (block/redact only) bounds how long the client-facing result is
	// withheld awaiting the scan. If the scan hasn't completed by then, OnTimeout
	// decides. Duration string; supports ${VAR:-default}. Default "30s".
	ScanTimeout string `yaml:"scan_timeout"`
	// OnTimeout (block/redact only): "fail_open" (default — deliver the un-scanned
	// result once ScanTimeout lapses) or "fail_closed" (mark the job failed).
	OnTimeout string `yaml:"on_timeout"`
}

// GuardrailModelConfig configures one model-backed guardrail detector (a
// self-hosted classifier/NER endpoint). It augments the regex checks; it does
// not replace them.
type GuardrailModelConfig struct {
	// Name identifies the detector in metrics/logs.
	Name string `yaml:"name"`
	// Endpoint is the guardrail model's HTTP endpoint.
	Endpoint string `yaml:"endpoint"`
	// Kind is "classifier" (block/flag) or "ner" (spans → redact). Default classifier.
	Kind string `yaml:"kind"`
	// Categories filters which findings to act on; empty = all returned.
	Categories []string `yaml:"categories"`
	// Mode is "async" (shadow: observe only, default) or "sync" (inline: can act).
	Mode string `yaml:"mode"`
	// Action on a finding: "block", "redact", or "flag". async coerces to flag.
	Action string `yaml:"action"`
	// Threshold is the minimum score to act on a finding.
	Threshold float64 `yaml:"threshold"`
	// Timeout bounds a sync detector call (duration string, e.g. "120ms").
	// Supports ${VAR:-default} expansion. Default 120ms.
	Timeout string `yaml:"timeout"`
	// OnError is "fail_open" (default: forward on failure) or "fail_closed".
	OnError string `yaml:"on_error"`
	// MaxInputTokens skips the model call (allow) when the estimated input token
	// count exceeds this, protecting the latency budget. 0 = no gate.
	MaxInputTokens int `yaml:"max_input_tokens"`
	// CacheTTL enables verdict caching for this detector when set (duration
	// string, e.g. "5m"; supports ${VAR:-default}). Identical inputs reuse the
	// cached findings instead of calling the model. Empty/0 = no caching.
	CacheTTL string `yaml:"cache_ttl"`
}

// LoadFromBytes parses a YAML config from an in-memory byte slice.
// Exported for testing; production code uses Load(path).
func LoadFromBytes(data []byte) (*Config, error) {
	expanded := []byte(os.Expand(string(data), expandWithDefault))
	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

// Load reads and validates the YAML config file at path.
// Values of the form ${VAR} or ${VAR:-default} are expanded from the environment.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}
	return LoadFromBytes(data)
}

// expandWithDefault gère la syntaxe ${VAR:-default} que os.ExpandEnv ne supporte pas.
// Si la variable est définie et non vide, sa valeur est retournée ; sinon le défaut est utilisé.
func expandWithDefault(key string) string {
	if i := strings.Index(key, ":-"); i >= 0 {
		varName, defaultVal := key[:i], key[i+2:]
		if v, ok := os.LookupEnv(varName); ok && v != "" {
			return v
		}
		return defaultVal
	}
	return os.Getenv(key)
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 120 * time.Second
	}
	// WriteTimeout has no default: 0 means no timeout, which is correct for a
	// gateway that handles long-running direct-proxy jobs (OCR, large audio).
	// Set explicitly in config if a hard limit is desired.
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = 120 * time.Second
	}
	if c.Health.Timeout == "" {
		c.Health.Timeout = "5s"
	}
	if c.Health.Interval == "" {
		c.Health.Interval = "30s"
	}
	if c.Redis.PendingMaxAge == "" {
		c.Redis.PendingMaxAge = "2h"
	}
	if c.Lifecycle.GC.Interval == "" {
		c.Lifecycle.GC.Interval = "15m"
	}
	if c.Lifecycle.GC.OrphanMinAge == "" {
		c.Lifecycle.GC.OrphanMinAge = "5m"
	}
	if len(c.Otel.Traces.IgnorePaths) == 0 {
		c.Otel.Traces.IgnorePaths = []string{"/health", "/metrics", "/docs", "/openapi.yaml"}
	}
	for i := range c.Services {
		if c.Services[i].MaxFileSizeMB == 0 {
			c.Services[i].MaxFileSizeMB = 100
		}
	}
	if c.CircuitBreaker.Enabled {
		if c.CircuitBreaker.FailureThreshold <= 0 {
			c.CircuitBreaker.FailureThreshold = 5
		}
		if c.CircuitBreaker.Cooldown == "" {
			c.CircuitBreaker.Cooldown = "30s"
		}
	}
}

func (c *Config) validate() error {
	validModes := map[string]bool{"": true, "oauth2": true, "proxy": true}
	if !validModes[c.Auth.Mode] {
		return fmt.Errorf("auth.mode %q is invalid (valid: \"\", \"oauth2\", \"proxy\")", c.Auth.Mode)
	}
	if c.Policies != nil {
		validDefaults := map[string]bool{"": true, "deny": true, "allow": true}
		if !validDefaults[c.Policies.Default] {
			return fmt.Errorf("policies.default %q is invalid (valid: \"\", \"deny\", \"allow\")", c.Policies.Default)
		}
		// Policies require an authenticated identity; enforce that auth is configured.
		if len(c.Policies.Rules) > 0 || c.Policies.Default == "deny" || c.Policies.Default == "" {
			if c.Auth.Mode == "" {
				return fmt.Errorf("policies require auth.mode to be set (oauth2 or proxy): cannot enforce identity-based access without authentication")
			}
		}
		for i, rule := range c.Policies.Rules {
			if rule.Limits == nil {
				continue
			}
			if rule.Limits.Rate > 0 && rule.Limits.Period == "" {
				return fmt.Errorf("policies.rules[%d].limits: rate requires period", i)
			}
			if rule.Limits.TokenRate > 0 && rule.Limits.TokenPeriod == "" {
				return fmt.Errorf("policies.rules[%d].limits: token_rate requires token_period", i)
			}
		}
	}
	if c.Auth.Mode == "oauth2" {
		if c.Auth.OAuth2.Issuer == "" {
			return fmt.Errorf("auth.oauth2.issuer is required when auth.mode is \"oauth2\"")
		}
		if len(c.Auth.OAuth2.Audiences) == 0 {
			return fmt.Errorf("auth.oauth2.audiences must have at least one entry when auth.mode is \"oauth2\"")
		}
		validValidations := map[string]bool{"": true, "auto": true, "jwt": true, "introspection": true}
		if !validValidations[c.Auth.OAuth2.Validation] {
			return fmt.Errorf("auth.oauth2.validation %q is invalid (valid: \"\", \"auto\", \"jwt\", \"introspection\")", c.Auth.OAuth2.Validation)
		}
		needsIntrospection := c.Auth.OAuth2.Validation == "introspection" ||
			(c.Auth.OAuth2.Validation == "auto" && c.Auth.OAuth2.Introspection != nil)
		if needsIntrospection {
			if c.Auth.OAuth2.Introspection == nil {
				return fmt.Errorf("auth.oauth2.introspection block is required when validation is \"introspection\"")
			}
			if c.Auth.OAuth2.Introspection.ClientID == "" {
				return fmt.Errorf("auth.oauth2.introspection.client_id is required")
			}
			if c.Auth.OAuth2.Introspection.Endpoint == "" && c.Auth.OAuth2.Issuer == "" {
				return fmt.Errorf("auth.oauth2.introspection.endpoint or auth.oauth2.issuer is required for introspection endpoint discovery")
			}
		}
	}
	if c.S3.Endpoint == "" {
		return fmt.Errorf("s3.endpoint is required")
	}
	if c.S3.Region == "" {
		return fmt.Errorf("s3.region is required")
	}
	if c.S3.Bucket == "" {
		return fmt.Errorf("s3.bucket is required")
	}
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr is required")
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("at least one service must be configured")
	}
	for _, svc := range c.Services {
		if svc.Type == "" {
			return fmt.Errorf("a service has an empty type")
		}
	}
	for svcType, userLimits := range c.RateLimits {
		for userType, limit := range userLimits {
			if limit.TokenRate > 0 && limit.TokenPeriod == "" {
				return fmt.Errorf("rate_limits[%s][%s]: token_rate requires token_period", svcType, userType)
			}
			if limit.ProcessingTime > 0 && limit.ProcessingPeriod == "" {
				return fmt.Errorf("rate_limits[%s][%s]: processing_time requires processing_period", svcType, userType)
			}
		}
	}
	poolNames := make([]string, 0, len(c.BackendPools))
	for name := range c.BackendPools {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)
	for _, name := range poolNames {
		pool := c.BackendPools[name]
		if len(pool.Members) == 0 {
			return fmt.Errorf("backend_pools[%s]: at least one member is required", name)
		}
		seenURLs := make(map[string]bool, len(pool.Members))
		for i, m := range pool.Members {
			if m.URL == "" {
				return fmt.Errorf("backend_pools[%s].members[%d].url must not be empty", name, i)
			}
			if seenURLs[m.URL] {
				return fmt.Errorf("backend_pools[%s].members[%d]: duplicate url %q", name, i, m.URL)
			}
			seenURLs[m.URL] = true
			if m.Weight < 0 {
				return fmt.Errorf("backend_pools[%s].members[%d].weight must be >= 0", name, i)
			}
			if m.MaxConcurrent < 0 {
				return fmt.Errorf("backend_pools[%s].members[%d].max_concurrent must be >= 0", name, i)
			}
			if err := validatePoolRateLimit(m.RateLimit); err != nil {
				return fmt.Errorf("backend_pools[%s].members[%d].rate_limit: %w", name, i, err)
			}
		}
		if pool.MaxConcurrent < 0 {
			return fmt.Errorf("backend_pools[%s].max_concurrent must be >= 0", name)
		}
		if pool.PriorityReservedConcurrent < 0 {
			return fmt.Errorf("backend_pools[%s].priority_reserved_concurrent must be >= 0", name)
		}
		if err := validatePoolRateLimit(pool.RateLimit); err != nil {
			return fmt.Errorf("backend_pools[%s].rate_limit: %w", name, err)
		}
	}
	validProviders := map[string]bool{"openai": true, "anthropic": true, "ollama": true, "passthrough": true}
	for _, svc := range c.Services {
		if svc.Provider != "" && !validProviders[svc.Provider] {
			return fmt.Errorf("service %q: unknown provider %q (valid: openai, anthropic, ollama, passthrough)", svc.Type, svc.Provider)
		}
		if svc.ResponseCacheTTL < 0 {
			return fmt.Errorf("service %q: response_cache_ttl must be >= 0", svc.Type)
		}
		if svc.Guardrails.Output != nil {
			switch svc.Guardrails.Output.Streaming {
			case "", "flag", "block", "buffer":
			default:
				return fmt.Errorf("service %q: guardrails.output.streaming %q is invalid (valid: flag, block, buffer)", svc.Type, svc.Guardrails.Output.Streaming)
			}
		}
		for i, b := range svc.Backends {
			if b.URL == "" {
				return fmt.Errorf("service %q: backends[%d].url must not be empty", svc.Type, i)
			}
			if b.Weight < 0 {
				return fmt.Errorf("service %q: backends[%d].weight must be >= 0", svc.Type, i)
			}
		}
		if svc.BackendPool != "" {
			if len(svc.Backends) > 0 {
				return fmt.Errorf("service %q: backend_pool is mutually exclusive with backends", svc.Type)
			}
			if svc.InferenceURL != "" {
				return fmt.Errorf("service %q: backend_pool is mutually exclusive with inference_url", svc.Type)
			}
			if _, ok := c.BackendPools[svc.BackendPool]; !ok {
				return fmt.Errorf("service %q: backend_pool %q is not defined in backend_pools", svc.Type, svc.BackendPool)
			}
		}
		for userType, limit := range svc.TokenLimits {
			if limit.TokenRate > 0 && limit.TokenPeriod == "" {
				return fmt.Errorf("service %q: token_limits[%q]: token_rate requires token_period", svc.Model, userType)
			}
		}
		validActions := map[string]bool{"block": true, "redact": true, "flag": true}
		validChecks := map[string]bool{
			"pii": true, "pii_fr": true, "pii_us": true, "pii_uk": true,
			"pii_es": true, "pii_it": true, "secrets": true,
		}
		if svc.Guardrails.Action != "" && !validActions[svc.Guardrails.Action] {
			return fmt.Errorf("service %q: guardrails.action %q is invalid (valid: block, redact, flag)", svc.Type, svc.Guardrails.Action)
		}
		for _, check := range svc.Guardrails.Checks {
			if !validChecks[check] {
				return fmt.Errorf("service %q: guardrails.checks contains unknown check %q (valid: pii, pii_fr, pii_us, pii_uk, pii_es, pii_it, secrets)", svc.Type, check)
			}
		}
		if out := svc.Guardrails.Output; out != nil {
			if out.Action != "" && !validActions[out.Action] {
				return fmt.Errorf("service %q: guardrails.output.action %q is invalid (valid: block, redact, flag)", svc.Type, out.Action)
			}
			for _, check := range out.Checks {
				if !validChecks[check] {
					return fmt.Errorf("service %q: guardrails.output.checks contains unknown check %q (valid: pii, pii_fr, pii_us, pii_uk, pii_es, pii_it, secrets)", svc.Type, check)
				}
			}
		}
	}
	return nil
}

// validatePoolRateLimit checks a RateLimitConfig used at backend_pools scope
// (pool or member level), where only Rate+Period are supported — token,
// processing-time, and max_concurrent semantics belong to other config
// fields (token_limits, and the pool/member max_concurrent field) and would
// be misleading if silently ignored here.
func validatePoolRateLimit(rl RateLimitConfig) error {
	if rl.Rate > 0 && rl.Period == "" {
		return fmt.Errorf("rate requires period")
	}
	if rl.TokenRate != 0 || rl.TokenPeriod != "" {
		return fmt.Errorf("token_rate/token_period are not supported here (use the service's token_limits instead)")
	}
	if rl.MaxConcurrent != 0 {
		return fmt.Errorf("max_concurrent is not supported on rate_limit (use the pool/member max_concurrent field instead)")
	}
	if rl.ProcessingTime != 0 || rl.ProcessingPeriod != "" {
		return fmt.Errorf("processing_time/processing_period are not supported here")
	}
	return nil
}
