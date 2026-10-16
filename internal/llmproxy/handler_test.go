package llmproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/cache"
	"gatewai/gateway/internal/concurrency"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/llmproxy/provider"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/usage"
)

// ── in-memory cache ──────────────────────────────────────────────────────────

type memCache struct {
	mu     sync.Mutex
	data   map[string]*cache.Entry
	setted chan struct{} // closed on first Set; nil = no notification needed
}

func newMemCache() *memCache { return &memCache{data: make(map[string]*cache.Entry)} }

// withSetNotify returns a new memCache that closes the returned channel on the
// first successful Set call, allowing tests to wait for async cache-fill.
func newMemCacheWithNotify() (*memCache, <-chan struct{}) {
	ch := make(chan struct{})
	return &memCache{data: make(map[string]*cache.Entry), setted: ch}, ch
}

func (m *memCache) Get(_ context.Context, key string) (*cache.Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[key]
	return e, ok, nil
}

func (m *memCache) Set(_ context.Context, key string, entry *cache.Entry, _ time.Duration) error {
	m.mu.Lock()
	ch := m.setted
	m.data[key] = entry
	m.setted = nil // only notify once
	m.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	return nil
}

// ── token limiter stub ────────────────────────────────────────────────────────

// stubTokenLimiter is a test double that satisfies ratelimit.TokenChecker.
// serviceResult is returned by CheckTokens; modelResult by CheckModelTokens.
type stubTokenLimiter struct {
	serviceResult ratelimit.CheckResult
	modelResult   ratelimit.CheckResult
}

func (s *stubTokenLimiter) CheckTokens(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return s.serviceResult, nil
}
func (s *stubTokenLimiter) AddTokens(_ context.Context, _ *http.Request, _ string, _ int) error {
	return nil
}
func (s *stubTokenLimiter) CheckModelTokens(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return s.modelResult, nil
}
func (s *stubTokenLimiter) AddModelTokens(_ context.Context, _ *http.Request, _ string, _ int) error {
	return nil
}
func (s *stubTokenLimiter) AddTokensFor(_ context.Context, _, _, _ string, _ int) error {
	return nil
}

// trackingTokenLimiter records AddTokens/AddModelTokens calls for assertions.
type trackingTokenLimiter struct {
	mu               sync.Mutex
	addedService     int
	addedModel       int
	addedServiceType string
	addedModelName   string
}

func (t *trackingTokenLimiter) CheckTokens(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}
func (t *trackingTokenLimiter) CheckModelTokens(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}
func (t *trackingTokenLimiter) AddTokens(_ context.Context, _ *http.Request, serviceType string, n int) error {
	t.mu.Lock()
	t.addedService += n
	t.addedServiceType = serviceType
	t.mu.Unlock()
	return nil
}
func (t *trackingTokenLimiter) AddModelTokens(_ context.Context, _ *http.Request, model string, n int) error {
	t.mu.Lock()
	t.addedModel += n
	t.addedModelName = model
	t.mu.Unlock()
	return nil
}
func (t *trackingTokenLimiter) AddTokensFor(_ context.Context, _, _, _ string, _ int) error {
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func llmDef(provider, backendModel string, cacheTTL time.Duration) *service.Def {
	return &service.Def{
		Type:             "llm",
		Model:            "my-alias",
		Provider:         provider,
		BackendModel:     backendModel,
		ResponseCacheTTL: cacheTTL,
		InferenceURL:     "", // set per-test via setBackend
	}
}

// setBackend points def at a single httptest backend URL.
func setBackend(def *service.Def, url string) {
	def.InferenceURL = url
	def.Backends = []service.Backend{{URL: url, Weight: 1}}
}

func doServeJSON(h *Handler, def *service.Def, body string, extraHeaders ...func(*http.Request)) *httptest.ResponseRecorder {
	return doServeJSONAs(h, def, body, "", extraHeaders...)
}

func doServeJSONAs(h *Handler, def *service.Def, body, consumer string, extraHeaders ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, fn := range extraHeaders {
		fn(req)
	}
	rr := httptest.NewRecorder()
	h.ServeJSON(rr, req, def, []byte(body), consumer)
	return rr
}

const chatBody = `{"model":"my-alias","messages":[{"role":"user","content":"Hello"}]}`

// openAI-format success response used by fake backends.
const fakeResponse = `{"id":"chatcmpl-1","object":"chat.completion","model":"my-alias","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`

// ── tests ────────────────────────────────────────────────────────────────────

func TestServeJSON_CircuitBreaker_OpensAndFastFails(t *testing.T) {
	// Backend always 500s.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithCircuitBreaker(NewCircuitBreaker(2, time.Minute))
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	// Two 5xx responses reach the backend → 502, tripping the breaker (threshold 2).
	for i := 0; i < 2; i++ {
		if rr := doServeJSON(h, def, chatBody); rr.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d: got %d, want 502", i, rr.Code)
		}
	}
	// Circuit now open → the next request is skipped and fast-fails 503.
	if rr := doServeJSON(h, def, chatBody); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 (circuit open)", rr.Code)
	}
}

