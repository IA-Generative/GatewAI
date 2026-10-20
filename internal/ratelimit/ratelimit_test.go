package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/ratelimit"
)

func newLimiter(t *testing.T, limits map[string]map[string]config.RateLimitConfig, consumerHeader, userTypeHeader string) (*ratelimit.Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return ratelimit.New(rdb, limits, nil, consumerHeader, userTypeHeader), mr
}

func TestCheck_NoConfiguredService_Allowed(t *testing.T) {
	l, _ := newLimiter(t, map[string]map[string]config.RateLimitConfig{}, "X-Consumer", "X-User-Type")
	r := httptest.NewRequest("POST", "/", nil)
	res, err := l.Check(context.Background(), r, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed when no config for service type")
	}
}

func TestCheck_UnlimitedRate_AlwaysAllowed(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"unlimited": {Rate: 0},
			"*":         {Rate: 2, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	for i := 0; i < 100; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "user1")
		r.Header.Set("X-User-Type", "unlimited")
		res, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("iteration %d: expected unlimited user to always be allowed", i)
		}
		if res.Limit != 0 {
			t.Fatalf("iteration %d: expected Limit=0 for unlimited, got %d", i, res.Limit)
		}
	}
}

func TestCheck_RateEnforced_AfterLimit(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 3, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	for i := 1; i <= 3; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "user1")
		res, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed {
			t.Fatalf("request %d should be allowed", i)
		}
		if res.Limit != 3 {
			t.Fatalf("request %d: expected Limit=3, got %d", i, res.Limit)
		}
		wantRemaining := 3 - i
		if res.Remaining != wantRemaining {
			t.Fatalf("request %d: expected Remaining=%d, got %d", i, wantRemaining, res.Remaining)
		}
	}

	// 4th request must be rejected.
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Consumer", "user1")
	res, err := l.Check(context.Background(), r, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("4th request should be rejected")
	}
	if res.Remaining != 0 {
		t.Fatalf("expected Remaining=0 on rejection, got %d", res.Remaining)
	}
	if res.ResetAfter <= 0 {
		t.Fatal("expected positive ResetAfter duration on rejection")
	}
}

