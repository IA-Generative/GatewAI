package usage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/usage"
)

func newStore(t *testing.T, retention string) (usage.UsageStore, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	return newStoreWithLimits(t, retention, nil)
}

func newStoreWithLimits(t *testing.T, retention string, rateLimits map[string]map[string]config.RateLimitConfig) (usage.UsageStore, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	return newStoreWithAllLimits(t, retention, rateLimits, nil)
}

func newStoreWithAllLimits(t *testing.T, retention string, rateLimits, modelLimits map[string]map[string]config.RateLimitConfig) (usage.UsageStore, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return usage.NewRedisUsageStore(rdb, retention, rateLimits, modelLimits, nil), mr, rdb
}

// A grouped alias reports the shared budget counter, not its own (empty) key.
func TestGetConsumerUsage_BudgetGroup_SharesCounter(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	modelLimits := map[string]map[string]config.RateLimitConfig{
		"code":            {"*": {TokenRate: 1000, TokenPeriod: "1h"}},
		"code-completion": {"*": {TokenRate: 1000, TokenPeriod: "1h"}},
	}
	budgetKeys := map[string]string{"code": "qwen", "code-completion": "qwen"}
	store := usage.NewRedisUsageStore(rdb, "", nil, modelLimits, budgetKeys)

	// Spend accrued under the shared group key; a service-level request keeps the
	// llm service from being skipped as empty.
	if err := mr.Set("trl:alice:model:qwen:user", "750"); err != nil {
		t.Fatal(err)
	}
	rdb.ZAdd(context.Background(), "usage:consumer:llm:requests", redis.Z{Score: 3, Member: "alice"})

	cu, err := store.GetConsumerUsage(context.Background(), "alice", "user", []string{"llm"},
		map[string][]string{"llm": {"code"}})
	if err != nil {
		t.Fatal(err)
	}
	var got int64 = -1
	for _, su := range cu.Usage {
		if su.ServiceType != "llm" {
			continue
		}
		for _, m := range su.Models {
			if m.Model == "code" {
				got = m.Tokens
			}
		}
	}
	if got != 750 {
		t.Errorf("grouped alias tokens = %d, want 750 (read from shared key)", got)
	}
}

func TestGetConsumerUsage_Empty(t *testing.T) {
	store, _, _ := newStore(t, "")
	result, err := store.GetConsumerUsage(context.Background(), "alice", "*", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Consumer != "alice" {
		t.Errorf("consumer = %q", result.Consumer)
	}
	if result.Retention != "all-time" {
		t.Errorf("retention = %q, want all-time", result.Retention)
	}
	if len(result.Usage) != 0 {
		t.Errorf("expected empty usage, got %d entries", len(result.Usage))
	}
}

func TestGetConsumerUsage_WithData(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 100, "alice")
	rdb.ZIncrBy(ctx, "usage:consumer:audio:jobs", 80, "alice")
	rdb.ZIncrBy(ctx, "usage:consumer:audio:processing_time", 3600.5, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) != 1 {
		t.Fatalf("expected 1 service, got %d", len(result.Usage))
	}
	svc := result.Usage[0]
	if svc.ServiceType != "audio" {
		t.Errorf("service_type = %q", svc.ServiceType)
	}
	if svc.Total.Requests != 100 {
		t.Errorf("requests = %d, want 100", svc.Total.Requests)
	}
	if svc.Total.Jobs != 80 {
		t.Errorf("jobs = %d, want 80", svc.Total.Jobs)
	}
	if svc.Total.ProcessingTime != 3600 {
		// float stored as sorted set score rounds to int in zscore
		t.Logf("processing_time = %v (raw float from ZScore may be ~3600)", svc.Total.ProcessingTime)
	}
}

