package handler_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/authz"
	"gatewai/gateway/internal/cache"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/llmproxy"
	"gatewai/gateway/internal/llmproxy/provider"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
)

// mockRateLimiter is a configurable Checker stub for testing.
type mockRateLimiter struct {
	result ratelimit.CheckResult
	err    error
}

func (m *mockRateLimiter) Check(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return m.result, m.err
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func buildRegistry() *service.Registry {
	cfgs := []config.ServiceConfig{
		{
			Type:  "transcription",
			Model: "whisper-large-v3",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			InferenceURL:  "http://inference.example.com",
			AcceptedExts:  []string{".mp3", ".wav"},
			MaxFileSizeMB: 100,
		},
	}
	return service.NewRegistry(cfgs)
}

func multipartRequest(t *testing.T, path, modelName string, fileContent []byte) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("model", modelName)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write(fileContent)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestSyncHandler_JSONAlwaysUsesDirectProxy verifies that JSON requests always go
// through the direct proxy.
func TestSyncHandler_JSONAlwaysUsesDirectProxy(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "ocr",
		Model: "llava",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: upstream.URL,
	}}
	reg := service.NewRegistry(cfgs)

	h := handler.NewSyncHandler(reg, "", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llava","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if !upstreamCalled {
		t.Error("JSON request should have been proxied to upstream, but upstream was not called")
	}
}

// TestSyncHandler_MultipartUsesDirectProxy verifies that multipart requests are
// proxied directly to the inference backend.
func TestSyncHandler_MultipartUsesDirectProxy(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: upstream.URL,
		AcceptedExts: []string{".mp3", ".wav"},
	}}
	reg := service.NewRegistry(cfgs)

	h := handler.NewSyncHandler(reg, "", nil, nil)
	req := multipartRequest(t, "/v1/audio/transcriptions", "whisper-large-v3", []byte("fake audio"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if !upstreamCalled {
		t.Error("multipart request should proxy to upstream, but upstream was not called")
	}
}

// TestSyncHandler_MissingModelField_SingleModel verifies that when only one model
// is registered for a path, omitting the "model" field auto-selects that model.
func TestSyncHandler_MissingModelField_SingleModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL:  upstream.URL,
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
	}}
	reg := service.NewRegistry(cfgs)
	h := handler.NewSyncHandler(reg, "", nil, nil)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write([]byte("audio"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	// Single registered model → auto-selected, request succeeds.
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when single model is auto-selected, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSyncHandler_JSONBodyTooLarge_Returns413 verifies that a JSON body larger
// than the default 1 MiB cap is rejected cleanly with 413 (not silently
// truncated), and the upstream is never contacted.
func TestSyncHandler_JSONBodyTooLarge_Returns413(t *testing.T) {
	var upstreamCalled atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:         "ocr",
		Model:        "llava",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		InferenceURL: upstream.URL,
	}}
	reg := service.NewRegistry(cfgs)
	h := handler.NewSyncHandler(reg, "", nil, nil) // default 1 MiB cap

	big := strings.Repeat("a", 2<<20) // 2 MiB payload, exceeds the 1 MiB default
	body := `{"model":"llava","messages":[{"role":"user","content":"` + big + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized body, got %d: %s", w.Code, w.Body.String())
	}
	if upstreamCalled.Load() {
		t.Error("upstream should not be contacted when the body is rejected")
	}
}

// TestSyncHandler_WithMaxBodyMB_AllowsLargerBody verifies that raising the cap
// via WithMaxBodyMB lets a body through that the default would have rejected.
func TestSyncHandler_WithMaxBodyMB_AllowsLargerBody(t *testing.T) {
	var upstreamCalled atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:         "ocr",
		Model:        "llava",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		InferenceURL: upstream.URL,
	}}
	reg := service.NewRegistry(cfgs)
	h := handler.NewSyncHandler(reg, "", nil, nil).WithMaxBodyMB(5) // 5 MiB cap

	big := strings.Repeat("a", 2<<20) // 2 MiB — over default, under the 5 MiB cap
	body := `{"model":"llava","messages":[{"role":"user","content":"` + big + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for body under the raised cap, got %d: %s", w.Code, w.Body.String())
	}
	if !upstreamCalled.Load() {
		t.Error("upstream should have been contacted for an in-limit body")
	}
}

// TestSyncHandler_MissingModelField_MultipleModels verifies that when multiple
// models are registered for a path, omitting "model" returns 400.
func TestSyncHandler_MissingModelField_MultipleModels(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{
			Type:  "transcription",
			Model: "whisper-large-v3",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			InferenceURL: "http://inference.example.com",
		},
		{
			Type:  "transcription",
			Model: "whisper-turbo",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			InferenceURL: "http://inference-turbo.example.com",
		},
	}
	reg := service.NewRegistry(cfgs)
	h := handler.NewSyncHandler(reg, "", nil, nil)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write([]byte("audio"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when multiple models and no model field, got %d", w.Code)
	}
}