func TestCheck_ExactMatchBeforeFallback(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"premium": {Rate: 10, Period: "1m"},
			"*":       {Rate: 1, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	// premium user gets rate 10, so 2 requests must both be allowed.
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("X-Consumer", "puser")
		r.Header.Set("X-User-Type", "premium")
		res, err := l.Check(context.Background(), r, "audio")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed {
			t.Fatalf("premium request %d should be allowed", i+1)
		}
	}

	// default user hits limit after 1 request.
	r1 := httptest.NewRequest("POST", "/", nil)
	r1.Header.Set("X-Consumer", "duser")
	res1, err := l.Check(context.Background(), r1, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if !res1.Allowed {
		t.Fatal("1st default request should be allowed")
	}

	r2 := httptest.NewRequest("POST", "/", nil)
	r2.Header.Set("X-Consumer", "duser")
	res2, err2 := l.Check(context.Background(), r2, "audio")
	if err2 != nil {
		t.Fatal(err2)
	}
	if res2.Allowed {
		t.Fatal("2nd default request should be rejected")
	}
}

func TestCheck_NoUserTypeHeader_UsesFallback(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 1, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Consumer", "u1")
	res, err := l.Check(context.Background(), r, "audio")
	if err != nil || !res.Allowed {
		t.Fatalf("first request should pass: allowed=%v err=%v", res.Allowed, err)
	}

	r2 := httptest.NewRequest("POST", "/", nil)
	r2.Header.Set("X-Consumer", "u1")
	res2, _ := l.Check(context.Background(), r2, "audio")
	if res2.Allowed {
		t.Fatal("second request should be rejected by fallback limit")
	}
}

func TestCheckTokens_NoConfig_Allowed(t *testing.T) {
	l, _ := newLimiter(t, nil, "", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	res, err := l.CheckTokens(context.Background(), r, "llm")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed when no config for service type")
	}
}

func TestCheckTokens_ZeroTokenRate_AlwaysAllowed(t *testing.T) {
	l, _ := newLimiter(t, map[string]map[string]config.RateLimitConfig{
		"llm": {"*": {TokenRate: 0, TokenPeriod: "1h"}},
	}, "", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	for i := 0; i < 5; i++ {
		res, err := l.CheckTokens(context.Background(), r, "llm")
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("iteration %d: expected allowed when token_rate=0", i)
		}
	}
}

func TestAddTokens_ThenCheckTokens_BudgetEnforced(t *testing.T) {
	const limit = 1000
	l, _ := newLimiter(t, map[string]map[string]config.RateLimitConfig{
		"llm": {"*": {TokenRate: limit, TokenPeriod: "1h"}},
	}, "", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	// Consume 900 — within budget.
	if err := l.AddTokens(context.Background(), r, "llm", 900); err != nil {
		t.Fatal(err)
	}

	res, err := l.CheckTokens(context.Background(), r, "llm")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed after 900/1000 tokens consumed")
	}
	if res.Limit != limit {
		t.Fatalf("expected Limit=%d, got %d", limit, res.Limit)
	}
	if res.Remaining != 100 {
		t.Fatalf("expected Remaining=100, got %d", res.Remaining)
	}

	// Consume 200 more → total 1100, over limit.
	if err := l.AddTokens(context.Background(), r, "llm", 200); err != nil {
		t.Fatal(err)
	}

	res, err = l.CheckTokens(context.Background(), r, "llm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("expected rejected after exceeding token budget")
	}
	if res.Remaining != 0 {
		t.Fatalf("expected Remaining=0 on rejection, got %d", res.Remaining)
	}
	if res.ResetAfter <= 0 {
		t.Fatal("expected positive ResetAfter on rejection")
	}
}

func TestAddTokens_SetsWindowTTL(t *testing.T) {
	l, mr := newLimiter(t, map[string]map[string]config.RateLimitConfig{
		"llm": {"*": {TokenRate: 5000, TokenPeriod: "1h"}},
	}, "", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	if err := l.AddTokens(context.Background(), r, "llm", 100); err != nil {
		t.Fatal(err)
	}

	ttl := mr.TTL("trl:anonymous:llm:*")
	if ttl <= time.Duration(0) {
		t.Fatalf("expected positive TTL after first AddTokens, got %v", ttl)
	}
}

// TestAddTokensFor_SameEffectAsAddTokens verifies AddTokensFor produces the
// same Redis-key effect as an equivalent AddTokens call for matching
// consumer/serviceType/userType/total inputs. This is a functional-equivalence
// guard, not a code-sharing guard: AddTokensFor is a self-contained method
// mirroring AddProcessingTime, not an extraction from AddTokens.
func TestAddTokensFor_SameEffectAsAddTokens(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"transcription": {"user": {TokenRate: 1000, TokenPeriod: "1h"}},
	}
	l, _ := newLimiter(t, limits, "X-Consumer-Username", "X-User-Type")

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
	req.Header.Set("X-Consumer-Username", "alice")
	req.Header.Set("X-User-Type", "user")
	if err := l.AddTokens(context.Background(), req, "transcription", 42); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	cr1, err := l.CheckTokens(context.Background(), req, "transcription")
	if err != nil {
		t.Fatalf("CheckTokens: %v", err)
	}

	l2, _ := newLimiter(t, limits, "X-Consumer-Username", "X-User-Type")
	if err := l2.AddTokensFor(context.Background(), "alice", "user", "transcription", 42); err != nil {
		t.Fatalf("AddTokensFor: %v", err)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
	req2.Header.Set("X-Consumer-Username", "alice")
	req2.Header.Set("X-User-Type", "user")
	cr2, err := l2.CheckTokens(context.Background(), req2, "transcription")
	if err != nil {
		t.Fatalf("CheckTokens: %v", err)
	}
	if cr1.Remaining != cr2.Remaining {
		t.Errorf("remaining mismatch: AddTokens path=%d, AddTokensFor path=%d", cr1.Remaining, cr2.Remaining)
	}
}

// TestAddTokensFor_ZeroTotal_NoOp verifies total <= 0 does nothing.
func TestAddTokensFor_ZeroTotal_NoOp(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"transcription": {"user": {TokenRate: 1000, TokenPeriod: "1h"}},
	}
	l, mr := newLimiter(t, limits, "X-Consumer-Username", "X-User-Type")
	if err := l.AddTokensFor(context.Background(), "alice", "user", "transcription", 0); err != nil {
		t.Fatalf("AddTokensFor: %v", err)
	}
	if mr.Exists("trl:alice:transcription:user") {
		t.Error("expected no Redis key to be created for zero total")
	}
}

// TestAddTokensFor_NoLimitConfigured_NoOp verifies a service type with no
// token_rate configured is a no-op (mirrors AddProcessingTime's behaviour).
func TestAddTokensFor_NoLimitConfigured_NoOp(t *testing.T) {
	l, mr := newLimiter(t, nil, "X-Consumer-Username", "X-User-Type")
	if err := l.AddTokensFor(context.Background(), "alice", "user", "transcription", 42); err != nil {
		t.Fatalf("AddTokensFor: %v", err)
	}
	if mr.Exists("trl:alice:transcription:user") {
		t.Error("expected no Redis key to be created when no limit is configured")
	}
}

func TestCheckTokens_UserTypeMatchAndFallback(t *testing.T) {
	l, _ := newLimiter(t, map[string]map[string]config.RateLimitConfig{
		"llm": {
			"sa": {TokenRate: 1_000_000, TokenPeriod: "1h"},
			"*":  {TokenRate: 10_000, TokenPeriod: "1h"},
		},
	}, "", "X-User-Type")

	// "sa" → exact match.
	rSA := httptest.NewRequest(http.MethodPost, "/", nil)
	rSA.Header.Set("X-User-Type", "sa")
	res, err := l.CheckTokens(context.Background(), rSA, "llm")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected sa user to be allowed")
	}
	if res.Limit != 1_000_000 {
		t.Fatalf("expected Limit=1_000_000 for sa, got %d", res.Limit)
	}

	// "user" not listed → falls back to "*".
	rUser := httptest.NewRequest(http.MethodPost, "/", nil)
	rUser.Header.Set("X-User-Type", "user")
	res, err = l.CheckTokens(context.Background(), rUser, "llm")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected unlisted user type to fall back to * and be allowed")
	}
	if res.Limit != 10_000 {
		t.Fatalf("expected Limit=10_000 for fallback, got %d", res.Limit)
	}
}

