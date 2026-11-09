package usage

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/config"
)

// UsageStore reads per-consumer usage from Redis.
type UsageStore interface {
	// GetConsumerUsage returns usage for consumer across the given service
	// types. modelsByType optionally lists the models registered for each
	// service type, used to surface a per-model token-quota breakdown
	// (services[].token_limits) nested under the service entry; pass nil to
	// skip model-level reporting.
	GetConsumerUsage(ctx context.Context, consumer, userType string, serviceTypes []string, modelsByType map[string][]string) (*ConsumerUsage, error)
	ListConsumers(ctx context.Context, limit, offset int64) (consumers []string, total int64, err error)
	ListConsumersByType(ctx context.Context, serviceType string, limit, offset int64) (consumers []string, total int64, err error)
	// UpdateRateLimits swaps the rate_limits/model token_limits config (and the
	// model→budget-key map) consulted when resolving the configured quota surfaced
	// alongside window usage. Called on config hot-reload.
	UpdateRateLimits(rateLimits, modelLimits map[string]map[string]config.RateLimitConfig, budgetKeys map[string]string)
	// GetUsageReport returns cross-consumer calendar-aligned totals for
	// serviceType, one bucket per period (daily/weekly/monthly) covering
	// [from, to] inclusive. Returns ErrTooManyBuckets if the range would
	// produce more than maxReportBuckets buckets.
	GetUsageReport(ctx context.Context, serviceType, period string, from, to time.Time) (*UsageReport, error)
}

// ErrTooManyBuckets is returned by GetUsageReport when [from, to] would
// produce more buckets than maxReportBuckets allows.
var ErrTooManyBuckets = fmt.Errorf("usage report: requested range exceeds %d buckets", maxReportBuckets)

// maxReportBuckets bounds a single GetUsageReport call (e.g. ~13 months of
// daily buckets), so a caller can't force a huge number of Redis round trips.
const maxReportBuckets = 400

type redisUsageStore struct {
	rdb       *redis.Client
	retention string // display label only ("all-time" when empty)

	mu          sync.RWMutex
	rateLimits  map[string]map[string]config.RateLimitConfig
	modelLimits map[string]map[string]config.RateLimitConfig
	budgetKeys  map[string]string // model → shared token-budget key (empty value = key by model)
}

// NewRedisUsageStore returns a UsageStore backed by Redis.
// retention is the human-readable retention string surfaced in API responses ("" = "all-time").
// rateLimits and modelLimits are the configured rate_limits and per-model
// token_limits maps, used to surface quotas alongside current usage; pass nil
// for either when not configured.
func NewRedisUsageStore(rdb *redis.Client, retention string, rateLimits, modelLimits map[string]map[string]config.RateLimitConfig, budgetKeys map[string]string) UsageStore {
	return &redisUsageStore{rdb: rdb, retention: retention, rateLimits: rateLimits, modelLimits: modelLimits, budgetKeys: budgetKeys}
}

// UpdateRateLimits swaps the rate_limits/modelLimits/budgetKeys maps consulted by
// buildWindowUsage/buildModelUsage.
func (s *redisUsageStore) UpdateRateLimits(rateLimits, modelLimits map[string]map[string]config.RateLimitConfig, budgetKeys map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimits = rateLimits
	s.modelLimits = modelLimits
	s.budgetKeys = budgetKeys
}

// resolveConfig looks up the RateLimitConfig for key/userType in limitsMap,
// falling back to the "*" wildcard user type when the specific type has no
// entry — mirrors ratelimit.Limiter.resolveFromMap's fallback behaviour.
func resolveConfig(limitsMap map[string]map[string]config.RateLimitConfig, key, userType string) (config.RateLimitConfig, bool) {
	keyLimits, ok := limitsMap[key]
	if !ok {
		return config.RateLimitConfig{}, false
	}
	if c, ok := keyLimits[userType]; ok {
		return c, true
	}
	if c, ok := keyLimits["*"]; ok {
		return c, true
	}
	return config.RateLimitConfig{}, false
}