// TestSyncHandler_UnknownModel verifies that an unknown model returns 400.
func TestSyncHandler_UnknownModel(t *testing.T) {
	// Two models share the path, so the "model" field is required and validated —
	// an unknown model is rejected. (Single-model paths route by path and stream,
	// treating the model field as advisory.)
	cfgs := []config.ServiceConfig{
		{Type: "transcription", Model: "whisper-large-v3", Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}}, InferenceURL: "http://inference.example.com", AcceptedExts: []string{".wav"}, MaxFileSizeMB: 100},
		{Type: "transcription", Model: "whisper-small", Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}}, InferenceURL: "http://inference2.example.com", AcceptedExts: []string{".wav"}, MaxFileSizeMB: 100},
	}
	h := handler.NewSyncHandler(service.NewRegistry(cfgs), "", nil, nil)

	req := multipartRequest(t, "/v1/audio/transcriptions", "unknown-model", []byte("audio"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown model on a multi-model path, got %d", w.Code)
	}
}

// TestSyncHandler_UnsupportedContentType verifies that unsupported content types
// return 415.
func TestSyncHandler_UnsupportedContentType(t *testing.T) {
	reg := buildRegistry()
	h := handler.NewSyncHandler(reg, "", nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader("plain text body"))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", w.Code)
	}
}

// ── Multi-backend tests ────────────────────────────────────────────────────────

func buildMultiBackendRegistry(backends []config.BackendConfig) *service.Registry {
	cfgs := []config.ServiceConfig{{
		Type:  "ocr",
		Model: "llava",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		Backends: backends,
	}}
	return service.NewRegistry(cfgs)
}

func jsonRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(`{"model":"llava","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestSyncHandler_MultiBackend_PrimaryFails_FallbackUsed(t *testing.T) {
	fallbackCalled := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer fallback.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: primary.URL, Weight: 100},
		{URL: fallback.URL, Weight: 10},
	})
	h := handler.NewSyncHandler(reg, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from fallback, got %d", w.Code)
	}
	if !fallbackCalled {
		t.Error("fallback backend was not called")
	}
}

func TestSyncHandler_MultiBackend_NetworkError_FallbackUsed(t *testing.T) {
	fallbackCalled := false
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer fallback.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 100}, // unreachable
		{URL: fallback.URL, Weight: 10},
	})
	h := handler.NewSyncHandler(reg, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from fallback, got %d", w.Code)
	}
	if !fallbackCalled {
		t.Error("fallback backend was not called")
	}
}

func TestSyncHandler_MultiBackend_AllFail_Returns502(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer secondary.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: primary.URL, Weight: 100},
		{URL: secondary.URL, Weight: 10},
	})
	h := handler.NewSyncHandler(reg, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 when all backends fail, got %d", w.Code)
	}
}

func TestSyncHandler_MultiBackend_WeightZero_UsedAsFallback(t *testing.T) {
	fallbackCalled := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer fallback.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: primary.URL, Weight: 100},
		{URL: fallback.URL, Weight: 0}, // fallback-only
	})
	h := handler.NewSyncHandler(reg, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from weight=0 fallback, got %d", w.Code)
	}
	if !fallbackCalled {
		t.Error("weight=0 fallback backend was not called after primary failure")
	}
}

func TestSyncHandler_MultiBackend_4xx_NotRetried(t *testing.T) {
	callCount := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer fallback.Close()

	// fallback weight=0 ensures primary is always selected first (deterministic).
	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: primary.URL, Weight: 100},
		{URL: fallback.URL, Weight: 0},
	})
	h := handler.NewSyncHandler(reg, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 forwarded as-is, got %d", w.Code)
	}
	if callCount != 1 {
		t.Errorf("4xx should not trigger retry; backend called %d time(s)", callCount)
	}
}

func TestSyncHandler_BackwardCompat_SingleInferenceURL(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "ocr",
		Model: "llava",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: upstream.URL, // legacy single-URL config
	}}
	reg := service.NewRegistry(cfgs)
	h := handler.NewSyncHandler(reg, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with legacy inference_url, got %d", w.Code)
	}
	if !called {
		t.Error("upstream was not called")
	}
}

func buildRetryRegistry(inferenceURL string, retries int) *service.Registry {
	cfgs := []config.ServiceConfig{{
		Type:  "ocr",
		Model: "llava",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: inferenceURL,
		Retries:      retries,
	}}
	return service.NewRegistry(cfgs)
}

// TestSyncHandler_Retry_SucceedsAfterFailure verifies that the handler retries on 5xx
// and succeeds once the backend recovers.
func TestSyncHandler_Retry_SucceedsAfterFailure(t *testing.T) {
	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n < 3 { // first 2 calls fail
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	reg := buildRetryRegistry(upstream.URL, 2) // 2 retries = 3 total attempts
	h := handler.NewSyncHandler(reg, "", nil, nil).
		WithRetryBackoff(5 * time.Millisecond)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 after retry, got %d", w.Code)
	}
	if n := callCount.Load(); n != 3 {
		t.Errorf("expected 3 upstream calls (1 initial + 2 retries), got %d", n)
	}
}

// TestSyncHandler_Retry_ExhaustsAllAttempts verifies that 502 is returned after all
// retry cycles fail.
func TestSyncHandler_Retry_ExhaustsAllAttempts(t *testing.T) {
	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	reg := buildRetryRegistry(upstream.URL, 1) // 1 retry = 2 total attempts
	h := handler.NewSyncHandler(reg, "", nil, nil).
		WithRetryBackoff(5 * time.Millisecond)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 after all retries exhausted, got %d", w.Code)
	}
	if n := callCount.Load(); n != 2 {
		t.Errorf("expected 2 upstream calls (1 initial + 1 retry), got %d", n)
	}
}

// TestSyncHandler_Retry_NoRetryOn4xx verifies that 4xx responses are not retried.
func TestSyncHandler_Retry_NoRetryOn4xx(t *testing.T) {
	var callCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()

	reg := buildRetryRegistry(upstream.URL, 3)
	h := handler.NewSyncHandler(reg, "", nil, nil).
		WithRetryBackoff(5 * time.Millisecond)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 forwarded as-is, got %d", w.Code)
	}
	if n := callCount.Load(); n != 1 {
		t.Errorf("4xx must not trigger retry; backend called %d time(s)", n)
	}
}

// ── Guardrails PII tests ──────────────────────────────────────────────────────

// buildLLMHandler returns a minimal llmproxy.Handler wired to the given backend URL.
func buildLLMHandler(backendURL string) *llmproxy.Handler {
	return llmproxy.New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, llmproxy.AuditConfig{}, nil)
}

// buildLLMRegistryDisabled returns a registry with a single openai LLM service
// with guardrails disabled.
func buildLLMRegistryDisabled(backendURL string) *service.Registry {
	cfgs := []config.ServiceConfig{{
		Type:     "llm",
		Model:    "gpt-4o",
		Provider: "openai",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: backendURL,
		Backends:     []config.BackendConfig{{URL: backendURL, Weight: 1}},
		Guardrails:   config.GuardrailsConfig{},
	}}
	return service.NewRegistry(cfgs)
}

// buildLLMRegistryWithGuardrails returns a registry with explicit action+checks guardrails config.
func buildLLMRegistryWithGuardrails(backendURL string, action string, checks []string) *service.Registry {
	cfgs := []config.ServiceConfig{{
		Type:     "llm",
		Model:    "gpt-4o",
		Provider: "openai",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: backendURL,
		Backends:     []config.BackendConfig{{URL: backendURL, Weight: 1}},
		Guardrails:   config.GuardrailsConfig{Action: action, Checks: checks},
	}}
	return service.NewRegistry(cfgs)
}

// piiJSONRequest builds a POST /v1/chat/completions request with the given message content.
func piiJSONRequest(content string) *http.Request {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + content + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestSyncHandler_Guardrails_PII_Blocked verifies that a request containing a
// recognised PII pattern (email) is rejected with 422 when the guardrails block action is set.
func TestSyncHandler_Guardrails_PII_Blocked(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	reg := buildLLMRegistryWithGuardrails(backend.URL, "block", []string{"pii"})
	h := handler.NewSyncHandler(reg, "", nil, buildLLMHandler(backend.URL))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, piiJSONRequest("Mon email est alice@example.com"))

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for PII request, got %d", w.Code)
	}
	if backendCalled {
		t.Error("backend must not be called when PII is detected")
	}
	if !strings.Contains(w.Body.String(), "guardrails violation") {
		t.Errorf("expected 'guardrails violation' in body, got: %s", w.Body.String())
	}
}

// TestSyncHandler_Guardrails_PII_Disabled_AllowsThrough verifies that PII content
// passes through unchanged when guardrails are disabled.
func TestSyncHandler_Guardrails_PII_Disabled_AllowsThrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{}}`)
	}))
	defer backend.Close()

	reg := buildLLMRegistryDisabled(backend.URL) // guardrails disabled
	h := handler.NewSyncHandler(reg, "", nil, buildLLMHandler(backend.URL))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, piiJSONRequest("Mon email est alice@example.com"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when PII guard is off, got %d", w.Code)
	}
}

// buildLLMRegistry returns a registry with a single openai LLM service with the
// guardrails block action enabled (checks: [pii]).
func buildLLMRegistry(backendURL string) *service.Registry {
	return buildLLMRegistryWithGuardrails(backendURL, "block", []string{"pii"})
}