func TestCheck_RateLimitHeaders_ResetAfterPositive(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"*": {Rate: 5, Period: "1m"},
		},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Consumer", "u1")
	res, err := l.Check(context.Background(), r, "audio")
	if err != nil {
		t.Fatal(err)
	}
	if res.ResetAfter <= 0 {
		t.Fatalf("expected positive ResetAfter, got %v", res.ResetAfter)
	}
	if res.Limit != 5 {
		t.Fatalf("expected Limit=5, got %d", res.Limit)
	}
	if res.Remaining != 4 {
		t.Fatalf("expected Remaining=4, got %d", res.Remaining)
	}
}

func newLimiterWithModelLimits(t *testing.T, limits, modelLimits map[string]map[string]config.RateLimitConfig, consumerHeader, userTypeHeader string) (*ratelimit.Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return ratelimit.New(rdb, limits, modelLimits, consumerHeader, userTypeHeader), mr
}

func TestCheckModelTokens_NoConfig_Allowed(t *testing.T) {
	l, _ := newLimiterWithModelLimits(t, nil, nil, "", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	res, err := l.CheckModelTokens(context.Background(), r, "gpt-4o", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed when no model config")
	}
}

func TestAddModelTokens_ThenCheckModelTokens_BudgetEnforced(t *testing.T) {
	const limit = 1000
	l, _ := newLimiterWithModelLimits(t, nil, map[string]map[string]config.RateLimitConfig{
		"gpt-4o": {"*": {TokenRate: limit, TokenPeriod: "1h"}},
	}, "X-Consumer", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Consumer", "user1")

	if err := l.AddModelTokens(context.Background(), r, "gpt-4o", "", 900); err != nil {
		t.Fatal(err)
	}

	res, err := l.CheckModelTokens(context.Background(), r, "gpt-4o", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed after 900/1000 tokens consumed")
	}
	if res.Limit != limit {
		t.Fatalf("expected Limit=%d, got %d", limit, res.Limit)
	}
	if res.Remaining != 100 {
		t.Fatalf("expected Remaining=100, got %d", res.Remaining)
	}

	if err := l.AddModelTokens(context.Background(), r, "gpt-4o", "", 200); err != nil {
		t.Fatal(err)
	}

	res, err = l.CheckModelTokens(context.Background(), r, "gpt-4o", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("expected rejected after exceeding model token budget")
	}
	if res.Remaining != 0 {
		t.Fatalf("expected Remaining=0 on rejection, got %d", res.Remaining)
	}
	if res.ResetAfter <= 0 {
		t.Fatal("expected positive ResetAfter on rejection")
	}
}

func TestCheckModelTokens_CounterIsolatedFromServiceCounter(t *testing.T) {
	const limit = 100
	l, _ := newLimiterWithModelLimits(t,
		map[string]map[string]config.RateLimitConfig{
			"llm": {"*": {TokenRate: limit, TokenPeriod: "1h"}},
		},
		map[string]map[string]config.RateLimitConfig{
			"gpt-4o": {"*": {TokenRate: limit, TokenPeriod: "1h"}},
		},
		"X-Consumer", "X-User-Type",
	)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Consumer", "user1")

	// Exhaust the service-level token counter.
	if err := l.AddTokens(context.Background(), r, "llm", limit+1); err != nil {
		t.Fatal(err)
	}
	svcRes, _ := l.CheckTokens(context.Background(), r, "llm")
	if svcRes.Allowed {
		t.Fatal("service counter should be exhausted")
	}

	// Model counter must be independent.
	modRes, err := l.CheckModelTokens(context.Background(), r, "gpt-4o", "")
	if err != nil {
		t.Fatal(err)
	}
	if !modRes.Allowed {
		t.Fatal("model counter must be independent of service counter")
	}
}

func TestAddModelTokens_SetsWindowTTL(t *testing.T) {
	l, mr := newLimiterWithModelLimits(t, nil, map[string]map[string]config.RateLimitConfig{
		"gpt-4o": {"*": {TokenRate: 5000, TokenPeriod: "1h"}},
	}, "X-Consumer", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Consumer", "user1")

	if err := l.AddModelTokens(context.Background(), r, "gpt-4o", "", 100); err != nil {
		t.Fatal(err)
	}

	ttl := mr.TTL("trl:user1:model:gpt-4o:*")
	if ttl <= time.Duration(0) {
		t.Fatalf("expected positive TTL after first AddModelTokens, got %v", ttl)
	}
}

func TestCheckConcurrent(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {"*": {MaxConcurrent: 1}},
	}
	l, mr := newLimiter(t, limits, "X-Consumer", "X-User-Type")
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Consumer", "user1")

	seedConcJob := func(id, svcType, status string) {
		t.Helper()
		mr.Set("job:"+id, `{"status":"`+status+`","service_type":"`+svcType+`"}`)
		if _, err := mr.ZAdd("consumer:user1:jobs", 0, id); err != nil {
			t.Fatalf("ZAdd: %v", err)
		}
	}

	// 0 jobs, 0 in-flight → allowed; in-flight counter becomes 1.
	r0, err := l.CheckConcurrent(context.Background(), req, "audio")
	if err != nil || !r0.Allowed {
		t.Fatalf("first check: want allowed, got %+v err=%v", r0, err)
	}

	// 0 jobs, 1 in-flight → rejected (concurrent submitter is using the slot).
	r1, err := l.CheckConcurrent(context.Background(), req, "audio")
	if err != nil || r1.Allowed {
		t.Fatalf("second check (concurrent): want rejected, got %+v err=%v", r1, err)
	}

	// Release the in-flight slot (simulates SaveJob completing).
	if err := l.ReleaseSlot(context.Background(), req, "audio"); err != nil {
		t.Fatalf("ReleaseSlot: %v", err)
	}

	// 0 jobs, 0 in-flight → allowed again.
	r2, err := l.CheckConcurrent(context.Background(), req, "audio")
	if err != nil || !r2.Allowed {
		t.Fatalf("after release: want allowed, got %+v err=%v", r2, err)
	}
	if err := l.ReleaseSlot(context.Background(), req, "audio"); err != nil {
		t.Fatalf("ReleaseSlot: %v", err)
	}

	// 1 pending job in sorted set, 0 in-flight → rejected (active=1 >= max=1).
	seedConcJob("j1", "audio", "pending")
	r3, err := l.CheckConcurrent(context.Background(), req, "audio")
	if err != nil || r3.Allowed {
		t.Fatalf("1 pending job: want rejected, got %+v err=%v", r3, err)
	}

	// Job completes → 0 active, 0 in-flight → allowed.
	mr.Set("job:j1", `{"status":"completed","service_type":"audio"}`)
	r4, err := l.CheckConcurrent(context.Background(), req, "audio")
	if err != nil || !r4.Allowed {
		t.Fatalf("after completion: want allowed, got %+v err=%v", r4, err)
	}
	l.ReleaseSlot(context.Background(), req, "audio") //nolint

	// Jobs for a different service_type are not counted.
	seedConcJob("j2", "video", "pending")
	r5, err := l.CheckConcurrent(context.Background(), req, "audio")
	if err != nil || !r5.Allowed {
		t.Fatalf("cross-service job: want allowed, got %+v err=%v", r5, err)
	}
	l.ReleaseSlot(context.Background(), req, "audio") //nolint
}

func TestCheckProcessingTime(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {"*": {ProcessingTime: 100, ProcessingPeriod: "1h"}},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Consumer", "user1")

	r1, err := l.CheckProcessingTime(context.Background(), req, "audio")
	if err != nil || !r1.Allowed {
		t.Fatalf("initial: want allowed, got %+v err=%v", r1, err)
	}
	// consume 95.7s → stored as 96 (ceil)
	if err := l.AddProcessingTime(context.Background(), "user1", "*", "audio", 95.7); err != nil {
		t.Fatalf("AddProcessingTime: %v", err)
	}
	r2, err := l.CheckProcessingTime(context.Background(), req, "audio")
	if err != nil || !r2.Allowed {
		t.Fatalf("after 96s (limit 100): want allowed, got %+v err=%v", r2, err)
	}
	// consume 10 more → total 106, over limit
	if err := l.AddProcessingTime(context.Background(), "user1", "*", "audio", 10); err != nil {
		t.Fatalf("AddProcessingTime: %v", err)
	}
	r3, err := l.CheckProcessingTime(context.Background(), req, "audio")
	if err != nil || r3.Allowed {
		t.Fatalf("over budget: want rejected, got %+v err=%v", r3, err)
	}
}

// ── Policy-limits context tests ───────────────────────────────────────────────

// TestCheck_PolicyLimits_PerConsumerBudget verifies that WithPolicyLimits
// enforces a separate per-consumer request-rate budget on key rlp:{consumer}:{serviceType}.
func TestCheck_PolicyLimits_PerConsumerBudget(t *testing.T) {
	// No service-level rate_limits — only policy limits matter.
	l, mr := newLimiter(t, nil, "X-Consumer", "X-User-Type")
	policyCfg := &config.RateLimitConfig{Rate: 2, Period: "1m"}

	makeReq := func(consumer string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Consumer", consumer)
		return r
	}

	ctx := ratelimit.WithPolicyLimits(context.Background(), policyCfg)

	// consumer "alice": first 2 requests must be allowed.
	for i := 1; i <= 2; i++ {
		res, err := l.Check(ctx, makeReq("alice"), "llm")
		if err != nil {
			t.Fatalf("alice req %d: unexpected error: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("alice req %d: expected allowed (policy rate=2), got rejected", i)
		}
	}
	// 3rd request must be rejected.
	res, err := l.Check(ctx, makeReq("alice"), "llm")
	if err != nil {
		t.Fatalf("alice req 3: unexpected error: %v", err)
	}
	if res.Allowed {
		t.Fatal("alice req 3: expected rejected after exceeding policy budget")
	}

	// "bob" has their own independent budget.
	for i := 1; i <= 2; i++ {
		res, err := l.Check(ctx, makeReq("bob"), "llm")
		if err != nil {
			t.Fatalf("bob req %d: unexpected error: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("bob req %d: expected allowed (independent budget)", i)
		}
	}

	// Verify the Redis key format.
	aliceKey := "rlp:alice:llm"
	if v, err := mr.Get(aliceKey); err != nil || v == "" {
		t.Errorf("expected key %q to exist in Redis, got %q err=%v", aliceKey, v, err)
	}
	bobKey := "rlp:bob:llm"
	if v, err := mr.Get(bobKey); err != nil || v == "" {
		t.Errorf("expected key %q to exist in Redis, got %q err=%v", bobKey, v, err)
	}
}

// TestCheck_PolicyLimits_AnonymousSkipped verifies that anonymous consumers
// bypass the policy-level rate check entirely (no Redis key written).
func TestCheck_PolicyLimits_AnonymousSkipped(t *testing.T) {
	l, mr := newLimiter(t, nil, "X-Consumer", "X-User-Type")
	policyCfg := &config.RateLimitConfig{Rate: 1, Period: "1m"}
	ctx := ratelimit.WithPolicyLimits(context.Background(), policyCfg)

	// No consumer header → anonymous.
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		res, err := l.Check(ctx, r, "llm")
		if err != nil {
			t.Fatalf("anon req %d: unexpected error: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("anon req %d: expected allowed (anonymous skips policy check)", i)
		}
	}

	// No policy key should have been written.
	if v, _ := mr.Get("rlp:anonymous:llm"); v != "" {
		t.Errorf("expected no rlp:anonymous:llm key, but got %q", v)
	}
}

// TestCheck_NoPolicyLimitsInCtx_ExistingBehaviorUnchanged confirms that when
// no policy limits are stored in ctx the existing rate_limits behavior is unchanged.
func TestCheck_NoPolicyLimitsInCtx_ExistingBehaviorUnchanged(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {"*": {Rate: 2, Period: "1m"}},
	}
	l, _ := newLimiter(t, limits, "X-Consumer", "X-User-Type")

	r1 := httptest.NewRequest(http.MethodPost, "/", nil)
	r1.Header.Set("X-Consumer", "user1")
	res, err := l.Check(context.Background(), r1, "audio")
	if err != nil || !res.Allowed {
		t.Fatalf("req 1: want allowed, got %+v err=%v", res, err)
	}

	r2 := httptest.NewRequest(http.MethodPost, "/", nil)
	r2.Header.Set("X-Consumer", "user1")
	res, err = l.Check(context.Background(), r2, "audio")
	if err != nil || !res.Allowed {
		t.Fatalf("req 2: want allowed, got %+v err=%v", res, err)
	}

	r3 := httptest.NewRequest(http.MethodPost, "/", nil)
	r3.Header.Set("X-Consumer", "user1")
	res, _ = l.Check(context.Background(), r3, "audio")
	if res.Allowed {
		t.Fatal("req 3: want rejected by existing rate_limits")
	}
}

// TestCheckTokens_PolicyLimits_BudgetEnforced verifies that AddTokens beyond
// the policy TokenRate causes CheckTokens to reject.
func TestCheckTokens_PolicyLimits_BudgetEnforced(t *testing.T) {
	// No service-level token limits; only policy limits.
	l, _ := newLimiter(t, nil, "X-Consumer", "X-User-Type")
	policyCfg := &config.RateLimitConfig{TokenRate: 500, TokenPeriod: "1h"}
	ctx := ratelimit.WithPolicyLimits(context.Background(), policyCfg)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Consumer", "user1")

	// Consume 400 tokens — within policy budget.
	if err := l.AddTokens(ctx, req, "llm", 400); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	res, err := l.CheckTokens(ctx, req, "llm")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed after 400/500 policy tokens consumed")
	}

	// Consume 200 more → total 600, over policy limit of 500.
	if err := l.AddTokens(ctx, req, "llm", 200); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	res, err = l.CheckTokens(ctx, req, "llm")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("expected rejected after exceeding policy token budget")
	}
	if res.ResetAfter <= 0 {
		t.Fatal("expected positive ResetAfter on policy token rejection")
	}
}

// TestCheckTokens_PolicyLimits_AnonymousSkipped verifies that anonymous
// consumers bypass the policy token layer.
func TestCheckTokens_PolicyLimits_AnonymousSkipped(t *testing.T) {
	l, _ := newLimiter(t, nil, "X-Consumer", "X-User-Type")
	policyCfg := &config.RateLimitConfig{TokenRate: 1, TokenPeriod: "1h"}
	ctx := ratelimit.WithPolicyLimits(context.Background(), policyCfg)

	// No consumer header → anonymous.
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	// Add many tokens — should not be counted.
	if err := l.AddTokens(ctx, req, "llm", 1000); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	res, err := l.CheckTokens(ctx, req, "llm")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed: anonymous consumer should skip policy token check")
	}
}