// resolveRateLimitConfig looks up the service-level RateLimitConfig for svcType/userType.
func (s *redisUsageStore) resolveRateLimitConfig(svcType, userType string) (config.RateLimitConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return resolveConfig(s.rateLimits, svcType, userType)
}

// resolveModelLimitConfig looks up the per-model token RateLimitConfig for model/userType.
func (s *redisUsageStore) resolveModelLimitConfig(model, userType string) (config.RateLimitConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return resolveConfig(s.modelLimits, model, userType)
}

func (s *redisUsageStore) retentionLabel() string {
	if s.retention == "" {
		return "all-time"
	}
	return s.retention
}

// GetConsumerUsage returns cumulative + window usage for consumer across the
// given service types. Service types with zero data are omitted.
//
// All Redis reads are issued in two pipelined round trips per consumer rather
// than the ~(12 + 2·models) serial calls per service type the earlier
// implementation made. Phase 1 fetches last_active, the per-service tracked
// user type, and the user-type-independent counters (requests/jobs/
// processing_time + generic tokens); phase 2 fetches the user-type-dependent
// counters (llm tokens, rate-limit windows, per-model token windows) once the
// tracked tier is known from phase 1. This keeps GET /usage and GET /-/usage
// proportional to the number of consumers, not the number of individual
// counters — the dominant cost was round trips through HAProxy, not Redis load.
func (s *redisUsageStore) GetConsumerUsage(ctx context.Context, consumer, userType string, serviceTypes []string, modelsByType map[string][]string) (*ConsumerUsage, error) {
	out := &ConsumerUsage{
		Consumer:  consumer,
		Retention: s.retentionLabel(),
	}

	// ---- Phase 1: last_active, tracked user type, and user-type-independent
	// counters, in one pipelined round trip ----
	type phase1Svc struct {
		userType  *redis.StringCmd
		requests  *redis.FloatCmd
		jobs      *redis.FloatCmd
		procTime  *redis.FloatCmd
		genPrompt *redis.FloatCmd // generic (non-llm-proxy) tokens
		genComp   *redis.FloatCmd
	}
	p1 := s.rdb.Pipeline()
	lastActive := p1.ZScore(ctx, "usage:consumers", consumer)
	p1svc := make(map[string]*phase1Svc, len(serviceTypes))
	for _, svcType := range serviceTypes {
		p1svc[svcType] = &phase1Svc{
			userType:  p1.HGet(ctx, "usage:consumer:"+svcType+":usertype", consumer),
			requests:  p1.ZScore(ctx, "usage:consumer:"+svcType+":requests", consumer),
			jobs:      p1.ZScore(ctx, "usage:consumer:"+svcType+":jobs", consumer),
			procTime:  p1.ZScore(ctx, "usage:consumer:"+svcType+":processing_time", consumer),
			genPrompt: p1.ZScore(ctx, "usage:consumer:"+svcType+":tokens:prompt", consumer),
			genComp:   p1.ZScore(ctx, "usage:consumer:"+svcType+":tokens:completion", consumer),
		}
	}
	// Best-effort: redis.Nil (missing keys) is expected; a connection-level
	// failure leaves every command zeroed, mirroring the earlier behaviour of
	// ignoring per-key read errors rather than failing the whole response.
	if _, err := p1.Exec(ctx); err != nil && err != redis.Nil {
		slog.WarnContext(ctx, "usage store: phase-1 pipeline failed", "consumer", consumer, "error", err)
	}

	if score, err := lastActive.Result(); err == nil {
		t := time.Unix(int64(score), 0).UTC()
		out.LastActive = &t
	}

	// Resolve the effective tier per service type: the tier actually recorded
	// for this consumer+service (from real requests, via
	// UsageTracker.TrackUserType) takes precedence over the userType resolved
	// from the current /usage request's own identity — a consumer can hold a
	// different role/tier per service.
	effUserType := make(map[string]string, len(serviceTypes))
	for _, svcType := range serviceTypes {
		ut := userType
		if v, err := p1svc[svcType].userType.Result(); err == nil && v != "" {
			ut = v
		}
		effUserType[svcType] = ut
	}

	// ---- Phase 2: user-type-dependent counters (llm tokens, rate-limit
	// windows, per-model token windows), in one pipelined round trip ----
	type modelCmd struct {
		model     string
		budgetKey string
		counter   intTTL
	}
	type phase2Svc struct {
		llmPrompt *redis.FloatCmd
		llmComp   *redis.FloatCmd
		rl        intTTL
		trl       intTTL
		ptrl      intTTL
		models    []modelCmd
	}
	p2 := s.rdb.Pipeline()
	getIntTTL := func(key string) intTTL {
		return intTTL{get: p2.Get(ctx, key), ttl: p2.TTL(ctx, key)}
	}
	p2svc := make(map[string]*phase2Svc, len(serviceTypes))
	for _, svcType := range serviceTypes {
		ut := effUserType[svcType]
		c := &phase2Svc{
			rl:   getIntTTL(fmt.Sprintf("rl:%s:%s:%s", consumer, svcType, ut)),
			trl:  getIntTTL(fmt.Sprintf("trl:%s:%s:%s", consumer, svcType, ut)),
			ptrl: getIntTTL(fmt.Sprintf("ptrl:%s:%s:%s", consumer, svcType, ut)),
		}
		// LLM tokens are keyed by tier, so they belong to phase 2.
		if svcType == "llm" {
			c.llmPrompt = p2.ZScore(ctx, "llm:consumer:tokens:"+ut+":prompt", consumer)
			c.llmComp = p2.ZScore(ctx, "llm:consumer:tokens:"+ut+":completion", consumer)
		}
		for _, model := range modelsByType[svcType] {
			// The counter is keyed by the budget key (the shared group when
			// configured, else the model name) so grouped aliases report the
			// shared total, not 0.
			s.mu.RLock()
			budgetKey := s.budgetKeys[model]
			s.mu.RUnlock()
			if budgetKey == "" {
				budgetKey = model
			}
			c.models = append(c.models, modelCmd{
				model:     model,
				budgetKey: budgetKey,
				counter:   getIntTTL(fmt.Sprintf("trl:%s:model:%s:%s", consumer, budgetKey, ut)),
			})
		}
		p2svc[svcType] = c
	}
	if _, err := p2.Exec(ctx); err != nil && err != redis.Nil {
		slog.WarnContext(ctx, "usage store: phase-2 pipeline failed", "consumer", consumer, "error", err)
	}

	// ---- Assemble per service type, skipping types with no recorded data ----
	for _, svcType := range serviceTypes {
		p1c := p1svc[svcType]
		p2c := p2svc[svcType]
		ut := effUserType[svcType]

		requests := int64(p1c.requests.Val())
		jobs := int64(p1c.jobs.Val())
		procTime := p1c.procTime.Val()

		// LLM tokens (tier-keyed) take precedence; generic tokens are only
		// consulted when the llm keys found nothing, so the two key formats
		// never both contribute to the same response.
		var tokens *TokenUsage
		if svcType == "llm" {
			prompt := int64(p2c.llmPrompt.Val())
			completion := int64(p2c.llmComp.Val())
			if prompt > 0 || completion > 0 {
				tokens = &TokenUsage{Prompt: prompt, Completion: completion}
			}
		}
		if tokens == nil {
			prompt := int64(p1c.genPrompt.Val())
			completion := int64(p1c.genComp.Val())
			if prompt > 0 || completion > 0 {
				tokens = &TokenUsage{Prompt: prompt, Completion: completion}
			}
		}

		if requests == 0 && jobs == 0 && procTime == 0 && tokens == nil {
			continue // no data for this service type
		}

		rlVal, rlTTL := p2c.rl.read()
		trlVal, _ := p2c.trl.read()
		ptrlVal, _ := p2c.ptrl.read()
		window := s.buildWindowUsage(svcType, ut, rlVal, trlVal, ptrlVal, rlTTL)

		var modelUsages []ModelUsage
		for _, mc := range p2c.models {
			mTokens, mTTL := mc.counter.read()
			if mu := s.buildModelUsage(mc.model, mc.budgetKey, ut, mTokens, mTTL); mu != nil {
				modelUsages = append(modelUsages, *mu)
			}
		}

		out.Usage = append(out.Usage, ServiceUsage{
			ServiceType: svcType,
			Total: TotalUsage{
				Requests:       requests,
				Jobs:           jobs,
				ProcessingTime: procTime,
				Tokens:         tokens,
			},
			Window:   window,
			Models:   modelUsages,
			UserType: ut,
		})
	}
	return out, nil
}

