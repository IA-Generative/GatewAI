package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/authz"
	"gatewai/gateway/internal/cache"
	"gatewai/gateway/internal/concurrency"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/consumer"
	"gatewai/gateway/internal/guardrails"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/health"
	"gatewai/gateway/internal/llmproxy"
	"gatewai/gateway/internal/llmproxy/provider"
	gmetrics "gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/storage"
	"gatewai/gateway/internal/telemetry"
	"gatewai/gateway/internal/usage"
)

// version is set at build time via -ldflags "-X main.version=v0.4.1".
var version = "dev"

// buildModelLimits derives a model→userType→RateLimitConfig map from services.
// Only services with a non-empty Model and non-empty TokenLimits contribute an entry.
func buildModelLimits(services []config.ServiceConfig) map[string]map[string]config.RateLimitConfig {
	m := make(map[string]map[string]config.RateLimitConfig)
	for _, svc := range services {
		if svc.Model != "" && len(svc.TokenLimits) > 0 {
			m[svc.Model] = svc.TokenLimits
		}
	}
	return m
}

// tokenChecker returns l as a ratelimit.TokenChecker, or a true nil interface
// when l is nil. Passing a typed-nil *Limiter directly would yield a non-nil
// interface, defeating the handler's nil check and panicking on first use.
func tokenChecker(l *ratelimit.Limiter) ratelimit.TokenChecker {
	if l == nil {
		return nil
	}
	return l
}

// poolChecker returns l as a ratelimit.PoolChecker, or a true nil interface
// when l is nil (see tokenChecker for why a typed-nil check doesn't suffice).
func poolChecker(l *ratelimit.Limiter) ratelimit.PoolChecker {
	if l == nil {
		return nil
	}
	return l
}

// buildPoolLimits derives a pool name → pool-wide RateLimitConfig map from
// backend_pools. Only pools with Rate > 0 contribute an entry.
func buildPoolLimits(pools map[string]config.BackendPoolConfig) map[string]config.RateLimitConfig {
	m := make(map[string]config.RateLimitConfig)
	for name, pool := range pools {
		if pool.RateLimit.Rate > 0 {
			m[name] = pool.RateLimit
		}
	}
	return m
}

// buildPoolMemberLimits derives a pool name → member URL → RateLimitConfig
// map from backend_pools. Only members with Rate > 0 contribute an entry.
func buildPoolMemberLimits(pools map[string]config.BackendPoolConfig) map[string]map[string]config.RateLimitConfig {
	m := make(map[string]map[string]config.RateLimitConfig)
	for name, pool := range pools {
		for _, member := range pool.Members {
			if member.RateLimit.Rate > 0 {
				if m[name] == nil {
					m[name] = make(map[string]config.RateLimitConfig)
				}
				m[name][member.URL] = member.RateLimit
			}
		}
	}
	return m
}

// dedupeProbeTargets flattens the backends of all sync-enabled services into
// a set of active-probe targets, de-duplicated by (URL, HealthPath) — the
// pair that actually determines probe behavior. Without this, backend_pools
// sharing a URL across multiple service aliases would be probed once per
// alias instead of once per physical backend.
func dedupeProbeTargets(defs []*service.Def) []llmproxy.ProbeTarget {
	type key struct{ url, healthPath string }
	seen := make(map[key]bool)
	var targets []llmproxy.ProbeTarget
	for _, d := range defs {
		if d.HealthCheck.Disabled {
			continue
		}
		for _, b := range d.Backends {
			k := key{b.URL, d.HealthCheck.Path}
			if seen[k] {
				continue
			}
			seen[k] = true
			targets = append(targets, llmproxy.ProbeTarget{Model: d.Model, URL: b.URL, HealthPath: d.HealthCheck.Path})
		}
	}
	return targets
}

// routerHolder is an atomically-swappable http.Handler.
// The outer http.Server always points to this wrapper; hot reload replaces the inner router.
type routerHolder struct {
	p atomic.Pointer[chi.Mux]
}

func (h *routerHolder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.p.Load().ServeHTTP(w, r)
}