func TestResetQuota_DeletesRateTokenAndProcessingKeysAcrossUserTypes(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {
			"premium": {Rate: 10, Period: "1m", TokenRate: 1000, TokenPeriod: "1m", ProcessingTime: 60, ProcessingPeriod: "1m"},
			"*":       {Rate: 5, Period: "1m"},
		},
	}
	l, mr := newLimiter(t, limits, "X-Consumer", "X-User-Type")
	ctx := context.Background()

	rPremium := httptest.NewRequest("POST", "/", nil)
	rPremium.Header.Set("X-Consumer", "acme")
	rPremium.Header.Set("X-User-Type", "premium")
	if _, err := l.Check(ctx, rPremium, "audio"); err != nil {
		t.Fatal(err)
	}
	if err := l.AddTokens(ctx, rPremium, "audio", 100); err != nil {
		t.Fatal(err)
	}
	if err := l.AddProcessingTime(ctx, "acme", "premium", "audio", 5); err != nil {
		t.Fatal(err)
	}

	rDefault := httptest.NewRequest("POST", "/", nil)
	rDefault.Header.Set("X-Consumer", "acme")
	if _, err := l.Check(ctx, rDefault, "audio"); err != nil {
		t.Fatal(err)
	}

	policyCfg := &config.RateLimitConfig{Rate: 3, Period: "1m", TokenRate: 500, TokenPeriod: "1m"}
	ctxPolicy := ratelimit.WithPolicyLimits(ctx, policyCfg)
	if _, err := l.Check(ctxPolicy, rPremium, "audio"); err != nil {
		t.Fatal(err)
	}
	if err := l.AddTokens(ctxPolicy, rPremium, "audio", 50); err != nil {
		t.Fatal(err)
	}

	wantKeys := []string{
		"rl:acme:audio:premium",
		"rl:acme:audio:*",
		"trl:acme:audio:premium",
		"ptrl:acme:audio:premium",
		"rlp:acme:audio",
		"trlp:acme:audio",
	}
	for _, k := range wantKeys {
		if !mr.Exists(k) {
			t.Fatalf("setup: expected key %q to exist before reset", k)
		}
	}

	deleted, err := l.ResetQuota(ctx, "acme", "audio", "")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != len(wantKeys) {
		t.Fatalf("deleted = %d, want %d", deleted, len(wantKeys))
	}
	for _, k := range wantKeys {
		if mr.Exists(k) {
			t.Fatalf("expected key %q to be deleted by ResetQuota", k)
		}
	}
}