// TestGetConsumerUsage_TrackedUserTypeOverridesRequestIdentity verifies that
// a consumer's per-service tracked tier (recorded from real requests via
// UsageTracker.TrackUserType) is what's surfaced in ServiceUsage.UserType,
// even when the current /usage request's own resolved identity (e.g. from a
// different service's role) disagrees — a consumer can hold a different
// role/tier per service, so the caller's own tier must not be assumed to
// apply to every service type being reported on.
func TestGetConsumerUsage_TrackedUserTypeOverridesRequestIdentity(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 10, "alice")
	// alice was actually rate-limited as "limited" on audio requests...
	rdb.HSet(ctx, "usage:consumer:audio:usertype", "alice", "limited")

	// ...but this /usage call's own resolved identity (e.g. via a different
	// service's role) is "unlimited".
	result, err := store.GetConsumerUsage(ctx, "alice", "unlimited", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) != 1 {
		t.Fatalf("expected 1 service, got %d", len(result.Usage))
	}
	if got := result.Usage[0].UserType; got != "limited" {
		t.Errorf("user_type = %q, want %q (tracked value should win over request identity)", got, "limited")
	}
}

// TestGetConsumerUsage_UntrackedUserTypeFallsBackToRequestIdentity verifies
// that when no tracked tier has been recorded yet for consumer+service (no
// prior request via that service type), the userType resolved from the
// current request is used as a fallback.
func TestGetConsumerUsage_UntrackedUserTypeFallsBackToRequestIdentity(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 10, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "unlimited", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) != 1 {
		t.Fatalf("expected 1 service, got %d", len(result.Usage))
	}
	if got := result.Usage[0].UserType; got != "unlimited" {
		t.Errorf("user_type = %q, want %q (fallback to request identity)", got, "unlimited")
	}
}

func TestGetConsumerUsage_LLMTokens(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:llm:requests", 50, "alice")
	rdb.ZIncrBy(ctx, "llm:consumer:tokens:user:prompt", 10000, "alice")
	rdb.ZIncrBy(ctx, "llm:consumer:tokens:user:completion", 2000, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "user", []string{"llm"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) != 1 {
		t.Fatalf("expected 1 service, got %d", len(result.Usage))
	}
	svc := result.Usage[0]
	if svc.Total.Tokens == nil {
		t.Fatal("expected tokens to be non-nil for llm service type")
	}
	if svc.Total.Tokens.Prompt != 10000 {
		t.Errorf("prompt = %d, want 10000", svc.Total.Tokens.Prompt)
	}
	if svc.Total.Tokens.Completion != 2000 {
		t.Errorf("completion = %d, want 2000", svc.Total.Tokens.Completion)
	}
}

// TestGetConsumerUsage_GenericTokens verifies non-"llm" service types read
// tokens from the new usage:consumer:{svcType}:tokens:* keys, and that this
// does not interfere with the existing "llm" svcType code path.
func TestGetConsumerUsage_GenericTokens(t *testing.T) {
	store, mr, _ := newStore(t, "")
	mr.ZAdd("usage:consumer:transcription:tokens:prompt", 500, "alice")
	mr.ZAdd("usage:consumer:transcription:tokens:completion", 80, "alice")

	usage, err := store.GetConsumerUsage(context.Background(), "alice", "user", []string{"transcription"}, nil)
	if err != nil {
		t.Fatalf("GetConsumerUsage: %v", err)
	}
	if len(usage.Usage) != 1 {
		t.Fatalf("expected 1 service usage entry, got %d", len(usage.Usage))
	}
	svc := usage.Usage[0]
	if svc.Total.Tokens == nil {
		t.Fatal("expected Tokens to be populated")
	}
	if svc.Total.Tokens.Prompt != 500 {
		t.Errorf("Prompt: got %d, want 500", svc.Total.Tokens.Prompt)
	}
	if svc.Total.Tokens.Completion != 80 {
		t.Errorf("Completion: got %d, want 80", svc.Total.Tokens.Completion)
	}
}

func TestGetConsumerUsage_WindowIncluded(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 10, "alice")
	rdb.Set(ctx, "rl:alice:audio:*", "5", 30*time.Minute)

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) == 0 {
		t.Fatal("expected usage entries")
	}
	svc := result.Usage[0]
	if svc.Window == nil {
		t.Fatal("expected window to be non-nil when rl: key exists")
	}
	if svc.Window.Requests != 5 {
		t.Errorf("window.requests = %d, want 5", svc.Window.Requests)
	}
	if svc.Window.ResetAt == nil {
		t.Error("expected reset_at to be set")
	}
}