func TestServeJSON_CircuitBreaker_HealthyBackendPasses(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithCircuitBreaker(NewCircuitBreaker(2, time.Minute))
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	// Repeated healthy requests keep the circuit closed (success resets).
	for i := 0; i < 3; i++ {
		if rr := doServeJSON(h, def, chatBody); rr.Code != http.StatusOK {
			t.Fatalf("attempt %d: got %d, want 200", i, rr.Code)
		}
	}
}

func TestServeJSON_CacheMiss_ThenHit(t *testing.T) {
	// Fake backend returns a valid response.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc, filled := newMemCacheWithNotify()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	// First call: cache miss.
	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cache") != "MISS" {
		t.Errorf("first call: expected X-Cache=MISS, got %q", rr.Header().Get("X-Cache"))
	}

	// Wait for the async cache-fill goroutine to write the entry.
	select {
	case <-filled:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for async cache-fill")
	}

	// Second call: cache hit.
	rr2 := doServeJSON(h, def, chatBody)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d", rr2.Code)
	}
	if rr2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("second call: expected X-Cache=HIT, got %q", rr2.Header().Get("X-Cache"))
	}
	if rr2.Body.String() != rr.Body.String() {
		t.Error("cached body should match original response")
	}
}

func TestServeJSON_NoCacheHeader_BypassesCache(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	setNoCache := func(r *http.Request) { r.Header.Set("Cache-Control", "no-cache") }

	doServeJSON(h, def, chatBody, setNoCache)
	doServeJSON(h, def, chatBody, setNoCache)

	if callCount != 2 {
		t.Errorf("Cache-Control: no-cache should bypass cache; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_Non200NotCached(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	doServeJSON(h, def, chatBody)
	doServeJSON(h, def, chatBody)

	if callCount != 2 {
		t.Errorf("429 responses should not be cached; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_CacheDisabled_WhenTTLZero(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 0) // TTL=0 → no cache
	setBackend(def, backend.URL)

	doServeJSON(h, def, chatBody)
	doServeJSON(h, def, chatBody)

	if callCount != 2 {
		t.Errorf("TTL=0 should disable cache; backend called %d times (want 2)", callCount)
	}
}

func TestServeJSON_BackendModel_RewrittenInRequest(t *testing.T) {
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "meta-llama/Meta-Llama-3-8B-Instruct", 0)
	setBackend(def, backend.URL)

	// Client sends alias "my-alias".
	doServeJSON(h, def, chatBody)

	if receivedModel != "meta-llama/Meta-Llama-3-8B-Instruct" {
		t.Errorf("backend should receive backend_model; got %q", receivedModel)
	}
}

func TestServeJSON_BackendModel_NotRewritten_WhenEmpty(t *testing.T) {
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 0) // no backend_model
	setBackend(def, backend.URL)

	doServeJSON(h, def, chatBody)

	// When BackendModel is empty, the alias from the body is forwarded as-is.
	if receivedModel != "my-alias" {
		t.Errorf("without backend_model, original model should be forwarded; got %q", receivedModel)
	}
}

func TestServeJSON_CanonicalModel_RevealedInResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Backend echoes the raw backend model id.
		io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"Qwen/Qwen3.8-27B-FP8","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "Qwen/Qwen3.8-27B-FP8", 0)
	def.CanonicalModel = "code-1.0"
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if got := resp["model"]; got != "code-1.0" {
		t.Errorf("response model should be canonical pin; got %v", got)
	}
}

func TestServeJSON_CanonicalModel_Empty_LeavesResponseModel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"Qwen/Qwen3.8-27B-FP8","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"}}]}`)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 0) // no canonical_model
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if got := resp["model"]; got != "Qwen/Qwen3.8-27B-FP8" {
		t.Errorf("without canonical_model, backend model should pass through; got %v", got)
	}
}

func TestServeStream_CanonicalModel_RevealedInChunks(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"Qwen/Qwen3.8-27B-FP8\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n")
		io.WriteString(w, "\n")
		io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"Qwen/Qwen3.8-27B-FP8\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n")
		io.WriteString(w, "\n")
		io.WriteString(w, "data: [DONE]\n")
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "Qwen/Qwen3.8-27B-FP8", 0)
	def.CanonicalModel = "code-1.0"
	setBackend(def, backend.URL)

	streamBody := `{"model":"my-alias","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	rr := doServeJSON(h, def, streamBody)

	out := rr.Body.String()
	if strings.Contains(out, "Qwen/Qwen3.8-27B-FP8") {
		t.Errorf("stream leaked raw backend model id:\n%s", out)
	}
	if !strings.Contains(out, `"model":"code-1.0"`) {
		t.Errorf("stream chunks should carry canonical model; got:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("stream should preserve [DONE]; got:\n%s", out)
	}
}

func TestServeJSON_CacheHit_ReturnsCachedBody(t *testing.T) {
	mc := newMemCache()

	// Pre-populate cache with a known entry.
	cacheKey, cacheable, err := cache.Key("passthrough", "my-alias", []byte(chatBody))
	if err != nil || !cacheable {
		t.Fatalf("test setup: cache.Key failed: err=%v cacheable=%v", err, cacheable)
	}
	mc.Set(context.Background(), cacheKey, &cache.Entry{
		Body:        []byte(`{"cached":true}`),
		ContentType: "application/json",
		StatusCode:  200,
	}, 60*time.Second)

	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cache") != "HIT" {
		t.Errorf("expected X-Cache=HIT, got %q", rr.Header().Get("X-Cache"))
	}
	if rr.Body.String() != `{"cached":true}` {
		t.Errorf("expected cached body, got %q", rr.Body.String())
	}
}