func TestResetQuota_NoKeys_ReturnsZero(t *testing.T) {
	l, _ := newLimiter(t, map[string]map[string]config.RateLimitConfig{}, "X-Consumer", "X-User-Type")
	deleted, err := l.ResetQuota(context.Background(), "acme", "audio", "")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
}

// TestResetQuota_WithModel_DeletesPerModelTokenBudget verifies that passing
// a non-empty model also clears that consumer's trl:{consumer}:model:{model}:*
// keys (set by AddModelTokens), which the service_type-scoped patterns never match.
func TestResetQuota_WithModel_DeletesPerModelTokenBudget(t *testing.T) {
	modelLimits := map[string]map[string]config.RateLimitConfig{
		"whisper-large-v3": {"*": {TokenRate: 1000, TokenPeriod: "1m"}},
	}
	l, mr := newLimiterWithModelLimits(t, nil, modelLimits, "X-Consumer", "X-User-Type")
	ctx := context.Background()

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Consumer", "acme")
	if err := l.AddModelTokens(ctx, req, "whisper-large-v3", "", 100); err != nil {
		t.Fatal(err)
	}

	modelKey := "trl:acme:model:whisper-large-v3:*"
	if !mr.Exists(modelKey) {
		t.Fatalf("setup: expected key %q to exist before reset", modelKey)
	}

	// A reset without model still misses the per-model key.
	if _, err := l.ResetQuota(ctx, "acme", "audio", ""); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists(modelKey) {
		t.Fatal("expected per-model key to survive a reset with no model param")
	}

	deleted, err := l.ResetQuota(ctx, "acme", "audio", "whisper-large-v3")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if mr.Exists(modelKey) {
		t.Fatal("expected per-model key to be deleted once model param is set")
	}
}