// reservedGatewayPaths lists the path prefixes owned by the gateway itself.
// Configured service paths matching any of these are silently skipped to
// prevent accidental overrides of built-in routes.
var reservedGatewayPaths = []string{
	"/health",
	"/metrics",
	"/docs",
	"/openapi.yaml",
	"/jobs",
	"/usage",
	"/-",
}

// reservedGatewayPath reports whether path starts with a reserved gateway prefix.
func reservedGatewayPath(path string) bool {
	for _, prefix := range reservedGatewayPaths {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// authExemptPrefixes lists the path prefixes that bypass authentication checks.
// "/-/relay" is the relay's completion callback — no client ever calls it, and
// it carries no credentials; it is trusted purely on cluster-internal network
// access, the same trust model as /health.
var authExemptPrefixes = []string{"/health", "/metrics", "/docs", "/openapi.yaml", "/-/relay"}

func buildRouter(
	cfg *config.Config,
	reg *service.Registry,
	s3Client *storage.S3Client,
	redisClient *storage.RedisClient,
	logger *slog.Logger,
	reloadFn func() error,
	rl ratelimit.Checker,
	limiter *ratelimit.Limiter,
	llmHandler *llmproxy.Handler,
	tracer trace.Tracer,
	authenticator auth.Authenticator,
	authzEngine *authz.Engine,
	healthChecker *health.Checker,
	usageTracker usage.UsageTracker,
	usageHTTPHandler *usage.UsageHandler,
	relayCompleteHandler *handler.RelayCompleteHandler,
) *chi.Mux {
	jobHandler := handler.NewJobHandler(reg, s3Client, redisClient, cfg.Server.PriorityHeader, cfg.Server.ConsumerHeader, rl, cfg.Lifecycle).
		WithIdempotencyTTL(cfg.Jobs.IdempotencyTTLDuration())
	if limiter != nil {
		jobHandler.WithConcurrentLimiter(limiter, cfg.Server.UserTypeHeader)
		jobHandler.WithProcessingTimeLimiter(limiter)
		jobHandler.WithTokenLimiter(limiter)
	}
	if authzEngine != nil {
		jobHandler.WithAuthz(authzEngine)
	}
	if usageTracker != nil {
		jobHandler.WithUsageTracker(usageTracker)
	}

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(handler.OtelMiddleware(tracer, cfg.Otel.Traces.IgnorePaths...))
	r.Use(handler.StructuredLogger(logger))
	r.Use(chimw.Recoverer)
	if authenticator != nil {
		r.Use(auth.Middleware(authenticator, cfg.Auth.Mode, authExemptPrefixes, cfg.Server.ConsumerHeader, cfg.Server.UserTypeHeader))
	}

	spec := handler.GenerateSpec(reg, version, usageHTTPHandler != nil)
	adminSpec := handler.GenerateAdminSpec(version, usageHTTPHandler != nil, limiter != nil)
	swaggerSpecs := handler.FetchSwaggerSpecs(cfg.Services)

	r.Get("/health", handler.NewHealthHandler(healthChecker.Snapshot))
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	r.Get("/docs", handler.DocsUI(swaggerSpecs))
	r.Get("/openapi.yaml", handler.NewDocsSpec(spec))
	r.Get("/docs/spec/{type}/{model}", handler.NewSwaggerHandler(swaggerSpecs))
	r.Get("/-/docs", handler.AdminDocsUI())
	r.Get("/-/openapi.yaml", handler.NewDocsSpec(adminSpec))
	r.Get("/jobs", jobHandler.ListJobs)
	r.Post("/jobs/{service_type}", jobHandler.Submit)
	r.Get("/jobs/{service_type}/{id}", jobHandler.GetStatus)
	r.Delete("/jobs/{service_type}/{id}", jobHandler.Cancel)
	r.Post("/-/reload", handler.NewReloadHandler(reloadFn))
	r.Post("/-/jobs/purge", jobHandler.AdminPurge)
	r.Post("/-/relay/jobs/{id}/complete", relayCompleteHandler.Complete)
	if usageHTTPHandler != nil {
		r.Get("/usage", usageHTTPHandler.GetMyUsage)
		r.Get("/-/usage", usageHTTPHandler.AdminListUsage)
		r.Get("/-/usage/report", usageHTTPHandler.AdminUsageReport)
	}
	if limiter != nil {
		r.Post("/-/quota/reset", handler.NewQuotaHandler(limiter).ResetQuota)
	}

	if reg.HasSyncServices() || reg.HasRealtimeServices() {
		// Backend health for degraded-model listing + cross-model fallback. Only
		// set when a breaker is present so BackendHealth stays a nil interface
		// (avoids a typed-nil wrapping a nil *CircuitBreaker).
		var modelHealth handler.BackendHealth
		if b := llmHandler.Breaker(); b != nil {
			modelHealth = b
		}
		sh := handler.NewSyncHandler(reg, cfg.Server.ConsumerHeader, rl, llmHandler).
			WithSemaphore(concurrency.NewModelSemaphore(reg, redisClient.Raw())).
			WithPoolSemaphore(concurrency.NewPoolSemaphore(cfg.BackendPools, redisClient.Raw())).
			WithPoolLimiter(poolChecker(limiter)).
			WithPriorityHeader(cfg.Server.PriorityHeader).
			WithMaxBodyMB(cfg.Server.MaxBodyMB).
			WithCircuitBreaker(modelHealth)
		if limiter != nil {
			sh.WithProcessingLimiter(limiter, cfg.Server.UserTypeHeader)
			sh.WithTokenLimiter(limiter)
		}
		if authzEngine != nil {
			sh.WithAuthz(authzEngine)
		}
		if usageTracker != nil {
			sh.WithUsageTracker(usageTracker)
		}
		syncHandler := sh
		r.Get("/v1/models", handler.ListModels(reg, cfg.Server.UserTypeHeader, modelHealth))
		// Register each configured path exactly. Chi handles {model} parameter
		// patterns natively. Single-segment paths (e.g. /rerank) are reachable
		// without needing a separate wildcard route.
		// Reserved gateway paths are skipped — they cannot be overridden by config.
		for _, path := range reg.SyncPaths() {
			if reservedGatewayPath(path) {
				slog.Warn("skipping sync path: conflicts with reserved gateway route", "path", path)
				continue
			}
			r.Post(path, syncHandler.ServeHTTP)
		}
		if reg.HasSyncServices() {
			slog.Info("sync proxy enabled", "paths", reg.SyncPaths())
		}
		// Realtime (WebSocket) streaming services are registered as GET (upgrade)
		// routes on their WS path.
		for _, path := range reg.RealtimePaths() {
			if reservedGatewayPath(path) {
				slog.Warn("skipping realtime path: conflicts with reserved gateway route", "path", path)
				continue
			}
			r.Get(path, syncHandler.ServeRealtimeWS)
		}
		for _, path := range reg.RealtimeInfoPaths() {
			if reservedGatewayPath(path) {
				slog.Warn("skipping realtime info path: conflicts with reserved gateway route", "path", path)
				continue
			}
			r.Get(path, syncHandler.ServeRealtimeInfo)
		}
		if reg.HasRealtimeServices() {
			slog.Info("realtime websocket proxy enabled", "paths", reg.RealtimePaths())
		}
	}

	return r
}

// guardrailMetricsObserver forwards model-detector telemetry from the guardrails
// package (a leaf) to the metrics package.
type guardrailMetricsObserver struct{}

func (guardrailMetricsObserver) ObserveModelLatency(detector string, seconds float64) {
	gmetrics.GuardrailsModelLatency.WithLabelValues(detector).Observe(seconds)
}

func (guardrailMetricsObserver) IncModelError(detector, reason string) {
	gmetrics.GuardrailsModelErrorsTotal.WithLabelValues(detector, reason).Inc()
}

func (guardrailMetricsObserver) IncModelCache(detector, result string) {
	gmetrics.GuardrailsModelCacheTotal.WithLabelValues(detector, result).Inc()
}

func (guardrailMetricsObserver) IncModelSkipped(detector, reason string) {
	gmetrics.GuardrailsModelSkippedTotal.WithLabelValues(detector, reason).Inc()
}

// guardrailVerdictCache adapts the Redis client to guardrails.VerdictCache,
// storing findings as JSON under a dedicated key namespace.
type guardrailVerdictCache struct{ rdb *redis.Client }

func (c guardrailVerdictCache) Get(ctx context.Context, key string) ([]guardrails.Finding, bool) {
	val, err := c.rdb.Get(ctx, "guardrail:verdict:"+key).Bytes()
	if err != nil {
		return nil, false
	}
	var f []guardrails.Finding
	if json.Unmarshal(val, &f) != nil {
		return nil, false
	}
	return f, true
}

func (c guardrailVerdictCache) Set(ctx context.Context, key string, findings []guardrails.Finding, ttl time.Duration) {
	data, err := json.Marshal(findings)
	if err != nil {
		return
	}
	c.rdb.Set(ctx, "guardrail:verdict:"+key, data, ttl)
}

func main() {
	// JSON structured logger — compatible with log aggregators (Loki, Datadog, …).
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Route model-detector telemetry to the metrics package (keeps guardrails a leaf).
	guardrails.SetObserver(guardrailMetricsObserver{})

	// ── Config ────────────────────────────────────────────────────────────────
	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// ── OpenTelemetry ─────────────────────────────────────────────────────────
	otelSvcName := cfg.Otel.ServiceName
	if otelSvcName == "" {
		otelSvcName = "gatewai/gateway"
	}
	tel, otelShutdown, err := telemetry.Setup(context.Background(), cfg.Otel, otelSvcName, version)
	if err != nil {
		slog.Error("failed to initialise OpenTelemetry", "error", err)
		os.Exit(1)
	}
	_ = tel // tel.Tracer / tel.Meter available for direct use if needed
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := otelShutdown(shutCtx); err != nil {
			slog.Error("OTel shutdown error", "error", err)
		}
	}()

	// ── Service registry ──────────────────────────────────────────────────────
	initialRegistry := service.NewRegistry(cfg.Services, service.WithBackendPools(cfg.BackendPools))
	slog.Info("service registry initialised", "types", initialRegistry.Types())

	// ── Dependencies ──────────────────────────────────────────────────────────
	s3Client, err := storage.NewS3Client(cfg.S3, cfg.Encryption)
	if err != nil {
		slog.Error("failed to initialise S3 storage", "error", err)
		os.Exit(1)
	}
	slog.Info("S3 storage initialised", "encryption", cfg.Encryption.Key != "")

	redisClient, err := storage.NewRedis(cfg.Redis, cfg.Lifecycle)
	if err != nil {
		slog.Error("failed to initialise Redis", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()
	redisClient.SetExpiredMarkerTTL(cfg.Jobs.ExpiredMarkerTTLDuration())

	// Verdict cache for model-backed guardrails (reuses the Redis client).
	guardrails.SetVerdictCache(guardrailVerdictCache{rdb: redisClient.Client()})

	var rl ratelimit.Checker
	var limiter *ratelimit.Limiter
	modelLimits := buildModelLimits(cfg.Services)
	poolLimits := buildPoolLimits(cfg.BackendPools)
	poolMemberLimits := buildPoolMemberLimits(cfg.BackendPools)
	// The limiter is also needed when policies carry per-group limits, since those
	// are enforced through the same limiter (via request-context policy limits).
	if len(cfg.RateLimits) > 0 || len(modelLimits) > 0 || cfg.Policies != nil || len(poolLimits) > 0 || len(poolMemberLimits) > 0 {
		limiter = ratelimit.New(redisClient.Client(), cfg.RateLimits, modelLimits, cfg.Server.ConsumerHeader, cfg.Server.UserTypeHeader)
		limiter.SetPoolLimits(poolLimits, poolMemberLimits)
		rl = limiter
		slog.Info("rate limiting enabled", "services", len(cfg.RateLimits), "model_limits", len(modelLimits), "policies", cfg.Policies != nil)
	}

	manager := consumer.NewManager(redisClient)

	relayCompleteHandler := handler.NewRelayCompleteHandler(redisClient, s3Client, cfg.Lifecycle.PersistsResult, cfg.Webhooks)
	relayCompleteHandler.WithRegistry(initialRegistry) // result-stage (async) guardrails
	if limiter != nil {
		relayCompleteHandler.WithProcessingTimeLimiter(limiter)
		relayCompleteHandler.WithTokenLimiter(limiter)
	}

	// ── LLM proxy ─────────────────────────────────────────────────────────────
	providerRegistry := provider.NewRegistry()
	responseCache := cache.NewRedisCache(redisClient.Raw())
	llmHTTPClient := &http.Client{Timeout: 15 * time.Minute}

	var consumerTracker gmetrics.ConsumerTracker = gmetrics.NoopTracker{}
	if cfg.Metrics.TopConsumers > 0 {
		consumerTracker = gmetrics.NewRedisTracker(redisClient.Raw())
	}

	// ── Usage tracker ─────────────────────────────────────────────────────────
	var usageTracker usage.UsageTracker
	var usageHTTPHandler *usage.UsageHandler
	var usageStore usage.UsageStore
	if cfg.Server.ConsumerHeader != "" {
		ut := usage.NewRedisUsageTracker(redisClient.Raw(), cfg.Usage.RetentionDuration())
		usageTracker = ut
		usageStore = usage.NewRedisUsageStore(redisClient.Raw(), cfg.Usage.Retention, cfg.RateLimits, modelLimits)
		usageHTTPHandler = usage.NewUsageHandler(usageStore, initialRegistry, cfg.Server.ConsumerHeader, cfg.Server.UserTypeHeader)
		relayCompleteHandler.WithUsageTracker(usageTracker)
		slog.Info("usage tracking enabled", "retention", cfg.Usage.Retention)
	}

	// Circuit breaker for LLM backends — created once so its per-backend state
	// (open/closed) survives config hot-reloads.
	var breaker *llmproxy.CircuitBreaker
	if cfg.CircuitBreaker.Enabled {
		cooldown, _ := time.ParseDuration(cfg.CircuitBreaker.Cooldown)
		breaker = llmproxy.NewCircuitBreaker(cfg.CircuitBreaker.FailureThreshold, cooldown)
		slog.Info("llm backend circuit breaker enabled",
			"failure_threshold", cfg.CircuitBreaker.FailureThreshold, "cooldown", cfg.CircuitBreaker.Cooldown)
	}

	var llmHandler *llmproxy.Handler
	llmHandler = llmproxy.New(responseCache, providerRegistry, llmHTTPClient,
		cfg.Server.UserTypeHeader, consumerTracker,
		llmproxy.AuditConfig{Enabled: cfg.AuditLog.Enabled, Prompt: cfg.AuditLog.Prompt},
		tokenChecker(limiter))
	if usageTracker != nil {
		llmHandler.WithUsageTracker(usageTracker)
	}
	llmHandler.WithLangfuse(cfg.Otel.Enabled && cfg.Otel.Traces.Enabled && cfg.Otel.Traces.Langfuse.Enabled)
	llmHandler.WithCircuitBreaker(breaker)
	llmHandler.WithPoolSemaphore(concurrency.NewPoolSemaphore(cfg.BackendPools, redisClient.Raw())).
		WithPoolLimiter(poolChecker(limiter)).
		WithPriorityHeader(cfg.Server.PriorityHeader)

	// ── Authenticator ────────────────────────────────────────────────────────
	// Build once; reused across reloads. The JWKS refresh goroutine is started
	// with context.Background() so it lives for the full process lifetime.
	// Auth config changes require a gateway restart.
	authenticator, err := buildAuthenticator(context.Background(), cfg)
	if err != nil {
		slog.Error("failed to initialise authenticator", "error", err)
		os.Exit(1)
	}
	if cfg.Auth.Mode != "" {
		slog.Info("authentication enabled", "mode", cfg.Auth.Mode)
	}
	// ── Health checker ────────────────────────────────────────────────────────
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	healthChecker := health.New(initialRegistry, redisClient.Raw(), cfg.Health, hostname)

	// ── Relay queue depth ────────────────────────────────────────────────────
	relayQueueDepth := gmetrics.NewRelayQueueDepthCollector(redisClient.Raw(), initialRegistry)
	prometheus.MustRegister(relayQueueDepth)

	// ── Hot-reload ────────────────────────────────────────────────────────────
	// reloadFn re-reads the config file, atomically swaps the active router,
	// and reconciles Redis subscribers (stopping removed, starting added models).
	// Infrastructure (S3, Redis) is not re-initialised.
	holder := &routerHolder{}

	// GC atomics — declared before reloadFn so the reload path can update them.
	var (
		gcEnabled         atomic.Bool
		gcInterval        atomic.Int64 // nanoseconds
		gcOrphanMinAge    atomic.Int64 // nanoseconds
		gcMaxAge          atomic.Int64 // nanoseconds
		gcMaxReapAttempts atomic.Int64
		gcRegistry        atomic.Pointer[service.Registry]
	)
	gcRegistry.Store(initialRegistry)

	var reloadFn func() error
	reloadFn = func() error {
		newCfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		newReg := service.NewRegistry(newCfg.Services, service.WithBackendPools(newCfg.BackendPools))

		// Update infrastructure state that survives across reloads.
		redisClient.UpdateLifecycle(newCfg.Lifecycle)
		relayCompleteHandler.UpdatePersistsResult(newCfg.Lifecycle.PersistsResult)
		relayCompleteHandler.UpdateRegistry(newReg)
		manager.Reconcile(newReg)
		healthChecker.UpdateRegistry(newReg)
		relayQueueDepth.UpdateRegistry(newReg)
		gcRegistry.Store(newReg)
		gcEnabled.Store(newCfg.Lifecycle.GC.Enabled)
		if iv := newCfg.Lifecycle.GC.IntervalDuration(); iv > 0 {
			gcInterval.Store(int64(iv))
		}
		if oma := newCfg.Lifecycle.GC.OrphanMinAgeDuration(); oma > 0 {
			gcOrphanMinAge.Store(int64(oma))
		}
		gcMaxAge.Store(int64(newCfg.Redis.PendingMaxAgeDuration()))
		gcMaxReapAttempts.Store(int64(newCfg.Lifecycle.GC.MaxReapAttemptsOrDefault()))

		// Rebuild stateless config-driven objects.
		newModelLimits := buildModelLimits(newCfg.Services)
		newPoolLimits := buildPoolLimits(newCfg.BackendPools)
		newPoolMemberLimits := buildPoolMemberLimits(newCfg.BackendPools)
		if len(newCfg.RateLimits) > 0 || len(newModelLimits) > 0 || newCfg.Policies != nil || len(newPoolLimits) > 0 || len(newPoolMemberLimits) > 0 {
			limiter = ratelimit.New(redisClient.Client(), newCfg.RateLimits, newModelLimits, newCfg.Server.ConsumerHeader, newCfg.Server.UserTypeHeader)
			limiter.SetPoolLimits(newPoolLimits, newPoolMemberLimits)
			rl = limiter
		} else {
			limiter = nil
			rl = nil
		}
		if limiter != nil {
			relayCompleteHandler.WithProcessingTimeLimiter(limiter)
			relayCompleteHandler.WithTokenLimiter(limiter)
		} else {
			relayCompleteHandler.WithProcessingTimeLimiter(nil)
			relayCompleteHandler.WithTokenLimiter(nil)
		}
		llmHandler = llmproxy.New(responseCache, providerRegistry, llmHTTPClient,
			newCfg.Server.UserTypeHeader, consumerTracker,
			llmproxy.AuditConfig{Enabled: newCfg.AuditLog.Enabled, Prompt: newCfg.AuditLog.Prompt},
			tokenChecker(limiter))
		if usageTracker != nil {
			llmHandler.WithUsageTracker(usageTracker)
		}
		llmHandler.WithLangfuse(newCfg.Otel.Enabled && newCfg.Otel.Traces.Enabled && newCfg.Otel.Traces.Langfuse.Enabled)
		llmHandler.WithCircuitBreaker(breaker) // reuse the same breaker (state persists across reloads)
		llmHandler.WithPoolSemaphore(concurrency.NewPoolSemaphore(newCfg.BackendPools, redisClient.Raw())).
			WithPoolLimiter(poolChecker(limiter)).
			WithPriorityHeader(newCfg.Server.PriorityHeader)

		// Reuse the existing authenticator. Auth config changes require a restart.
		var newAuthzEngine *authz.Engine
		if newCfg.Policies != nil {
			newAuthzEngine = authz.New(*newCfg.Policies)
		}
		if usageTracker != nil {
			usageTracker.UpdateRetention(newCfg.Usage.RetentionDuration())
		}
		if usageStore != nil {
			usageStore.UpdateRateLimits(newCfg.RateLimits, newModelLimits)
		}
		if usageHTTPHandler != nil {
			usageHTTPHandler.UpdateRegistry(newReg)
		}
		newRouter := buildRouter(newCfg, newReg, s3Client, redisClient, logger, reloadFn, rl, limiter, llmHandler, otel.Tracer("gatewai/gateway"), authenticator, newAuthzEngine, healthChecker, usageTracker, usageHTTPHandler, relayCompleteHandler)
		holder.p.Store(newRouter)
		slog.Info("config reloaded", "types", newReg.Types())
		return nil
	}

	// ── HTTP router ───────────────────────────────────────────────────────────
	var authzEngine *authz.Engine
	if cfg.Policies != nil {
		authzEngine = authz.New(*cfg.Policies)
	}
	initialRouter := buildRouter(cfg, initialRegistry, s3Client, redisClient, logger, reloadFn, rl, limiter, llmHandler, otel.Tracer("gatewai/gateway"), authenticator, authzEngine, healthChecker, usageTracker, usageHTTPHandler, relayCompleteHandler)
	holder.p.Store(initialRouter)

	// ── Async workers + context ───────────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.Metrics.TopConsumers > 0 {
		gmetrics.StartTopNRefresh(ctx, redisClient.Raw(), cfg.Metrics.TopConsumers, 60*time.Second)
		gmetrics.StartUsageTopNRefresh(ctx, redisClient.Raw(), cfg.Metrics.TopConsumers, 60*time.Second, initialRegistry.Types())
	}

	// Drive the once-per-job completion work (webhook, result scan, debits) from
	// the reliable pub/sub broadcast; the relay's /complete HTTP call remains a
	// redundant fast-path onto the same Redis claim.
	manager.WithCompletionProcessor(relayCompleteHandler)
	manager.Start(ctx, initialRegistry)
	go healthChecker.Start(ctx)
	relayCompleteHandler.StartRetryLoop(ctx)

	// Active circuit-breaker probing (opt-in) — per replica, so each replica's
	// breaker view stays current for idle/dead/recovered backends.
	if breaker != nil {
		if pi, _ := time.ParseDuration(cfg.CircuitBreaker.ProbeInterval); pi > 0 {
			probeClient := &http.Client{Timeout: 5 * time.Second}
			llmproxy.StartProber(ctx, breaker, probeClient, pi, func() []llmproxy.ProbeTarget {
				return dedupeProbeTargets(gcRegistry.Load().Models())
			})
			slog.Info("llm backend active health probing enabled", "interval", cfg.CircuitBreaker.ProbeInterval)
		}
	}

	// ── Unified GC ────────────────────────────────────────────────────────────
	// All atomics are read on each tick so hot-reload takes effect without restart.
	gcEnabled.Store(cfg.Lifecycle.GC.Enabled)
	iv := cfg.Lifecycle.GC.IntervalDuration()
	if iv <= 0 {
		iv = 15 * time.Minute
	}
	gcInterval.Store(int64(iv))
	oma := cfg.Lifecycle.GC.OrphanMinAgeDuration()
	if oma <= 0 {
		oma = 5 * time.Minute
	}
	gcOrphanMinAge.Store(int64(oma))
	gcMaxAge.Store(int64(cfg.Redis.PendingMaxAgeDuration()))
	gcMaxReapAttempts.Store(int64(cfg.Lifecycle.GC.MaxReapAttemptsOrDefault()))

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		var lastRun time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !gcEnabled.Load() {
					continue
				}
				interval := time.Duration(gcInterval.Load())
				if interval <= 0 {
					interval = 15 * time.Minute
				}
				if !lastRun.IsZero() && time.Since(lastRun) < interval {
					continue
				}
				lastRun = time.Now()
				runGC(ctx, redisClient, s3Client, gcRegistry.Load(),
					time.Duration(gcMaxAge.Load()),
					time.Duration(gcOrphanMinAge.Load()),
					int(gcMaxReapAttempts.Load()),
				)
			}
		}
	}()

	if cfg.Lifecycle.GC.Enabled {
		slog.Info("unified GC enabled",
			"interval", cfg.Lifecycle.GC.Interval,
			"pending_max_age", cfg.Redis.PendingMaxAge,
			"orphan_min_age", cfg.Lifecycle.GC.OrphanMinAge,
		)
	}

	// ── HTTP server ───────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      holder,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server starting", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server error", "error", err)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	}

	slog.Info("shutting down…")
	cancel() // stop async workers and other background goroutines

	relayCompleteHandler.Wait() // drain in-flight webhook goroutines

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}

	slog.Info("server stopped")
}