// TestSyncHandler_Guardrails_NoPII_PassesThrough verifies that a clean request is
// not blocked when the guardrails block action is enabled.
func TestSyncHandler_Guardrails_NoPII_PassesThrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{}}`)
	}))
	defer backend.Close()

	reg := buildLLMRegistry(backend.URL) // guardrails block action enabled
	h := handler.NewSyncHandler(reg, "", nil, buildLLMHandler(backend.URL))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, piiJSONRequest("Quelle est la capitale de la France ?"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for clean request, got %d", w.Code)
	}
}

// TestSyncHandler_Guardrails_PII_MetricIncremented verifies that the Prometheus
// counter is incremented exactly once when a PII-containing request is blocked.
func TestSyncHandler_Guardrails_PII_MetricIncremented(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	reg := buildLLMRegistry(backend.URL)
	h := handler.NewSyncHandler(reg, "", nil, buildLLMHandler(backend.URL))

	counter := metrics.GuardrailsPiiBlockedTotal.WithLabelValues("llm", "gpt-4o")
	before := testutil.ToFloat64(counter)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, piiJSONRequest("IBAN: FR7630006000011234567890189"))

	after := testutil.ToFloat64(counter)
	if after-before != 1 {
		t.Errorf("expected counter to increment by 1, got delta %.0f", after-before)
	}
}

// TestSyncHandler_Guardrails_ConsumerHeader_LoggedOnBlock verifies that the consumer
// header value is available for logging when a request is blocked by the guardrail.
func TestSyncHandler_Guardrails_ConsumerHeader_LoggedOnBlock(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	reg := buildLLMRegistry(backend.URL)
	h := handler.NewSyncHandler(reg, "X-Consumer-Username", nil, buildLLMHandler(backend.URL))

	req := piiJSONRequest("Tel: +33612345678")
	req.Header.Set("X-Consumer-Username", "alice")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", w.Code)
	}
}

// TestSyncHandler_Guardrails_Action_Block verifies that action=block rejects
// the request with 422 and does not call the backend.
func TestSyncHandler_Guardrails_Action_Block(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	reg := buildLLMRegistryWithGuardrails(backend.URL, "block", []string{"pii"})
	h := handler.NewSyncHandler(reg, "", nil, buildLLMHandler(backend.URL))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, piiJSONRequest("Mon email est alice@example.com"))

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for block action, got %d", w.Code)
	}
	if backendCalled {
		t.Error("backend must not be called when action=block and PII detected")
	}
}

// TestSyncHandler_Guardrails_Action_Redact verifies that action=redact forwards the
// request to the backend with PII replaced, not the original body.
func TestSyncHandler_Guardrails_Action_Redact(t *testing.T) {
	var forwardedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{}}`)
	}))
	defer backend.Close()

	reg := buildLLMRegistryWithGuardrails(backend.URL, "redact", []string{"pii"})
	h := handler.NewSyncHandler(reg, "", nil, buildLLMHandler(backend.URL))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, piiJSONRequest("Mon email est alice@example.com"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for redact action, got %d: %s", w.Code, w.Body.String())
	}
	if len(forwardedBody) == 0 {
		t.Fatal("backend was not called")
	}
	if strings.Contains(string(forwardedBody), "alice@example.com") {
		t.Errorf("raw PII must not appear in the forwarded body; got: %s", string(forwardedBody))
	}
}