func TestResetQuota_DoesNotAffectOtherConsumersOrServiceTypes(t *testing.T) {
	limits := map[string]map[string]config.RateLimitConfig{
		"audio": {"*": {Rate: 5, Period: "1m"}},
		"video": {"*": {Rate: 5, Period: "1m"}},
	}
	l, mr := newLimiter(t, limits, "X-Consumer", "X-User-Type")
	ctx := context.Background()

	rAcme := httptest.NewRequest("POST", "/", nil)
	rAcme.Header.Set("X-Consumer", "acme")
	if _, err := l.Check(ctx, rAcme, "audio"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Check(ctx, rAcme, "video"); err != nil {
		t.Fatal(err)
	}

	rOther := httptest.NewRequest("POST", "/", nil)
	rOther.Header.Set("X-Consumer", "other")
	if _, err := l.Check(ctx, rOther, "audio"); err != nil {
		t.Fatal(err)
	}

	if _, err := l.ResetQuota(ctx, "acme", "audio", ""); err != nil {
		t.Fatal(err)
	}

	if mr.Exists("rl:acme:audio:*") {
		t.Fatal("expected acme's audio key to be deleted")
	}
	if !mr.Exists("rl:acme:video:*") {
		t.Fatal("acme's video key should be untouched by an audio-scoped reset")
	}
	if !mr.Exists("rl:other:audio:*") {
		t.Fatal("other consumer's audio key should be untouched")
	}
}

// ── CheckBackendPool ─────────────────────────────────────────────────────────

func TestCheckBackendPool_NoLimitsConfigured_Allowed(t *testing.T) {
	l, _ := newLimiter(t, nil, "X-Consumer", "X-User-Type")
	res, err := l.CheckBackendPool(context.Background(), "pool-a", "http://a:8000")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed when no pool limits configured")
	}
}