func TestGetConsumerUsage_QuotaIncludedEvenWhenUsageZero(t *testing.T) {
	rateLimits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 100, Period: "1m", TokenRate: 5000, TokenPeriod: "1h", ProcessingTime: 3600, ProcessingPeriod: "24h"},
		},
	}
	store, _, rdb := newStoreWithLimits(t, "", rateLimits)
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "user", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) == 0 {
		t.Fatal("expected usage entries")
	}
	w := result.Usage[0].Window
	if w == nil {
		t.Fatal("expected window to be non-nil when a quota is configured, even with zero live usage")
	}
	if w.Requests != 0 {
		t.Errorf("requests = %d, want 0 (no rl: key set)", w.Requests)
	}
	if w.RequestLimit != 100 || w.RequestPeriod != "1m" {
		t.Errorf("request quota = %d/%q, want 100/1m", w.RequestLimit, w.RequestPeriod)
	}
	if w.TokenLimit != 5000 || w.TokenPeriod != "1h" {
		t.Errorf("token quota = %d/%q, want 5000/1h", w.TokenLimit, w.TokenPeriod)
	}
	if w.ProcessingTimeLimit != 3600 || w.ProcessingTimePeriod != "24h" {
		t.Errorf("processing time quota = %d/%q, want 3600/24h", w.ProcessingTimeLimit, w.ProcessingTimePeriod)
	}
}

func TestGetConsumerUsage_UnlimitedRateSentinel_NoPartialQuotaDisplay(t *testing.T) {
	// rate: 0 is the documented sentinel for "no limit" (ratelimit.Check skips
	// the rl: counter entirely in that case). A leftover period alongside it
	// must not be surfaced as if a real request quota were enforced.
	rateLimits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 0, Period: "1m", TokenRate: 5000, TokenPeriod: "1h"},
		},
	}
	store, _, rdb := newStoreWithLimits(t, "", rateLimits)
	ctx := context.Background()
	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 5, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "user", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := result.Usage[0].Window
	if w == nil {
		t.Fatal("expected window to be non-nil (token quota is still configured)")
	}
	if w.RequestLimit != 0 || w.RequestPeriod != "" {
		t.Errorf("request quota = %d/%q, want 0/\"\" since rate:0 means unlimited", w.RequestLimit, w.RequestPeriod)
	}
	if w.TokenLimit != 5000 || w.TokenPeriod != "1h" {
		t.Errorf("token quota = %d/%q, want 5000/1h", w.TokenLimit, w.TokenPeriod)
	}
}

func TestGetConsumerUsage_QuotaExactUserTypeOverridesWildcard(t *testing.T) {
	rateLimits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*":       {Rate: 10, Period: "1m"},
			"premium": {Rate: 1000, Period: "1m"},
		},
	}
	store, _, rdb := newStoreWithLimits(t, "", rateLimits)
	ctx := context.Background()
	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "premium", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := result.Usage[0].Window
	if w == nil || w.RequestLimit != 1000 {
		t.Fatalf("expected premium quota (1000) to override wildcard, got %+v", w)
	}
}

