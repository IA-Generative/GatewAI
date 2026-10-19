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
// getWindowUsage/getModelUsage.
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
func (s *redisUsageStore) GetConsumerUsage(ctx context.Context, consumer, userType string, serviceTypes []string, modelsByType map[string][]string) (*ConsumerUsage, error) {
	out := &ConsumerUsage{
		Consumer:  consumer,
		Retention: s.retentionLabel(),
	}

	// last_active from usage:consumers
	score, err := s.rdb.ZScore(ctx, "usage:consumers", consumer).Result()
	if err == nil {
		t := time.Unix(int64(score), 0).UTC()
		out.LastActive = &t
	}

	for _, svcType := range serviceTypes {
		svc, err := s.getServiceUsage(ctx, consumer, userType, svcType, modelsByType[svcType])
		if err != nil {
			slog.WarnContext(ctx, "usage store: failed to read service usage",
				"consumer", consumer, "service_type", svcType, "error", err)
			continue
		}
		if svc == nil {
			continue // no data for this service type
		}
		out.Usage = append(out.Usage, *svc)
	}
	return out, nil
}

func (s *redisUsageStore) getServiceUsage(ctx context.Context, consumer, userType, svcType string, models []string) (*ServiceUsage, error) {
	// The tier actually recorded for this consumer+service (from real requests,
	// via UsageTracker.TrackUserType) takes precedence over the userType
	// resolved from the current /usage request's own identity: a consumer can
	// hold a different role/tier per service, so the caller's own tier does not
	// necessarily apply to every service type being reported on.
	effectiveUserType := userType
	if tracked, ok := s.resolveTrackedUserType(ctx, consumer, svcType); ok {
		effectiveUserType = tracked
	}
	userType = effectiveUserType

	requests, _ := s.zscore(ctx, "usage:consumer:"+svcType+":requests", consumer)
	jobs, _ := s.zscore(ctx, "usage:consumer:"+svcType+":jobs", consumer)
	procTime, _ := s.zscoref(ctx, "usage:consumer:"+svcType+":processing_time", consumer)

	// LLM tokens come from the existing ConsumerTracker sorted sets.
	var tokens *TokenUsage
	if svcType == "llm" {
		prompt, _ := s.zscore(ctx, "llm:consumer:tokens:"+userType+":prompt", consumer)
		completion, _ := s.zscore(ctx, "llm:consumer:tokens:"+userType+":completion", consumer)
		if prompt > 0 || completion > 0 {
			tokens = &TokenUsage{Prompt: prompt, Completion: completion}
		}
	}

	// Generic (non-LLM-proxy) tokens come from the UsageTracker.TrackTokens sorted
	// sets. Only consulted when the "llm" branch above found nothing, so the two
	// key formats never both contribute to the same response.
	if tokens == nil {
		promptGeneric, _ := s.zscore(ctx, "usage:consumer:"+svcType+":tokens:prompt", consumer)
		completionGeneric, _ := s.zscore(ctx, "usage:consumer:"+svcType+":tokens:completion", consumer)
		if promptGeneric > 0 || completionGeneric > 0 {
			tokens = &TokenUsage{Prompt: promptGeneric, Completion: completionGeneric}
		}
	}

	// Skip service types with no recorded data.
	if requests == 0 && jobs == 0 && procTime == 0 && tokens == nil {
		return nil, nil
	}

	total := TotalUsage{
		Requests:       requests,
		Jobs:           jobs,
		ProcessingTime: procTime,
		Tokens:         tokens,
	}

	window := s.getWindowUsage(ctx, consumer, userType, svcType)

	var modelUsages []ModelUsage
	for _, model := range models {
		if mu := s.getModelUsage(ctx, consumer, userType, model); mu != nil {
			modelUsages = append(modelUsages, *mu)
		}
	}

	return &ServiceUsage{
		ServiceType: svcType,
		Total:       total,
		Window:      window,
		Models:      modelUsages,
		UserType:    userType,
	}, nil
}

// resolveTrackedUserType reads back the tier consumer was actually evaluated
// under for svcType, recorded by UsageTracker.TrackUserType at real-request
// time. Returns ok=false when no such record exists yet (e.g. no request to
// this service has ever been made), in which case callers should fall back to
// the userType resolved from the current request's own identity.
func (s *redisUsageStore) resolveTrackedUserType(ctx context.Context, consumer, svcType string) (string, bool) {
	val, err := s.rdb.HGet(ctx, "usage:consumer:"+svcType+":usertype", consumer).Result()
	if err != nil || val == "" {
		return "", false
	}
	return val, true
}

// getWindowUsage reads rate-limit window counters for this consumer+service,
// alongside the configured quota (from rate_limits) it's measured against.
// Returns nil when there's neither window data nor a configured quota to show.
func (s *redisUsageStore) getWindowUsage(ctx context.Context, consumer, userType, svcType string) *WindowUsage {
	rlKey := fmt.Sprintf("rl:%s:%s:%s", consumer, svcType, userType)
	trlKey := fmt.Sprintf("trl:%s:%s:%s", consumer, svcType, userType)
	ptrlKey := fmt.Sprintf("ptrl:%s:%s:%s", consumer, svcType, userType)

	rlVal, rlTTL := s.getIntWithTTL(ctx, rlKey)
	trlVal, _ := s.getIntWithTTL(ctx, trlKey)
	ptrlVal, _ := s.getIntWithTTL(ctx, ptrlKey)

	rlCfg, hasQuota := s.resolveRateLimitConfig(svcType, userType)
	// rate/token_rate/processing_time of 0 is the documented sentinel for "no
	// limit" (see ratelimit.Check/CheckTokens/CheckProcessingTime), under which
	// the matching Redis counter is never written. Track per-dimension whether
	// a limit is actually enforced, rather than just "does a RateLimitConfig
	// entry exist at all", so an unlimited dimension neither counts toward
	// "is there anything to show" nor gets a *_period with no matching *_limit.
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

// getModelUsage reads the per-model token window counter (trl:{consumer}:model:{model}:{userType},
// written by ratelimit.Limiter.AddModelTokens) alongside the configured
// per-model quota (services[].token_limits). Returns nil when there's neither
// window data nor a configured quota for this model.
func (s *redisUsageStore) getModelUsage(ctx context.Context, consumer, userType, model string) *ModelUsage {
	// The counter is keyed by the budget key (the shared group when configured,
	// else the model name) so grouped aliases report the shared total, not 0.
	s.mu.RLock()
	budgetKey := s.budgetKeys[model]
	s.mu.RUnlock()
	if budgetKey == "" {
		budgetKey = model
	}
	key := fmt.Sprintf("trl:%s:model:%s:%s", consumer, budgetKey, userType)
	tokens, ttl := s.getIntWithTTL(ctx, key)

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

func (s *redisUsageStore) zscore(ctx context.Context, key, member string) (int64, error) {
	f, err := s.rdb.ZScore(ctx, key, member).Result()
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

func (s *redisUsageStore) zscoref(ctx context.Context, key, member string) (float64, error) {
	return s.rdb.ZScore(ctx, key, member).Result()
}

func (s *redisUsageStore) getIntWithTTL(ctx context.Context, key string) (int64, time.Duration) {
	val, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		return 0, 0
	}
	n, _ := strconv.ParseInt(val, 10, 64)
	ttl, _ := s.rdb.TTL(ctx, key).Result()
	return n, ttl
}