func TestCheckBackendPool_PoolOnlyLimit_Enforced(t *testing.T) {
	l, _ := newLimiter(t, nil, "X-Consumer", "X-User-Type")
	l.SetPoolLimits(
		map[string]config.RateLimitConfig{"pool-a": {Rate: 2, Period: "1m"}},
		nil,
	)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		res, err := l.CheckBackendPool(ctx, "pool-a", "http://a:8000")
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("iteration %d: expected allowed within pool budget", i)
		}
	}
	res, err := l.CheckBackendPool(ctx, "pool-a", "http://a:8000")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("expected pool-wide budget to reject the 3rd attempt")
	}
	// A different member URL shares the same exhausted pool-wide budget.
	res, err = l.CheckBackendPool(ctx, "pool-a", "http://b:8000")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("expected pool-wide budget to reject regardless of member")
	}
}

func TestCheckBackendPool_MemberOnlyLimit_Enforced(t *testing.T) {
	l, _ := newLimiter(t, nil, "X-Consumer", "X-User-Type")
	l.SetPoolLimits(
		nil,
		map[string]map[string]config.RateLimitConfig{
			"pool-a": {"http://a:8000": {Rate: 1, Period: "1m"}},
		},
	)
	ctx := context.Background()
	res, err := l.CheckBackendPool(ctx, "pool-a", "http://a:8000")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected first attempt allowed")
	}
	res, err = l.CheckBackendPool(ctx, "pool-a", "http://a:8000")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("expected member budget to reject the 2nd attempt")
	}
	// A sibling member with no configured limit of its own is unaffected.
	res, err = l.CheckBackendPool(ctx, "pool-a", "http://b:8000")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected sibling member without its own limit to be allowed")
	}
}

func TestCheckBackendPool_PoolAndMemberBothConfigured_EitherCanReject(t *testing.T) {
	l, _ := newLimiter(t, nil, "X-Consumer", "X-User-Type")
	l.SetPoolLimits(
		map[string]config.RateLimitConfig{"pool-a": {Rate: 100, Period: "1m"}},
		map[string]map[string]config.RateLimitConfig{
			"pool-a": {"http://a:8000": {Rate: 1, Period: "1m"}},
		},
	)
	ctx := context.Background()
	if res, err := l.CheckBackendPool(ctx, "pool-a", "http://a:8000"); err != nil || !res.Allowed {
		t.Fatalf("expected first attempt allowed, got %+v err=%v", res, err)
	}
	// Pool budget has plenty of room, but the tighter member budget rejects.
	res, err := l.CheckBackendPool(ctx, "pool-a", "http://a:8000")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("expected the tighter member budget to reject even though pool budget has room")
	}
}