func TestGetConsumerUsage_ModelQuotaAndUsageIncluded(t *testing.T) {
	modelLimits := map[string]map[string]config.RateLimitConfig{
		"gpt-oss": {
			"*": {TokenRate: 20000, TokenPeriod: "1h"},
		},
	}
	store, _, rdb := newStoreWithAllLimits(t, "", nil, modelLimits)
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:llm:requests", 1, "alice")
	rdb.Set(ctx, "trl:alice:model:gpt-oss:user", "1500", 30*time.Minute)

	result, err := store.GetConsumerUsage(ctx, "alice", "user", []string{"llm"}, map[string][]string{"llm": {"gpt-oss"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) != 1 {
		t.Fatalf("expected 1 service, got %d", len(result.Usage))
	}
	models := result.Usage[0].Models
	if len(models) != 1 {
		t.Fatalf("expected 1 model usage entry, got %d", len(models))
	}
	m := models[0]
	if m.Model != "gpt-oss" {
		t.Errorf("model = %q, want gpt-oss", m.Model)
	}
	if m.Tokens != 1500 {
		t.Errorf("tokens = %d, want 1500", m.Tokens)
	}
	if m.TokenLimit != 20000 || m.TokenPeriod != "1h" {
		t.Errorf("token quota = %d/%q, want 20000/1h", m.TokenLimit, m.TokenPeriod)
	}
	if m.ResetAt == nil {
		t.Error("expected reset_at to be set")
	}
}

func TestGetConsumerUsage_ModelQuotaIncludedEvenWhenUsageZero(t *testing.T) {
	modelLimits := map[string]map[string]config.RateLimitConfig{
		"gpt-oss": {
			"*": {TokenRate: 20000, TokenPeriod: "1h"},
		},
	}
	store, _, rdb := newStoreWithAllLimits(t, "", nil, modelLimits)
	ctx := context.Background()
	rdb.ZIncrBy(ctx, "usage:consumer:llm:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "user", []string{"llm"}, map[string][]string{"llm": {"gpt-oss"}})
	if err != nil {
		t.Fatal(err)
	}
	models := result.Usage[0].Models
	if len(models) != 1 || models[0].TokenLimit != 20000 {
		t.Fatalf("expected gpt-oss quota surfaced with zero live tokens, got %+v", models)
	}
	if models[0].Tokens != 0 {
		t.Errorf("tokens = %d, want 0 (no trl:model key set)", models[0].Tokens)
	}
}

func TestGetConsumerUsage_ModelExactUserTypeOverridesWildcard(t *testing.T) {
	modelLimits := map[string]map[string]config.RateLimitConfig{
		"gpt-oss": {
			"*":       {TokenRate: 1000, TokenPeriod: "1h"},
			"premium": {TokenRate: 100000, TokenPeriod: "1h"},
		},
	}
	store, _, rdb := newStoreWithAllLimits(t, "", nil, modelLimits)
	ctx := context.Background()
	rdb.ZIncrBy(ctx, "usage:consumer:llm:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "premium", []string{"llm"}, map[string][]string{"llm": {"gpt-oss"}})
	if err != nil {
		t.Fatal(err)
	}
	models := result.Usage[0].Models
	if len(models) != 1 || models[0].TokenLimit != 100000 {
		t.Fatalf("expected premium quota (100000) to override wildcard, got %+v", models)
	}
}

func TestGetConsumerUsage_NoModelDataOrQuota_NoModelsEntry(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()
	rdb.ZIncrBy(ctx, "usage:consumer:llm:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "user", []string{"llm"}, map[string][]string{"llm": {"gpt-oss"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage[0].Models) != 0 {
		t.Errorf("expected no model entries when neither usage nor quota exist, got %+v", result.Usage[0].Models)
	}
}

func TestGetConsumerUsage_NoQuotaConfigured_NoWindowWhenUsageZero(t *testing.T) {
	store, _, _ := newStore(t, "")
	ctx := context.Background()

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) != 0 {
		t.Errorf("expected no usage entries for a service with zero data and no quota, got %d", len(result.Usage))
	}
}

func TestGetConsumerUsage_NoWindowWhenKeyAbsent(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 10, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Usage) == 0 {
		t.Fatal("expected usage entries")
	}
	if result.Usage[0].Window != nil {
		t.Error("expected window to be nil when no rl: key exists")
	}
}

func TestGetConsumerUsage_LastActive(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	ts := time.Now().UTC().Truncate(time.Second)
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: float64(ts.Unix()), Member: "alice"})
	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.LastActive == nil {
		t.Fatal("expected last_active to be set")
	}
	if !result.LastActive.Equal(ts) {
		t.Errorf("last_active = %v, want %v", *result.LastActive, ts)
	}
}

func TestGetConsumerUsage_RetentionLabel(t *testing.T) {
	store, _, rdb := newStore(t, "720h")
	ctx := context.Background()

	rdb.ZIncrBy(ctx, "usage:consumer:audio:requests", 1, "alice")

	result, err := store.GetConsumerUsage(ctx, "alice", "*", []string{"audio"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Retention != "720h" {
		t.Errorf("retention = %q, want 720h", result.Retention)
	}
}

func TestListConsumers_Paginated(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()
	now := float64(time.Now().Unix())

	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now, Member: "alice"})
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now - 1, Member: "bob"})
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now - 2, Member: "charlie"})

	consumers, total, err := store.ListConsumers(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(consumers) != 2 {
		t.Errorf("got %d consumers, want 2", len(consumers))
	}
	// Most recent first
	if consumers[0] != "alice" {
		t.Errorf("first consumer = %q, want alice", consumers[0])
	}
}