func TestServeJSON_UnknownProvider_Returns500(t *testing.T) {
	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := &service.Def{
		Type:     "llm",
		Model:    "x",
		Provider: "nonexistent-provider",
	}

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for unknown provider, got %d", rr.Code)
	}
}

func TestRewriteBodyModel(t *testing.T) {
	body := []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`)
	out, err := rewriteBodyModel(body, "real-model-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)

	if result["model"] != "real-model-id" {
		t.Errorf("expected model=real-model-id, got %q", result["model"])
	}
	// Other fields must be preserved.
	if result["temperature"] != 0.5 {
		t.Errorf("expected temperature=0.5, got %v", result["temperature"])
	}
}

func TestRewriteBodyModel_InvalidJSON(t *testing.T) {
	_, err := rewriteBodyModel([]byte(`not json`), "model")
	if err == nil {
		t.Error("expected error for invalid JSON body")
	}
}

// ── consumer tracker ─────────────────────────────────────────────────────────

// testTracker records Track calls for assertion in tests.
type testTracker struct {
	mu    sync.Mutex
	calls []trackCall
}

type trackCall struct {
	consumer, userType, tokenType string
	count                         int
}

func (t *testTracker) Track(_ context.Context, consumer, userType, tokenType string, count int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, trackCall{consumer, userType, tokenType, count})
}

func (t *testTracker) sum(consumer, tokenType string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := 0
	for _, c := range t.calls {
		if c.consumer == consumer && c.tokenType == tokenType {
			total += c.count
		}
	}
	return total
}

// testUsageTracker records TrackTokens calls for assertion in tests; all
// other UsageTracker methods are no-ops.
type testUsageTracker struct {
	mu    sync.Mutex
	calls []usageTokensCall
}

type usageTokensCall struct {
	consumer, serviceType string
	prompt, completion    int64
}

func (t *testUsageTracker) TrackRequest(_ context.Context, _, _ string)                   {}
func (t *testUsageTracker) TrackJob(_ context.Context, _, _ string)                       {}
func (t *testUsageTracker) TrackProcessingTime(_ context.Context, _, _ string, _ float64) {}
func (t *testUsageTracker) TrackActive(_ context.Context, _ string)                       {}
func (t *testUsageTracker) TrackUserType(_ context.Context, _, _, _ string)               {}
func (t *testUsageTracker) UpdateRetention(_ time.Duration)                               {}

func (t *testUsageTracker) TrackTokens(_ context.Context, consumer, serviceType string, prompt, completion int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, usageTokensCall{consumer, serviceType, prompt, completion})
}

func (t *testUsageTracker) snapshot() []usageTokensCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]usageTokensCall(nil), t.calls...)
}

var _ usage.UsageTracker = (*testUsageTracker)(nil)

// ── consumer metrics ─────────────────────────────────────────────────────────

func TestServeJSON_ConsumerMetrics_EmittedOnBackendResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer backend.Close()

	tracker := &testTracker{}
	reg := provider.NewRegistry()
	h := New(newMemCache(), reg, &http.Client{Timeout: 5 * time.Second}, "", tracker, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	doServeJSONAs(h, def, chatBody, "alice")

	if got := tracker.sum("alice", "prompt"); got != 7 {
		t.Errorf("expected 7 prompt tokens tracked for alice, got %d", got)
	}
}

func TestServeJSON_ConsumerMetrics_EmittedOnCacheHit(t *testing.T) {
	mc := newMemCache()
	cacheKey, _, _ := cache.Key("passthrough", "my-alias", []byte(chatBody))
	mc.Set(context.Background(), cacheKey, &cache.Entry{
		Body:        []byte(`{"id":"c2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`),
		ContentType: "application/json",
		StatusCode:  200,
	}, 60*time.Second)

	tracker := &testTracker{}
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", tracker, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)

	doServeJSONAs(h, def, chatBody, "bob")

	if got := tracker.sum("bob", "completion"); got != 2 {
		t.Errorf("expected 2 completion tokens tracked for bob on cache hit, got %d", got)
	}
}