// TestSyncHandler_Guardrails_Action_Flag verifies that action=flag forwards the
// ORIGINAL body to the backend (no blocking, no redaction).
func TestSyncHandler_Guardrails_Action_Flag(t *testing.T) {
	const piiContent = "Mon email est alice@example.com"
	var forwardedBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{}}`)
	}))
	defer backend.Close()

	reg := buildLLMRegistryWithGuardrails(backend.URL, "flag", []string{"pii"})
	h := handler.NewSyncHandler(reg, "", nil, buildLLMHandler(backend.URL))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, piiJSONRequest(piiContent))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for flag action, got %d: %s", w.Code, w.Body.String())
	}
	if len(forwardedBody) == 0 {
		t.Fatal("backend was not called")
	}
	// Flag action must forward the ORIGINAL body, PII intact.
	if !strings.Contains(string(forwardedBody), "alice@example.com") {
		t.Errorf("original body must be forwarded for flag action; got: %s", string(forwardedBody))
	}
}

// ── X-RateLimit header tests ──────────────────────────────────────────────────

// TestSyncHandler_RateLimitHeaders_SetOnAllowedRequest verifies that
// X-RateLimit-{Limit,Remaining,Reset} headers are present on allowed requests.
func TestSyncHandler_RateLimitHeaders_SetOnAllowedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: upstream.URL,
	}}
	reg := service.NewRegistry(cfgs)

	rl := &mockRateLimiter{result: ratelimit.CheckResult{
		Allowed:    true,
		Limit:      10,
		Remaining:  9,
		ResetAfter: 30 * time.Second,
	}}
	h := handler.NewSyncHandler(reg, "", rl, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader(`{"model":"whisper-large-v3"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	for _, hdr := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset"} {
		if w.Header().Get(hdr) == "" {
			t.Errorf("expected header %q to be set, but it was empty", hdr)
		}
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "10" {
		t.Errorf("X-RateLimit-Limit: expected '10', got %q", got)
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "9" {
		t.Errorf("X-RateLimit-Remaining: expected '9', got %q", got)
	}
	// Reset must be a unix timestamp in the future.
	resetStr := w.Header().Get("X-RateLimit-Reset")
	resetTs, err := strconv.ParseInt(resetStr, 10, 64)
	if err != nil {
		t.Fatalf("X-RateLimit-Reset %q is not a valid int64: %v", resetStr, err)
	}
	if resetTs <= time.Now().Unix() {
		t.Errorf("X-RateLimit-Reset %d should be in the future", resetTs)
	}
}

// TestSyncHandler_RateLimitHeaders_SetOnRejectedRequest verifies that
// all three X-RateLimit-* headers plus Retry-After are set on 429 responses.
func TestSyncHandler_RateLimitHeaders_SetOnRejectedRequest(t *testing.T) {
	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: "http://should-not-be-called.example.com",
	}}
	reg := service.NewRegistry(cfgs)

	rl := &mockRateLimiter{result: ratelimit.CheckResult{
		Allowed:    false,
		Limit:      5,
		Remaining:  0,
		ResetAfter: 45 * time.Second,
	}}
	h := handler.NewSyncHandler(reg, "", rl, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader(`{"model":"whisper-large-v3"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}
	for _, hdr := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"} {
		if w.Header().Get(hdr) == "" {
			t.Errorf("expected header %q to be set on 429, but it was empty", hdr)
		}
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "5" {
		t.Errorf("X-RateLimit-Limit: expected '5', got %q", got)
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining: expected '0', got %q", got)
	}
}

// TestSyncHandler_RateLimitHeaders_AbsentWhenNoLimiter verifies that no
// X-RateLimit-* headers are set when the rate limiter is not configured.
// mockProcessingLimiter is a test double for ratelimit.ProcessingTimeChecker.
type mockProcessingLimiter struct {
	checkResult ratelimit.CheckResult
	addCalled   bool
	addSeconds  float64
}

func (m *mockProcessingLimiter) CheckProcessingTime(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return m.checkResult, nil
}

func (m *mockProcessingLimiter) AddProcessingTime(_ context.Context, _, _, _ string, s float64) error {
	m.addCalled = true
	m.addSeconds = s
	return nil
}

// mockUsageTrackerSync is a test double for usage.UsageTracker that records
// TrackTokens and TrackProcessingTime calls; the other methods are no-ops.
type mockUsageTrackerSync struct {
	tokenCalls []struct {
		consumer, serviceType string
		prompt, completion    int64
	}
	processingTimeCalls []struct {
		consumer, serviceType string
		seconds               float64
	}
}

func (m *mockUsageTrackerSync) TrackRequest(_ context.Context, _, _ string) {}
func (m *mockUsageTrackerSync) TrackJob(_ context.Context, _, _ string)     {}
func (m *mockUsageTrackerSync) TrackProcessingTime(_ context.Context, consumer, serviceType string, seconds float64) {
	m.processingTimeCalls = append(m.processingTimeCalls, struct {
		consumer, serviceType string
		seconds               float64
	}{consumer, serviceType, seconds})
}
func (m *mockUsageTrackerSync) TrackActive(_ context.Context, _ string)         {}
func (m *mockUsageTrackerSync) TrackUserType(_ context.Context, _, _, _ string) {}
func (m *mockUsageTrackerSync) UpdateRetention(_ time.Duration)                 {}
func (m *mockUsageTrackerSync) TrackTokens(_ context.Context, consumer, serviceType string, prompt, completion int64) {
	m.tokenCalls = append(m.tokenCalls, struct {
		consumer, serviceType string
		prompt, completion    int64
	}{consumer, serviceType, prompt, completion})
}

// TestProxyToInference_TokenLimit_DeniedBeforeBackendCall verifies a 429 is
// returned before any backend HTTP call when the token budget is exhausted.
func TestProxyToInference_TokenLimit_DeniedBeforeBackendCall(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "faster-whisper",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: backend.URL,
	}}
	reg := service.NewRegistry(cfgs)
	tLimiter := &mockTokenLimiter{result: ratelimit.CheckResult{Allowed: false, Limit: 10000, Remaining: 0, ResetAfter: time.Hour}}

	sh := handler.NewSyncHandler(reg, "X-Consumer-Username", nil, nil).WithTokenLimiter(tLimiter)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"model":"faster-whisper"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	sh.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d: %s", w.Code, w.Body.String())
	}
	if backendCalled {
		t.Error("backend must not be called when token budget is exceeded")
	}
}

// TestProxyToInference_TokenLimit_SuccessTracksTokens verifies a successful
// response triggers AddTokens with the parsed usage total and TrackTokens on
// the usage tracker.
func TestProxyToInference_TokenLimit_SuccessTracksTokens(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"text":"hello","usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`)
	}))
	defer backend.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "faster-whisper",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: backend.URL,
	}}
	reg := service.NewRegistry(cfgs)
	tLimiter := &mockTokenLimiter{result: ratelimit.CheckResult{Allowed: true}}
	tracker := &mockUsageTrackerSync{}

	sh := handler.NewSyncHandler(reg, "X-Consumer-Username", nil, nil).
		WithTokenLimiter(tLimiter).
		WithUsageTracker(tracker)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"model":"faster-whisper"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Consumer-Username", "alice")
	w := httptest.NewRecorder()
	sh.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(tLimiter.addCalls) != 1 {
		t.Fatalf("expected 1 AddTokens call, got %d", len(tLimiter.addCalls))
	}
	if tLimiter.addCalls[0].total != 120 {
		t.Errorf("total: got %d, want 120", tLimiter.addCalls[0].total)
	}
	if len(tracker.tokenCalls) != 1 {
		t.Fatalf("expected 1 TrackTokens call, got %d", len(tracker.tokenCalls))
	}
	if tracker.tokenCalls[0].prompt != 100 || tracker.tokenCalls[0].completion != 20 {
		t.Errorf("TrackTokens: got prompt=%d completion=%d, want 100/20", tracker.tokenCalls[0].prompt, tracker.tokenCalls[0].completion)
	}
}