// buildWindowUsage assembles the rate-limit window block from pre-fetched
// counter values alongside the configured quota (from rate_limits) usage is
// measured against. Returns nil when there's neither window data nor a
// configured quota to show. Pure: no Redis I/O.
func (s *redisUsageStore) buildWindowUsage(svcType, userType string, rlVal, trlVal, ptrlVal int64, rlTTL time.Duration) *WindowUsage {
	rlCfg, hasQuota := s.resolveRateLimitConfig(svcType, userType)
	// rate/token_rate/processing_time of 0 is the documented sentinel for "no
	// limit" (see ratelimit.Check/CheckTokens/CheckProcessingTime), under which
	// the matching Redis counter is never written. Track per-dimension whether
	// a limit is actually enforced, rather than just "does a RateLimitConfig
	// entry exist at all", so an unlimited dimension neither counts toward "is
	// there anything to show" nor gets a *_period with no matching *_limit.
	hasRequestQuota := hasQuota && rlCfg.Rate > 0
	hasTokenQuota := hasQuota && rlCfg.TokenRate > 0
	hasProcTimeQuota := hasQuota && rlCfg.ProcessingTime > 0

	if rlVal == 0 && trlVal == 0 && ptrlVal == 0 && !hasRequestQuota && !hasTokenQuota && !hasProcTimeQuota {
		return nil
	}

	w := &WindowUsage{
		Requests:       rlVal,
		Tokens:         trlVal,
		ProcessingTime: float64(ptrlVal),
	}
	if rlTTL > 0 {
		resetAt := time.Now().Add(rlTTL).UTC()
		w.ResetAt = &resetAt
	}
	if hasRequestQuota {
		w.RequestLimit = int64(rlCfg.Rate)
		w.RequestPeriod = rlCfg.Period
	}
	if hasTokenQuota {
		w.TokenLimit = int64(rlCfg.TokenRate)
		w.TokenPeriod = rlCfg.TokenPeriod
	}
	if hasProcTimeQuota {
		w.ProcessingTimeLimit = int64(rlCfg.ProcessingTime)
		w.ProcessingTimePeriod = rlCfg.ProcessingPeriod
	}
	return w
}