// ── usage report tracker (GET /-/usage/report calendar aggregates) ──────────

func TestServeJSON_UsageTracker_TrackTokensOnBackendResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer backend.Close()

	ut := &testUsageTracker{}
	reg := provider.NewRegistry()
	h := New(newMemCache(), reg, &http.Client{Timeout: 5 * time.Second}, "", &testTracker{}, AuditConfig{}, nil)
	h.WithUsageTracker(ut)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	doServeJSONAs(h, def, chatBody, "alice")

	calls := ut.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 TrackTokens call, got %d: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.consumer != "alice" || c.serviceType != "llm" || c.prompt != 7 || c.completion != 3 {
		t.Errorf("unexpected TrackTokens call: %+v", c)
	}
}

func TestServeJSON_UsageTracker_TrackTokensOnCacheHit(t *testing.T) {
	mc := newMemCache()
	cacheKey, _, _ := cache.Key("passthrough", "my-alias", []byte(chatBody))
	mc.Set(context.Background(), cacheKey, &cache.Entry{
		Body:        []byte(`{"id":"c2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`),
		ContentType: "application/json",
		StatusCode:  200,
	}, 60*time.Second)

	ut := &testUsageTracker{}
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", &testTracker{}, AuditConfig{}, nil)
	h.WithUsageTracker(ut)

	def := llmDef("passthrough", "", 60*time.Second)

	doServeJSONAs(h, def, chatBody, "bob")

	calls := ut.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 TrackTokens call, got %d: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.consumer != "bob" || c.serviceType != "llm" || c.prompt != 4 || c.completion != 2 {
		t.Errorf("unexpected TrackTokens call: %+v", c)
	}
}

func TestServeJSON_UsageTracker_NilTracker_NoPanic(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(newMemCache(), reg, &http.Client{Timeout: 5 * time.Second}, "", &testTracker{}, AuditConfig{}, nil)
	// WithUsageTracker deliberately not called: usageTracker stays nil.

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	doServeJSONAs(h, def, chatBody, "alice")
}

// ── streaming ────────────────────────────────────────────────────────────────

const streamBody = `{"model":"my-alias","messages":[{"role":"user","content":"Hello"}],"stream":true}`

func TestServeJSON_Streaming_PipedToClient(t *testing.T) {
	sseChunks := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseChunks)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, streamBody)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("expected X-Accel-Buffering=no, got %q", rr.Header().Get("X-Accel-Buffering"))
	}
	if rr.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("expected Cache-Control=no-cache, got %q", rr.Header().Get("Cache-Control"))
	}
	if rr.Body.String() != sseChunks {
		t.Errorf("body mismatch: got %q", rr.Body.String())
	}
}

// TestServeJSON_Streaming_TracksPerConsumerTokens is the regression guard for the
// streaming token-accounting bug: a streamed response's usage (from the injected
// stream_options.include_usage chunk) must reach BOTH per-consumer trackers, just
// like the non-streaming path — otherwise every streaming consumer is silently
// under-reported. It also asserts include_usage is injected when a usage tracker
// is configured (not only when a token limiter is).
func TestServeJSON_Streaming_TracksPerConsumerTokens(t *testing.T) {
	// Final chunk carries usage with empty choices, mirroring real OpenAI-compatible
	// backends when stream_options.include_usage is set.
	sseChunks := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	var gotIncludeUsage bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		gotIncludeUsage = strings.Contains(string(reqBody), `"include_usage":true`)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseChunks)
	}))
	defer backend.Close()

	ut := &testUsageTracker{}
	ct := &testTracker{}
	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", ct, AuditConfig{}, nil)
	h.WithUsageTracker(ut)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	rr := doServeJSONAs(h, def, streamBody, "carol")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	// include_usage must be injected because a usage tracker is configured.
	if !gotIncludeUsage {
		t.Error("expected stream_options.include_usage=true to be injected into the upstream request")
	}

	// Calendar usage store must record the streamed tokens for the consumer.
	calls := ut.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 TrackTokens call for the streamed request, got %d: %+v", len(calls), calls)
	}
	if c := calls[0]; c.consumer != "carol" || c.serviceType != "llm" || c.prompt != 11 || c.completion != 5 {
		t.Errorf("unexpected TrackTokens call: %+v", c)
	}

	// Per-consumer metrics tracker must record them too.
	if got := ct.sum("carol", "prompt"); got != 11 {
		t.Errorf("expected 11 prompt tokens tracked for carol, got %d", got)
	}
	if got := ct.sum("carol", "completion"); got != 5 {
		t.Errorf("expected 5 completion tokens tracked for carol, got %d", got)
	}
}