// buildAuthenticator creates the Authenticator requested by cfg.Auth.Mode.
// Returns (nil, nil) when mode is "" (legacy/no-auth).
// On error the caller must treat startup as fatal.
func buildAuthenticator(ctx context.Context, cfg *config.Config) (auth.Authenticator, error) {
	switch cfg.Auth.Mode {
	case "oauth2":
		c := cfg.Auth.OAuth2
		var introspectionCfg *auth.IntrospectionConfig
		if c.Introspection != nil {
			cacheTTL := 60 * time.Second
			if c.Introspection.CacheTTL != "" {
				if d, err := time.ParseDuration(c.Introspection.CacheTTL); err == nil {
					cacheTTL = d
				}
			}
			introspectionCfg = &auth.IntrospectionConfig{
				Endpoint:     c.Introspection.Endpoint,
				ClientID:     c.Introspection.ClientID,
				ClientSecret: c.Introspection.ClientSecret,
				CacheTTL:     cacheTTL,
			}
		}
		a, err := auth.NewOAuth2Authenticator(ctx, auth.OAuth2Config{
			Issuer:    c.Issuer,
			JWKSURL:   c.JWKSURL,
			Audiences: c.Audiences,
			Claims: auth.ClaimMap{
				Subject:  c.Claims.Subject,
				Consumer: c.Claims.Consumer,
				Scopes:   c.Claims.Scopes,
				Groups:   c.Claims.Groups,
				Roles:    c.Claims.Roles,
			},
			Validation:    c.Validation,
			Introspection: introspectionCfg,
		})
		if err != nil {
			return nil, err
		}
		return a, nil

	case "proxy":
		p := cfg.Auth.Proxy
		// Fall back to server-level headers when proxy-specific ones are empty,
		// so existing deployments keep working without any config change.
		consumerHeader := p.ConsumerHeader
		if consumerHeader == "" {
			consumerHeader = cfg.Server.ConsumerHeader
		}
		userTypeHeader := p.UserTypeHeader
		if userTypeHeader == "" {
			userTypeHeader = cfg.Server.UserTypeHeader
		}
		return auth.NewHeaderAuthenticator(auth.HeaderConfig{
			ConsumerHeader: consumerHeader,
			UserTypeHeader: userTypeHeader,
			GroupsHeader:   p.GroupsHeader,
			RolesHeader:    p.RolesHeader,
			ScopesHeader:   p.ScopesHeader,
		}), nil

	default: // "" — legacy, no auth
		return nil, nil
	}
}