// buildModelUsage assembles a per-model token-window row from a pre-fetched
// counter value alongside the configured per-model quota (services[].token_limits).
// budgetKey is the counter's budget key (the shared group when configured, else
// the model name). Returns nil when there's neither window data nor a configured
// quota for this model. Pure: no Redis I/O.
func (s *redisUsageStore) buildModelUsage(model, budgetKey, userType string, tokens int64, ttl time.Duration) *ModelUsage {
	cfg, hasQuota := s.resolveModelLimitConfig(model, userType)
	hasTokenQuota := hasQuota && cfg.TokenRate > 0
	if tokens == 0 && !hasTokenQuota {
		return nil
	}

	mu := &ModelUsage{
		Model:  model,
		Tokens: tokens,
	}
	if budgetKey != model {
		mu.BudgetGroup = budgetKey // shared across aliases; model is the representative
	}
	if ttl > 0 {
		resetAt := time.Now().Add(ttl).UTC()
		mu.ResetAt = &resetAt
	}
	if hasTokenQuota {
		mu.TokenLimit = int64(cfg.TokenRate)
		mu.TokenPeriod = cfg.TokenPeriod
	}
	return mu
}

// ListConsumers returns consumers sorted by most-recently-active (desc),
// paginated from usage:consumers.
func (s *redisUsageStore) ListConsumers(ctx context.Context, limit, offset int64) ([]string, int64, error) {
	total, _ := s.rdb.ZCard(ctx, "usage:consumers").Result()
	members, _ := s.rdb.ZRevRange(ctx, "usage:consumers", offset, offset+limit-1).Result()
	return members, total, nil
}