// TestServeJSON_Streaming_OutputFlag_DetectsSplitPII guards the cross-chunk fix:
// an email streamed as separate token deltas ("bob","@example",".com") must still
// be detected by joining the deltas before scanning. Streaming never redacts —
// the body passes through unmodified — but the flag metric must still fire.
func TestServeJSON_Streaming_OutputFlag_DetectsSplitPII(t *testing.T) {
	sseChunks := "data: {\"choices\":[{\"delta\":{\"content\":\"bob\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"@example\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\".com\"}}]}\n\n" +
		"data: [DONE]\n\n"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseChunks)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	def := llmDefWithOutput("redact", []string{"pii"}) // redact degrades to flag for streams
	setBackend(def, backend.URL)

	ctr := metrics.GuardrailsTotal.WithLabelValues("llm", "my-alias", "output", "flag", "flagged")
	before := testutil.ToFloat64(ctr)

	rr := doServeJSON(h, def, streamBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// Stream is never modified: the (split) raw content is forwarded as-is.
	if !strings.Contains(rr.Body.String(), "bob") || !strings.Contains(rr.Body.String(), "@example") {
		t.Error("streamed body should pass through unmodified")
	}
	if got := testutil.ToFloat64(ctr) - before; got != 1 {
		t.Errorf("expected output flag metric +1 for split-PII stream, got +%v", got)
	}
}

func TestServeJSON_Streaming_SkipsCache(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 60*time.Second)
	setBackend(def, backend.URL)

	doServeJSON(h, def, streamBody)
	doServeJSON(h, def, streamBody)

	if callCount != 2 {
		t.Errorf("streaming should bypass cache: backend called %d times (want 2)", callCount)
	}
	// Cache should not have been written.
	if len(mc.data) != 0 {
		t.Errorf("streaming should not populate cache, got %d entries", len(mc.data))
	}
}

func TestServeJSON_Streaming_BackendModel_Rewritten(t *testing.T) {
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		receivedModel, _ = req["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "real-model-id", 0)
	setBackend(def, backend.URL)

	doServeJSON(h, def, streamBody)

	if receivedModel != "real-model-id" {
		t.Errorf("expected backend to receive real-model-id, got %q", receivedModel)
	}
}

type noopTracker struct{}

func (n *noopTracker) Track(_ context.Context, _, _, _ string, _ int) {}

// ── audit log ────────────────────────────────────────────────────────────────

// capturingHandler captures slog records for assertion in tests.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(name string) slog.Handler       { return h }

func (h *capturingHandler) attr(key string) (slog.Value, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		var found slog.Value
		var ok bool
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				found = a.Value
				ok = true
				return false
			}
			return true
		})
		if ok {
			return found, true
		}
	}
	return slog.Value{}, false
}

func TestAuditLog_Disabled_NoLog(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	cap := &capturingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(old)

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{Enabled: false}, nil)
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)
	doServeJSON(h, def, chatBody)

	cap.mu.Lock()
	n := len(cap.records)
	cap.mu.Unlock()
	if n != 0 {
		t.Errorf("expected no audit log records when disabled, got %d", n)
	}
}

func TestAuditLog_Enabled_LogsFields(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	cap := &capturingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(old)

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{Enabled: true}, nil)
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)
	doServeJSONAs(h, def, chatBody, "alice")

	for _, key := range []string{"service_type", "model", "consumer", "status", "duration_ms", "backend_url"} {
		if _, ok := cap.attr(key); !ok {
			t.Errorf("audit log missing field %q", key)
		}
	}
	if v, ok := cap.attr("consumer"); !ok || v.String() != "alice" {
		t.Errorf("expected consumer=alice, got %q ok=%v", v, ok)
	}
	if v, ok := cap.attr("stream"); !ok || v.Bool() {
		t.Errorf("expected stream=false for non-streaming request, got %v ok=%v", v, ok)
	}
}

func TestAuditLog_Prompt_IncludesBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	cap := &capturingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(old)

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{Enabled: true, Prompt: true}, nil)
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)
	doServeJSON(h, def, chatBody)

	if _, ok := cap.attr("prompt"); !ok {
		t.Error("expected prompt field in audit log when Prompt=true")
	}
}

func TestAuditLog_NoPrompt_BodyOmitted(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	cap := &capturingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(old)

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{Enabled: true, Prompt: false}, nil)
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)
	doServeJSON(h, def, chatBody)

	if _, ok := cap.attr("prompt"); ok {
		t.Error("prompt field should be absent when Prompt=false")
	}
}

func TestServeJSON_ConsumerMetrics_SkippedWhenNoConsumer(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	tracker := &testTracker{}
	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", tracker, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	rr := doServeJSONAs(h, def, chatBody, "")
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if len(tracker.calls) != 0 {
		t.Errorf("expected no tracker calls for empty consumer, got %d", len(tracker.calls))
	}
}