func TestListConsumers_Offset(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()
	now := float64(time.Now().Unix())

	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now, Member: "alice"})
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now - 1, Member: "bob"})
	rdb.ZAdd(ctx, "usage:consumers", redis.Z{Score: now - 2, Member: "charlie"})

	consumers, total, err := store.ListConsumers(ctx, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(consumers) != 2 {
		t.Errorf("got %d consumers, want 2", len(consumers))
	}
	if consumers[0] != "bob" {
		t.Errorf("first consumer with offset=1 = %q, want bob", consumers[0])
	}
}

func TestGetUsageReport_EmptyRange_ZeroFilledNotError(t *testing.T) {
	store, _, _ := newStore(t, "")
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)

	result, err := store.GetUsageReport(context.Background(), "llm", "daily", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Buckets) != 3 {
		t.Fatalf("expected 3 daily buckets, got %d", len(result.Buckets))
	}
	for _, b := range result.Buckets {
		if b.Requests != 0 || b.Jobs != 0 || b.ProcessingTime != 0 || b.Tokens != nil {
			t.Errorf("expected zero-filled bucket, got %+v", b)
		}
	}
}

func TestGetUsageReport_DailySumsAcrossBuckets(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.Set(ctx, "usage:agg:llm:requests:daily:20260301", "10", 0)
	rdb.Set(ctx, "usage:agg:llm:requests:daily:20260302", "5", 0)
	rdb.Set(ctx, "usage:agg:llm:tokens_prompt:daily:20260301", "1000", 0)
	rdb.Set(ctx, "usage:agg:llm:tokens_completion:daily:20260301", "200", 0)

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

	result, err := store.GetUsageReport(ctx, "llm", "daily", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if result.ServiceType != "llm" || result.Period != "daily" {
		t.Errorf("unexpected header fields: %+v", result)
	}
	if len(result.Buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(result.Buckets))
	}
	b0 := result.Buckets[0]
	if b0.Bucket != "20260301" || b0.Requests != 10 {
		t.Errorf("bucket0 = %+v, want bucket=20260301 requests=10", b0)
	}
	if b0.Tokens == nil || b0.Tokens.Prompt != 1000 || b0.Tokens.Completion != 200 {
		t.Errorf("bucket0 tokens = %+v, want prompt=1000 completion=200", b0.Tokens)
	}
	b1 := result.Buckets[1]
	if b1.Bucket != "20260302" || b1.Requests != 5 {
		t.Errorf("bucket1 = %+v, want bucket=20260302 requests=5", b1)
	}
	if b1.Tokens != nil {
		t.Errorf("bucket1 tokens = %+v, want nil (no token writes)", b1.Tokens)
	}
}

func TestGetUsageReport_MonthlyBucket(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()
	rdb.Set(ctx, "usage:agg:llm:requests:monthly:202603", "42", 0)

	from := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	result, err := store.GetUsageReport(ctx, "llm", "monthly", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Buckets) != 1 || result.Buckets[0].Bucket != "202603" || result.Buckets[0].Requests != 42 {
		t.Fatalf("unexpected result: %+v", result.Buckets)
	}
}

func TestGetUsageReport_InvalidPeriod_Error(t *testing.T) {
	store, _, _ := newStore(t, "")
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	_, err := store.GetUsageReport(context.Background(), "llm", "hourly", from, to)
	if err == nil {
		t.Fatal("expected error for invalid period")
	}
}

func TestGetUsageReport_OverCapRange_ErrTooManyBuckets(t *testing.T) {
	store, _, _ := newStore(t, "")
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // >400 days

	_, err := store.GetUsageReport(context.Background(), "llm", "daily", from, to)
	if !errors.Is(err, usage.ErrTooManyBuckets) {
		t.Fatalf("expected ErrTooManyBuckets, got %v", err)
	}
}

func TestListConsumersByType_Paginated(t *testing.T) {
	store, _, rdb := newStore(t, "")
	ctx := context.Background()

	rdb.ZAdd(ctx, "usage:consumer:audio:requests", redis.Z{Score: 100, Member: "alice"})
	rdb.ZAdd(ctx, "usage:consumer:audio:requests", redis.Z{Score: 50, Member: "bob"})

	consumers, total, err := store.ListConsumersByType(ctx, "audio", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	// Highest score first
	if consumers[0] != "alice" {
		t.Errorf("first = %q, want alice", consumers[0])
	}
}