// TestProxyToInference_IsLLMService_SkipsTokenLogic is a regression guard:
// proxyToInference must not run token check/capture logic for LLM-provider
// services, even when reached via the multipart path (which has no IsLLM
// gate), to avoid double-counting against the independent llmproxy tracking.
func TestProxyToInference_IsLLMService_SkipsTokenLogic(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	}))
	defer backend.Close()

	cfgs := []config.ServiceConfig{{
		Type:     "llm",
		Model:    "some-llm-model",
		Provider: "openai",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: backend.URL,
		AcceptedExts: []string{".wav"},
	}}
	reg := service.NewRegistry(cfgs)
	tLimiter := &mockTokenLimiter{result: ratelimit.CheckResult{Allowed: true}}

	sh := handler.NewSyncHandler(reg, "X-Consumer-Username", nil, nil).WithTokenLimiter(tLimiter)

	req := multipartRequest(t, "/v1/chat/completions", "some-llm-model", []byte("data"))
	w := httptest.NewRecorder()
	sh.ServeHTTP(w, req)

	if len(tLimiter.addCalls) != 0 {
		t.Errorf("expected no AddTokens call for an IsLLM() service, got %d calls (double-counting risk)", len(tLimiter.addCalls))
	}
}

// TestSyncHandler_ProcessingTimeLimitDenied verifies that a 429 is returned when
// the processing time budget is exhausted.
func TestSyncHandler_ProcessingTimeLimitDenied(t *testing.T) {
	limiter := &mockProcessingLimiter{
		checkResult: ratelimit.CheckResult{Allowed: false, Limit: 100, Remaining: 0},
	}
	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: "http://unused.example.com",
	}}
	h := handler.NewSyncHandler(service.NewRegistry(cfgs), "", nil, nil).
		WithProcessingLimiter(limiter, "X-User-Type")

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader(`{"model":"whisper-large-v3"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when processing time budget exceeded, got %d", w.Code)
	}
}

// TestSyncHandler_ProcessingTimeLimitAddsAfterProxy verifies AddProcessingTime is
// called with the processing_time from the upstream JSON response.
func TestSyncHandler_ProcessingTimeLimitAddsAfterProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"text":"hello","processing_time":5.5}`)
	}))
	defer upstream.Close()

	limiter := &mockProcessingLimiter{
		checkResult: ratelimit.CheckResult{Allowed: true, Limit: 100, Remaining: 50},
	}
	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: upstream.URL,
	}}
	h := handler.NewSyncHandler(service.NewRegistry(cfgs), "X-Consumer", nil, nil).
		WithProcessingLimiter(limiter, "X-User-Type")

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader(`{"model":"whisper-large-v3"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Consumer", "consumer-x")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !limiter.addCalled {
		t.Fatal("expected AddProcessingTime to be called after successful proxy")
	}
	if limiter.addSeconds != 5.5 {
		t.Errorf("expected addSeconds=5.5, got %v", limiter.addSeconds)
	}
}

// TestSyncHandler_ProcessingTimeLimitAddsAfterProxy_TracksUsageTotal is a
// regression guard: a successful sync-direct proxy must also feed the
// permanent usage total (usage.UsageTracker.TrackProcessingTime), not just
// the rate-limit window (ratelimit.ProcessingTimeChecker.AddProcessingTime).
// Before this fix, /usage's total.processing_time_seconds never increased
// for sync-direct traffic even though the window quota was tracked correctly.
func TestSyncHandler_ProcessingTimeLimitAddsAfterProxy_TracksUsageTotal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"text":"hello","processing_time":5.5}`)
	}))
	defer upstream.Close()

	limiter := &mockProcessingLimiter{
		checkResult: ratelimit.CheckResult{Allowed: true, Limit: 100, Remaining: 50},
	}
	tracker := &mockUsageTrackerSync{}
	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: upstream.URL,
	}}
	h := handler.NewSyncHandler(service.NewRegistry(cfgs), "X-Consumer", nil, nil).
		WithProcessingLimiter(limiter, "X-User-Type").
		WithUsageTracker(tracker)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader(`{"model":"whisper-large-v3"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Consumer", "consumer-x")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(tracker.processingTimeCalls) != 1 {
		t.Fatalf("expected 1 TrackProcessingTime call, got %d", len(tracker.processingTimeCalls))
	}
	if tracker.processingTimeCalls[0].seconds != 5.5 {
		t.Errorf("TrackProcessingTime: got seconds=%v, want 5.5", tracker.processingTimeCalls[0].seconds)
	}
	if tracker.processingTimeCalls[0].consumer != "consumer-x" {
		t.Errorf("TrackProcessingTime: got consumer=%q, want consumer-x", tracker.processingTimeCalls[0].consumer)
	}
}

func TestSyncHandler_RateLimitHeaders_AbsentWhenNoLimiter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[],"usage":{}}`)
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: upstream.URL,
	}}
	reg := service.NewRegistry(cfgs)

	h := handler.NewSyncHandler(reg, "", nil, nil) // nil limiter

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader(`{"model":"whisper-large-v3"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	for _, hdr := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset"} {
		if v := w.Header().Get(hdr); v != "" {
			t.Errorf("expected header %q to be absent without limiter, got %q", hdr, v)
		}
	}
}

// ── Authz tests ───────────────────────────────────────────────────────────────

// buildAuthzEngine returns an Engine that allows "ok-model" for any principal
// and denies everything else (default deny).
func buildAuthzEngine() *authz.Engine {
	return authz.New(config.PoliciesConfig{
		Rules: []config.PolicyRule{
			{
				Match:       config.PolicyMatch{},
				AllowModels: []string{"ok-model"},
			},
		},
	})
}

// buildAuthzRegistry returns a registry with two models: "ok-model" (allowed)
// and "blocked-model" (denied by the test engine).
func buildAuthzRegistry(backendURL string) *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{
		{
			Type:  "llm",
			Model: "ok-model",
			Operations: map[string][]string{
				"chat": {"/v1/chat/completions"},
			},
			InferenceURL: backendURL,
		},
		{
			Type:    "llm",
			Model:   "blocked-model",
			Default: false,
			Operations: map[string][]string{
				"chat": {"/v1/chat/completions"},
			},
			InferenceURL: backendURL,
		},
	})
}

// withPrincipal injects p into r's context.
func withPrincipal(r *http.Request, p *auth.Principal) *http.Request {
	return r.WithContext(auth.WithPrincipal(r.Context(), p))
}

// TestSyncHandler_Authz_NilEngine_NoEnforcement verifies that a nil engine
// is fully backward-compatible: requests pass through regardless of model.
func TestSyncHandler_Authz_NilEngine_NoEnforcement(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[],"usage":{}}`)
	}))
	defer backend.Close()

	reg := buildAuthzRegistry(backend.URL)
	h := handler.NewSyncHandler(reg, "", nil, nil) // no WithAuthz → nil engine

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"blocked-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("nil engine: expected 200, got %d", w.Code)
	}
	if !backendCalled {
		t.Error("nil engine: backend should have been called")
	}
}