// TestAuditLog_Streaming_LogsStreamTrue verifies that the streaming path emits
// an audit log record with stream=true when audit is enabled.
func TestAuditLog_Streaming_LogsStreamTrue(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	cap := &capturingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(old)

	streamBody := `{"model":"my-alias","messages":[{"role":"user","content":"Hello"}],"stream":true}`

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{Enabled: true}, nil)
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)
	doServeJSON(h, def, streamBody)

	if v, ok := cap.attr("stream"); !ok {
		t.Error("audit log missing 'stream' field on streaming request")
	} else if !v.Bool() {
		t.Errorf("expected stream=true for streaming request, got %v", v)
	}
	if _, ok := cap.attr("service_type"); !ok {
		t.Error("audit log missing 'service_type' field on streaming request")
	}
}

// TestAuditLog_Enabled_BackendError_StillLogged verifies that an audit record is
// emitted even when the backend returns a non-2xx status code.
func TestAuditLog_Enabled_BackendError_StillLogged(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"oops"}`)
	}))
	defer backend.Close()

	cap := &capturingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(old)

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", &noopTracker{}, AuditConfig{Enabled: true}, nil)
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)
	doServeJSON(h, def, chatBody)

	v, ok := cap.attr("status")
	if !ok {
		t.Fatal("audit log missing 'status' field on backend error")
	}
	if v.Int64() != 500 {
		t.Errorf("expected status=500 in audit log, got %d", v.Int64())
	}
}

func TestServeJSON_Streaming_TokensCountedFromUsageChunk(t *testing.T) {
	// Backend sends a stream where the last data chunk contains usage.
	sseStream := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseStream)
	}))
	defer backend.Close()

	tracker := &trackingTokenLimiter{}
	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, tracker)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, streamBody)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if tracker.addedService != 15 {
		t.Errorf("expected 15 service tokens added (10+5), got %d", tracker.addedService)
	}
	if tracker.addedModel != 15 {
		t.Errorf("expected 15 model tokens added (10+5), got %d", tracker.addedModel)
	}
	if tracker.addedModelName != "my-alias" {
		t.Errorf("expected model name %q, got %q", "my-alias", tracker.addedModelName)
	}
}

func TestServeJSON_Streaming_InjectsStreamOptions_WhenLimiterSet(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	tracker := &trackingTokenLimiter{}
	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, tracker)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	doServeJSON(h, def, streamBody)

	var parsed map[string]any
	if err := json.Unmarshal(receivedBody, &parsed); err != nil {
		t.Fatalf("backend received invalid JSON: %v", err)
	}
	opts, _ := parsed["stream_options"].(map[string]any)
	if opts == nil {
		t.Fatal("expected stream_options injected in upstream body, got none")
	}
	if opts["include_usage"] != true {
		t.Errorf("expected stream_options.include_usage=true, got %v", opts["include_usage"])
	}
}

func TestServeJSON_Streaming_NoStreamOptions_WhenNoLimiter(t *testing.T) {
	var receivedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	doServeJSON(h, def, streamBody)

	var parsed map[string]any
	if err := json.Unmarshal(receivedBody, &parsed); err != nil {
		t.Fatalf("backend received invalid JSON: %v", err)
	}
	if _, ok := parsed["stream_options"]; ok {
		t.Error("stream_options should not be injected when no token limiter is set")
	}
}

func TestServeJSON_ModelTokenLimit_Rejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	limiter := &stubTokenLimiter{
		serviceResult: ratelimit.CheckResult{Allowed: true},
		modelResult: ratelimit.CheckResult{
			Allowed:    false,
			Limit:      5000,
			ResetAfter: 30 * time.Minute,
		},
	}

	mc := newMemCache()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, limiter)

	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when model token limit exceeded, got %d", rr.Code)
	}
	if rr.Header().Get("X-TokenRateLimit-Limit") != "5000" {
		t.Errorf("expected X-TokenRateLimit-Limit=5000, got %q", rr.Header().Get("X-TokenRateLimit-Limit"))
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on model token limit rejection")
	}
}

// ── output DLP guardrails ────────────────────────────────────────────────────

// fakeResponseWithEmail returns an OpenAI-format response whose assistant message
// contains a plain email address — detected by the "pii" check group.
const fakeResponseWithPII = `{"id":"chatcmpl-2","object":"chat.completion","model":"my-alias","choices":[{"index":0,"message":{"role":"assistant","content":"Contact me at test@example.com please"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":8}}`

// llmDefWithOutput builds a service.Def that enables output DLP guardrails.
func llmDefWithOutput(action string, checks []string) *service.Def {
	def := llmDef("passthrough", "", 60*time.Second)
	def.Guardrails.Output = service.GuardrailsStage{
		Enabled: true,
		Checks:  checks,
		Action:  action,
	}
	return def
}

func TestOutputGuardrails_Redact_RemovesPIIFromBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponseWithPII)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDefWithOutput("redact", []string{"pii"})
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "test@example.com") {
		t.Error("email should have been redacted from response body")
	}
	if !strings.Contains(body, "[REDACTED") {
		t.Error("expected a redaction placeholder in response body")
	}
}

func TestOutputGuardrails_Block_Returns422_AndSkipsCache(t *testing.T) {
	callCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponseWithPII)
	}))
	defer backend.Close()

	mc, filled := newMemCacheWithNotify()
	reg := provider.NewRegistry()
	h := New(mc, reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDefWithOutput("block", []string{"pii"})
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when output blocked by guardrails, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "response blocked by guardrails") {
		t.Errorf("expected block error message, got: %q", body)
	}

	// Cache must not be filled after a block.
	select {
	case <-filled:
		t.Error("cache should not be filled when output guardrails block the response")
	case <-time.After(200 * time.Millisecond):
		// expected: no cache fill
	}
	if len(mc.data) != 0 {
		t.Errorf("cache should be empty after output block, got %d entries", len(mc.data))
	}
}