func TestCheckBackendPool_RedisError_FailsOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	l := ratelimit.New(rdb, nil, nil, "X-Consumer", "X-User-Type")
	l.SetPoolLimits(map[string]config.RateLimitConfig{"pool-a": {Rate: 1, Period: "1m"}}, nil)

	mr.Close() // force Redis errors
	res, err := l.CheckBackendPool(context.Background(), "pool-a", "http://a:8000")
	if err != nil {
		t.Fatalf("expected fail-open (no error), got %v", err)
	}
	if !res.Allowed {
		t.Fatal("expected fail-open to allow the request when Redis is unreachable")
	}
}

// Two models sharing a budget_group draw from one per-consumer counter.
func TestModelTokens_BudgetGroup_SharesCounter(t *testing.T) {
	const limit = 1000
	l, _ := newLimiterWithModelLimits(t, nil, map[string]map[string]config.RateLimitConfig{
		"code":            {"*": {TokenRate: limit, TokenPeriod: "1h"}},
		"code-completion": {"*": {TokenRate: limit, TokenPeriod: "1h"}},
	}, "X-Consumer", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Consumer", "user1")

	if err := l.AddModelTokens(context.Background(), r, "code", "qwen", 900); err != nil {
		t.Fatal(err)
	}
	// A different alias, same budget_group → sees the 900 already spent.
	res, err := l.CheckModelTokens(context.Background(), r, "code-completion", "qwen")
	if err != nil {
		t.Fatal(err)
	}
	if res.Remaining != 100 {
		t.Errorf("shared budget: remaining = %d, want 100", res.Remaining)
	}
}

// With budget_group unset the counter is per-alias and keyed by the model name —
// the pre-existing behaviour, unchanged.
func TestModelTokens_NoBudgetGroup_IsolatedPerModel(t *testing.T) {
	const limit = 1000
	l, _ := newLimiterWithModelLimits(t, nil, map[string]map[string]config.RateLimitConfig{
		"code":            {"*": {TokenRate: limit, TokenPeriod: "1h"}},
		"code-completion": {"*": {TokenRate: limit, TokenPeriod: "1h"}},
	}, "X-Consumer", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Consumer", "user1")

	if err := l.AddModelTokens(context.Background(), r, "code", "", 900); err != nil {
		t.Fatal(err)
	}
	// Same alias sees the spend.
	res, _ := l.CheckModelTokens(context.Background(), r, "code", "")
	if res.Remaining != 100 {
		t.Errorf("code remaining = %d, want 100", res.Remaining)
	}
	// A different alias is unaffected (separate counter).
	res, _ = l.CheckModelTokens(context.Background(), r, "code-completion", "")
	if res.Remaining != 1000 {
		t.Errorf("code-completion remaining = %d, want 1000 (isolated)", res.Remaining)
	}
}

func TestWriteOpenAIHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	ratelimit.WriteOpenAIHeaders(w, "tokens", ratelimit.CheckResult{Limit: 1000, Remaining: 250, ResetAfter: 90 * time.Second})
	if got := w.Header().Get("x-ratelimit-limit-tokens"); got != "1000" {
		t.Errorf("limit-tokens = %q, want 1000", got)
	}
	if got := w.Header().Get("x-ratelimit-remaining-tokens"); got != "250" {
		t.Errorf("remaining-tokens = %q, want 250", got)
	}
	if got := w.Header().Get("x-ratelimit-reset-tokens"); got != "1m30s" {
		t.Errorf("reset-tokens = %q, want 1m30s", got)
	}
	// Unlimited → no headers.
	w2 := httptest.NewRecorder()
	ratelimit.WriteOpenAIHeaders(w2, "requests", ratelimit.CheckResult{Limit: 0})
	if got := w2.Header().Get("x-ratelimit-limit-requests"); got != "" {
		t.Errorf("expected no header when limit=0, got %q", got)
	}
}

// An allowed token check still surfaces the window reset (ResetAfter > 0) so
// x-ratelimit-reset-tokens appears on 2xx, not only on 429.
func TestCheckModelTokens_Allowed_ExposesReset(t *testing.T) {
	l, _ := newLimiterWithModelLimits(t, nil, map[string]map[string]config.RateLimitConfig{
		"gpt-4o": {"*": {TokenRate: 1000, TokenPeriod: "1h"}},
	}, "X-Consumer", "X-User-Type")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Consumer", "user1")

	if err := l.AddModelTokens(context.Background(), r, "gpt-4o", "", 100); err != nil {
		t.Fatal(err)
	}
	res, err := l.CheckModelTokens(context.Background(), r, "gpt-4o", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed (100 < 1000)")
	}
	if res.ResetAfter <= 0 {
		t.Errorf("expected ResetAfter > 0 on an allowed check with an active window, got %v", res.ResetAfter)
	}
}