// TestSyncHandler_Authz_DeniedModel_JSON verifies that a JSON request to a
// denied model returns 403 and the backend is NOT called.
func TestSyncHandler_Authz_DeniedModel_JSON(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	reg := buildAuthzRegistry(backend.URL)
	engine := buildAuthzEngine()
	h := handler.NewSyncHandler(reg, "", nil, nil).WithAuthz(engine)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"blocked-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = withPrincipal(req, &auth.Principal{Consumer: "alice", Authenticated: true})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for denied model (JSON), got %d: %s", w.Code, w.Body.String())
	}
	if backendCalled {
		t.Error("backend must not be called when model is denied")
	}
	if !strings.Contains(w.Body.String(), "denied") {
		t.Errorf("expected 'denied' in response body, got: %s", w.Body.String())
	}
}

// TestSyncHandler_Authz_AllowedModel_JSON verifies that an allowed model
// proceeds normally (2xx) when authz is configured.
func TestSyncHandler_Authz_AllowedModel_JSON(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[],"usage":{}}`)
	}))
	defer backend.Close()

	reg := buildAuthzRegistry(backend.URL)
	engine := buildAuthzEngine()
	h := handler.NewSyncHandler(reg, "", nil, nil).WithAuthz(engine)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ok-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = withPrincipal(req, &auth.Principal{Consumer: "alice", Authenticated: true})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for allowed model (JSON), got %d: %s", w.Code, w.Body.String())
	}
}

// TestSyncHandler_Authz_DeniedModel_Multipart verifies that a multipart request
// to a denied model returns 403 without calling the backend.
func TestSyncHandler_Authz_DeniedModel_Multipart(t *testing.T) {
	backendCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// Use a registry with both models under the transcription type.
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:  "transcription",
			Model: "ok-model",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			InferenceURL: backend.URL,
			AcceptedExts: []string{".wav"},
		},
		{
			Type:  "transcription",
			Model: "blocked-model",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			InferenceURL: backend.URL,
			AcceptedExts: []string{".wav"},
		},
	})
	engine := buildAuthzEngine()
	h := handler.NewSyncHandler(reg, "", nil, nil).WithAuthz(engine)

	req := multipartRequest(t, "/v1/audio/transcriptions", "blocked-model", []byte("audio"))
	req = withPrincipal(req, &auth.Principal{Consumer: "alice", Authenticated: true})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for denied model (multipart), got %d: %s", w.Code, w.Body.String())
	}
	if backendCalled {
		t.Error("backend must not be called when model is denied (multipart)")
	}
}

// TestSyncHandler_Authz_DenyMetricIncremented verifies that the deny counter
// is incremented when access is denied.
func TestSyncHandler_Authz_DenyMetricIncremented(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	reg := buildAuthzRegistry(backend.URL)
	engine := buildAuthzEngine()
	h := handler.NewSyncHandler(reg, "", nil, nil).WithAuthz(engine)

	counter := metrics.AuthzDecisionsTotal.WithLabelValues("llm", "blocked-model", "deny")
	before := testutil.ToFloat64(counter)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"blocked-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = withPrincipal(req, &auth.Principal{Consumer: "alice", Authenticated: true})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	after := testutil.ToFloat64(counter)
	if after-before != 1 {
		t.Errorf("expected deny counter to increment by 1, got delta %.0f", after-before)
	}
}

// TestSyncHandler_CrossModelFallback_WhenPrimaryDegraded verifies that a request
// to a model whose backends are all circuit-open is routed to its fallback_model.
func TestSyncHandler_CrossModelFallback_WhenPrimaryDegraded(t *testing.T) {
	fbBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer fbBackend.Close()

	primaryURL := "http://primary.invalid"
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type: "llm", Model: "chat", Provider: "passthrough",
			Operations:    map[string][]string{"chat": {"/v1/chat/completions"}},
			Backends:      []config.BackendConfig{{URL: primaryURL, Weight: 100}},
			FallbackModel: "chat-fb",
		},
		{
			Type: "llm", Model: "chat-fb", Provider: "passthrough",
			Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
			Backends:   []config.BackendConfig{{URL: fbBackend.URL, Weight: 100}},
		},
	})

	llm := llmproxy.New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, llmproxy.AuditConfig{}, nil)
	health := fakeHealth{open: map[string]bool{primaryURL: true}} // primary all-open → degraded
	h := handler.NewSyncHandler(reg, "", nil, llm).WithCircuitBreaker(health)

	before := testutil.ToFloat64(metrics.LLMFallbackTotal.WithLabelValues("llm", "chat", "chat-fb"))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hey"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from fallback backend, got %d: %s", w.Code, w.Body.String())
	}
	if got := testutil.ToFloat64(metrics.LLMFallbackTotal.WithLabelValues("llm", "chat", "chat-fb")); got != before+1 {
		t.Errorf("fallback metric: got %v, want %v", got, before+1)
	}
}

// TestSyncHandler_NoFallback_WhenPrimaryHealthy verifies no fallback when the
// primary is not degraded.
func TestSyncHandler_NoFallback_WhenPrimaryHealthy(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"primary"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer primary.Close()

	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type: "llm", Model: "chat", Provider: "passthrough",
			Operations:    map[string][]string{"chat": {"/v1/chat/completions"}},
			Backends:      []config.BackendConfig{{URL: primary.URL, Weight: 100}},
			FallbackModel: "chat-fb",
		},
		{
			Type: "llm", Model: "chat-fb", Provider: "passthrough",
			Operations: map[string][]string{"chat": {"/v1/chat/completions"}},
			Backends:   []config.BackendConfig{{URL: "http://fb.invalid", Weight: 100}},
		},
	})

	llm := llmproxy.New(cache.NewNoop(), provider.NewRegistry(), &http.Client{Timeout: 5 * time.Second}, "", metrics.NoopTracker{}, llmproxy.AuditConfig{}, nil)
	health := fakeHealth{open: map[string]bool{}} // nothing open → healthy
	h := handler.NewSyncHandler(reg, "", nil, llm).WithCircuitBreaker(health)

	before := testutil.ToFloat64(metrics.LLMFallbackTotal.WithLabelValues("llm", "chat", "chat-fb"))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hey"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "primary") {
		t.Fatalf("expected primary to serve, got %d: %s", w.Code, w.Body.String())
	}
	if got := testutil.ToFloat64(metrics.LLMFallbackTotal.WithLabelValues("llm", "chat", "chat-fb")); got != before {
		t.Errorf("fallback must not fire for a healthy primary: %v → %v", before, got)
	}
}