func TestOutputGuardrails_Flag_LeavesBodyUnchanged(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fakeResponseWithPII)
	}))
	defer backend.Close()

	reg := provider.NewRegistry()
	h := New(cache.NewNoop(), reg, &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)

	def := llmDefWithOutput("flag", []string{"pii"})
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with flag action, got %d", rr.Code)
	}
	// Body must be the original — flag does not modify it.
	if !strings.Contains(rr.Body.String(), "test@example.com") {
		t.Error("flag action must leave the email in the body unchanged")
	}
}

// TestTruncateForSpan_RuneBoundary ensures oversized-body truncation for span
// attributes snaps to a rune boundary instead of splitting a multi-byte char.
func TestTruncateForSpan_RuneBoundary(t *testing.T) {
	// Fill just under the cap with ASCII, then pad with 2-byte runes so the cut
	// at maxSpanAttrBytes lands inside a rune.
	body := append([]byte(strings.Repeat("a", maxSpanAttrBytes-1)), []byte("éééé")...)
	out := truncateForSpan(body)
	if !utf8.ValidString(out) {
		t.Fatalf("truncated span attribute is not valid UTF-8: %q", out[len(out)-8:])
	}
	if !strings.HasSuffix(out, "...[truncated]") {
		t.Errorf("expected truncation marker, got tail %q", out[len(out)-16:])
	}
}

// ── backend_pools wiring (Phase 5) ──────────────────────────────────────────

// stubPoolChecker is a test double for ratelimit.PoolChecker: memberURLs in
// deny are rejected, everything else is allowed. calls records every checked
// member URL, in order, for assertions.
type stubPoolChecker struct {
	mu    sync.Mutex
	deny  map[string]bool
	calls []string
}

func (s *stubPoolChecker) CheckBackendPool(_ context.Context, _, memberURL string) (ratelimit.CheckResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, memberURL)
	if s.deny[memberURL] {
		return ratelimit.CheckResult{Allowed: false}, nil
	}
	return ratelimit.CheckResult{Allowed: true}, nil
}

// poolDef returns a Def with PoolName set, backed by the given backend URLs.
func poolDef(poolName string, urls ...string) *service.Def {
	backends := make([]service.Backend, len(urls))
	for i, u := range urls {
		backends[i] = service.Backend{URL: u, Weight: 1}
	}
	return &service.Def{
		Type:     "llm",
		Model:    "my-alias",
		Provider: "passthrough",
		PoolName: poolName,
		Backends: backends,
	}
}

// newPoolSemaphoreForTest builds a real *concurrency.ModelSemaphore against an
// in-process miniredis instance — the field is a concrete type, not an
// interface, so a stub cannot substitute for it.
func newPoolSemaphoreForTest(t *testing.T, poolName string, maxConcurrent, priorityReserved int) *concurrency.ModelSemaphore {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sem := concurrency.NewPoolSemaphore(map[string]config.BackendPoolConfig{
		poolName: {MaxConcurrent: maxConcurrent, PriorityReservedConcurrent: priorityReserved},
	}, rdb)
	if sem == nil {
		t.Fatal("expected non-nil pool semaphore")
	}
	return sem
}

func TestServeJSON_PoolSemaphore_Saturated_Returns503_NoBackendCall(t *testing.T) {
	var calls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	sem := newPoolSemaphoreForTest(t, "vllm-llama3", 1, 0)
	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithPoolSemaphore(sem)
	def := poolDef("vllm-llama3", backend.URL)

	// Occupy the pool's single concurrency slot directly.
	ok, _ := sem.TryAcquire("vllm-llama3", false)
	if !ok {
		t.Fatal("expected to occupy the pool's only slot")
	}

	before := testutil.ToFloat64(metrics.BackendPoolConcurrencyRejectedTotal.WithLabelValues("vllm-llama3"))
	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 (pool saturated)", rr.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("expected no HTTP call to reach the backend, got %d", got)
	}
	if got := testutil.ToFloat64(metrics.BackendPoolConcurrencyRejectedTotal.WithLabelValues("vllm-llama3")) - before; got != 1 {
		t.Errorf("expected concurrency-rejected metric +1, got +%v", got)
	}
}