// ListConsumersByType returns consumers sorted by request count desc for the
// given service type, paginated from usage:consumer:{type}:requests.
func (s *redisUsageStore) ListConsumersByType(ctx context.Context, serviceType string, limit, offset int64) ([]string, int64, error) {
	key := "usage:consumer:" + serviceType + ":requests"
	total, _ := s.rdb.ZCard(ctx, key).Result()
	members, _ := s.rdb.ZRevRange(ctx, key, offset, offset+limit-1).Result()
	return members, total, nil
}

// GetUsageReport returns cross-consumer calendar-aligned totals for
// serviceType, reading the usage:agg:* counters written by
// UsageTracker.trackPeriodAggregate. Buckets with no data are returned as
// zeroed entries rather than omitted, so callers get a contiguous series.
func (s *redisUsageStore) GetUsageReport(ctx context.Context, serviceType, period string, from, to time.Time) (*UsageReport, error) {
	if !isValidPeriod(period) {
		return nil, fmt.Errorf("usage report: invalid period %q", period)
	}
	if estimatedBucketCount(period, from, to) > maxReportBuckets {
		return nil, ErrTooManyBuckets
	}
	buckets := walkBuckets(period, from, to)
	if len(buckets) > maxReportBuckets {
		return nil, ErrTooManyBuckets
	}

	metrics := []string{"requests", "jobs", "processing_time", "tokens_prompt", "tokens_completion"}
	pipe := s.rdb.Pipeline()
	cmds := make([][]*redis.StringCmd, len(buckets))
	for i, bucket := range buckets {
		cmds[i] = make([]*redis.StringCmd, len(metrics))
		for j, metric := range metrics {
			key := "usage:agg:" + serviceType + ":" + metric + ":" + period + ":" + bucket
			cmds[i][j] = pipe.Get(ctx, key)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("usage report: pipeline exec: %w", err)
	}

	out := &UsageReport{
		ServiceType: serviceType,
		Period:      period,
		From:        from.UTC().Format("2006-01-02"),
		To:          to.UTC().Format("2006-01-02"),
		Buckets:     make([]PeriodUsage, len(buckets)),
	}
	for i, bucket := range buckets {
		requests, _ := strconv.ParseInt(cmds[i][0].Val(), 10, 64)
		jobs, _ := strconv.ParseInt(cmds[i][1].Val(), 10, 64)
		procTime, _ := strconv.ParseFloat(cmds[i][2].Val(), 64)
		prompt, _ := strconv.ParseInt(cmds[i][3].Val(), 10, 64)
		completion, _ := strconv.ParseInt(cmds[i][4].Val(), 10, 64)

		pu := PeriodUsage{
			Bucket:         bucket,
			Requests:       requests,
			Jobs:           jobs,
			ProcessingTime: procTime,
		}
		if prompt > 0 || completion > 0 {
			pu.Tokens = &TokenUsage{Prompt: prompt, Completion: completion}
		}
		out.Buckets[i] = pu
	}
	return out, nil
}

// helpers

// intTTL holds the pipelined GET + TTL commands for a single integer counter,
// read back together after the pipeline executes.
type intTTL struct {
	get *redis.StringCmd
	ttl *redis.DurationCmd
}

// read returns the parsed counter value and its TTL. A missing key reads as
// (0, 0): the TTL is only meaningful when the GET succeeded.
func (c intTTL) read() (int64, time.Duration) {
	val, err := c.get.Result()
	if err != nil {
		return 0, 0
	}
	n, _ := strconv.ParseInt(val, 10, 64)
	return n, c.ttl.Val()
}