func TestServeJSON_PoolSemaphore_ReleasedAfterSuccessAndFailure(t *testing.T) {
	status := int32(http.StatusOK)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.LoadInt32(&status) != http.StatusOK {
			w.WriteHeader(int(atomic.LoadInt32(&status)))
			return
		}
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	sem := newPoolSemaphoreForTest(t, "vllm-llama3", 1, 0)
	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithPoolSemaphore(sem)
	def := poolDef("vllm-llama3", backend.URL)

	// A successful request must release the slot so a subsequent request can acquire it.
	if rr := doServeJSON(h, def, chatBody); rr.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rr.Code)
	}
	if rr := doServeJSON(h, def, chatBody); rr.Code != http.StatusOK {
		t.Fatalf("second request after release: got %d, want 200", rr.Code)
	}

	// A failed backend request must also release the slot.
	atomic.StoreInt32(&status, http.StatusInternalServerError)
	if rr := doServeJSON(h, def, chatBody); rr.Code != http.StatusBadGateway {
		t.Fatalf("failing request: got %d, want 502", rr.Code)
	}
	atomic.StoreInt32(&status, http.StatusOK)
	if rr := doServeJSON(h, def, chatBody); rr.Code != http.StatusOK {
		t.Fatalf("request after failure release: got %d, want 200", rr.Code)
	}
}

func TestServeJSON_PoolRateLimit_SkipsMemberThenSucceedsOnNext(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("rate-limited member A must not receive a request")
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fakeResponse)
	}))
	defer backendB.Close()

	pc := &stubPoolChecker{deny: map[string]bool{backendA.URL: true}}
	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithPoolLimiter(pc)
	def := poolDef("vllm-llama3", backendA.URL, backendB.URL)

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (should succeed via member B)", rr.Code)
	}
}

func TestServeJSON_PoolRateLimit_AllMembersRejected_Returns503(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("no backend should be called when all members are pool-rate-limited")
	}))
	defer backend.Close()

	pc := &stubPoolChecker{deny: map[string]bool{backend.URL: true}}
	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithPoolLimiter(pc)
	def := poolDef("vllm-llama3", backend.URL)

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 (all members rate limited)", rr.Code)
	}
}

// TestServeJSON_NonPoolService_PoolLimiterAndSemaphoreSet_NoBehaviorChange is a
// regression guard: a service with no backend_pool (PoolName == "") must be
// completely unaffected even when pool checkers are wired up globally.
func TestServeJSON_NonPoolService_PoolLimiterAndSemaphoreSet_NoBehaviorChange(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fakeResponse)
	}))
	defer backend.Close()

	// Semaphore/limiter configured for an unrelated pool name; def.PoolName is empty.
	sem := newPoolSemaphoreForTest(t, "some-other-pool", 1, 0)
	ok, _ := sem.TryAcquire("some-other-pool", false) // saturate it, to prove it's never consulted
	if !ok {
		t.Fatal("setup: expected to saturate the unrelated pool")
	}
	pc := &stubPoolChecker{deny: map[string]bool{backend.URL: true}} // would reject every member if consulted

	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithPoolSemaphore(sem).WithPoolLimiter(pc)
	def := llmDef("passthrough", "", 0)
	setBackend(def, backend.URL)

	rr := doServeJSON(h, def, chatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (non-pool service must ignore pool checkers entirely)", rr.Code)
	}
}

func TestServeStream_PoolSemaphore_Saturated_Returns503(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("saturated pool must not reach the backend")
	}))
	defer backend.Close()

	sem := newPoolSemaphoreForTest(t, "vllm-llama3", 1, 0)
	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithPoolSemaphore(sem)
	def := poolDef("vllm-llama3", backend.URL)

	ok, _ := sem.TryAcquire("vllm-llama3", false)
	if !ok {
		t.Fatal("expected to occupy the pool's only slot")
	}

	rr := doServeJSON(h, def, streamBody)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 (pool saturated)", rr.Code)
	}
}

func TestServeStream_PoolRateLimit_SkipsMemberThenSucceedsOnNext(t *testing.T) {
	sseChunks := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("rate-limited member A must not receive a request")
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseChunks)
	}))
	defer backendB.Close()

	pc := &stubPoolChecker{deny: map[string]bool{backendA.URL: true}}
	h := New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, AuditConfig{}, nil)
	h.WithPoolLimiter(pc)
	def := poolDef("vllm-llama3", backendA.URL, backendB.URL)

	rr := doServeJSON(h, def, streamBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (should succeed via member B)", rr.Code)
	}
}
