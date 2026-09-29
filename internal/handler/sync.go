package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/authz"
	"gatewai/gateway/internal/concurrency"
	"gatewai/gateway/internal/guardrails"
	"gatewai/gateway/internal/llmproxy"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/usage"
)

// SyncHandler handles OpenAI-compatible POST /v1/* requests.
//
// All requests are routed directly to the inference backend:
//   - multipart/form-data → reconstruct and proxy to inference_url
//   - application/json    → proxy to inference_url (or LLM proxy handler)
type SyncHandler struct {
	registry          *service.Registry
	httpClient        *http.Client
	consumerHeader    string                          // HTTP header identifying the API consumer (e.g. "X-Consumer-Username")
	rateLimiter       ratelimit.Checker               // nil = no rate limiting
	semaphore         *concurrency.ModelSemaphore     // nil = no concurrency limit
	llm               *llmproxy.Handler               // nil when no LLM services are configured
	piiChecker        *guardrails.Checker             // nil = PII scanning disabled globally
	retryBackoff      time.Duration                   // initial backoff between retry cycles; default 500ms
	processingLimiter ratelimit.ProcessingTimeChecker // nil = no processing time limit
	tokenLimiter      ratelimit.TokenChecker          // nil = no token limit
	userTypeHeader    string
	authz             *authz.Engine               // nil = no enforcement
	usageTracker      usage.UsageTracker          // nil = no usage tracking
	maxBodyBytes      int64                       // max request body on the JSON path; default 1 MiB
	priorityHeader    string                      // HTTP header that grants access to the reserved sync capacity pool (e.g. "X-Priority")
	breaker           BackendHealth               // nil = no circuit-breaker-driven fallback
	poolSemaphore     *concurrency.ModelSemaphore // nil = no backend_pools concurrency limit
	poolLimiter       ratelimit.PoolChecker       // nil = no backend_pools rate limit
	realtimeSessions  *realtimeSessionLimiter     // per-consumer concurrent realtime session cap
}

// WithPoolSemaphore sets the backend_pools concurrency limiter for sync calls.
func (h *SyncHandler) WithPoolSemaphore(s *concurrency.ModelSemaphore) *SyncHandler {
	h.poolSemaphore = s
	return h
}

// WithPoolLimiter sets the backend_pools rate limiter for sync calls.
func (h *SyncHandler) WithPoolLimiter(pl ratelimit.PoolChecker) *SyncHandler {
	h.poolLimiter = pl
	return h
}

// WithCircuitBreaker attaches backend-health info so a request to a fully
// circuit-open model can be re-routed to its configured fallback_model. nil
// disables cross-model fallback.
func (h *SyncHandler) WithCircuitBreaker(b BackendHealth) *SyncHandler {
	h.breaker = b
	return h
}

// defaultMaxBodyBytes caps the sync JSON request body when max_body_mb is unset.
const defaultMaxBodyBytes int64 = 1 << 20 // 1 MiB

func NewSyncHandler(
	registry *service.Registry,
	consumerHeader string,
	rateLimiter ratelimit.Checker,
	llm *llmproxy.Handler,
) *SyncHandler {
	return &SyncHandler{
		registry:       registry,
		consumerHeader: consumerHeader,
		rateLimiter:    rateLimiter,
		llm:            llm,
		piiChecker:     guardrails.New(),
		// Generous timeout for direct-proxy path.
		httpClient:       &http.Client{Timeout: 15 * time.Minute},
		retryBackoff:     500 * time.Millisecond,
		maxBodyBytes:     defaultMaxBodyBytes,
		realtimeSessions: newRealtimeSessionLimiter(),
	}
}

// WithMaxBodyMB sets the maximum request body size (in MiB) accepted on the
// sync JSON path. A value ≤ 0 keeps the default (1 MiB).
func (h *SyncHandler) WithMaxBodyMB(mb int) *SyncHandler {
	if mb > 0 {
		h.maxBodyBytes = int64(mb) << 20
	}
	return h
}

// WithSemaphore sets the per-model concurrency limiter for sync calls.
func (h *SyncHandler) WithSemaphore(s *concurrency.ModelSemaphore) *SyncHandler {
	h.semaphore = s
	return h
}

// WithPriorityHeader sets the HTTP header that grants access to a model's
// reserved sync capacity pool (see service.Def.PriorityReservedSync).
// Mirrors the header used by the async job handler for queue-jump priority.
func (h *SyncHandler) WithPriorityHeader(header string) *SyncHandler {
	h.priorityHeader = header
	return h
}

// WithProcessingLimiter sets the processing time budget limiter.
func (h *SyncHandler) WithProcessingLimiter(l ratelimit.ProcessingTimeChecker, userTypeHeader string) *SyncHandler {
	h.processingLimiter = l
	h.userTypeHeader = userTypeHeader
	return h
}

// WithTokenLimiter sets the token budget limiter.
func (h *SyncHandler) WithTokenLimiter(tc ratelimit.TokenChecker) *SyncHandler {
	h.tokenLimiter = tc
	return h
}

// WithRetryBackoff overrides the initial backoff between retry cycles.
// Intended for tests to avoid sleeping the full 500ms.
func (h *SyncHandler) WithRetryBackoff(d time.Duration) *SyncHandler {
	h.retryBackoff = d
	return h
}

// WithAuthz sets the authorization engine. nil disables enforcement (default).
func (h *SyncHandler) WithAuthz(e *authz.Engine) *SyncHandler {
	h.authz = e
	return h
}

// WithUsageTracker attaches a usage tracker. Call before serving requests.
func (h *SyncHandler) WithUsageTracker(t usage.UsageTracker) *SyncHandler {
	h.usageTracker = t
	return h
}

// checkAccess enforces the authz policy for (serviceType, model).
// Returns true when the request is allowed to proceed, false when it was rejected
// (the 403 response has already been written to w).
// checkAccess returns the (possibly context-augmented) request and whether the
// caller may proceed. When the granting policy rule carries per-group limits,
// they are stashed in the request context so the rate/token limiters enforce
// them per-consumer downstream.
func (h *SyncHandler) checkAccess(w http.ResponseWriter, r *http.Request, def *service.Def) (*http.Request, bool) {
	if h.authz == nil {
		return r, true
	}
	p, _ := auth.FromContext(r.Context())
	decision := h.authz.Evaluate(p, def.Type, def.Model)
	if decision.Allowed {
		metrics.AuthzDecisionsTotal.WithLabelValues(def.Type, def.Model, "allow").Inc()
		if decision.Limits != nil {
			r = r.WithContext(ratelimit.WithPolicyLimits(r.Context(), decision.Limits))
		}
		return r, true
	}
	consumer := ""
	if p != nil {
		consumer = p.Consumer
	}
	slog.WarnContext(r.Context(), "access denied by policy", "service_type", def.Type, "model", def.Model, "consumer", consumer)
	metrics.AuthzDecisionsTotal.WithLabelValues(def.Type, def.Model, "deny").Inc()
	writeError(w, http.StatusForbidden, fmt.Sprintf("access to model %q denied", def.Model))
	return r, false
}

func (h *SyncHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))

	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		h.handleMultipart(w, r)

	case ct == "application/json":
		h.handleJSON(w, r)

	default:
		writeError(w, http.StatusUnsupportedMediaType,
			"Content-Type must be multipart/form-data or application/json")
	}
}

func (h *SyncHandler) handleMultipart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}

	// model is optional when the path pattern embeds the model name
	// (e.g. /v2/models/{model}/infer). RouteSync extracts it from the URL.
	modelName := r.FormValue("model")

	def, err := h.registry.RouteSync(r.URL.Path, modelName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !checkModelVisible(w, r, def, h.userTypeHeader) {
		return
	}

	r, accessOK := h.checkAccess(w, r, def)
	if !accessOK {
		return
	}

	if h.rateLimiter != nil {
		rl, err := h.rateLimiter.Check(r.Context(), r, def.Type)
		if err != nil {
			slog.ErrorContext(r.Context(), "rate limit check failed", "error", err)
		} else {
			setRateLimitHeaders(w, rl)
			if !rl.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(rl.ResetAfter.Seconds())))
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
		}
	}

	if h.processingLimiter != nil {
		pr, err := h.processingLimiter.CheckProcessingTime(r.Context(), r, def.Type)
		if err != nil {
			slog.ErrorContext(r.Context(), "processing time check failed", "error", err)
			// fail open
		} else {
			if pr.Limit > 0 {
				w.Header().Set("X-ProcessingTime-Limit", strconv.Itoa(pr.Limit))
				w.Header().Set("X-ProcessingTime-Remaining", strconv.Itoa(pr.Remaining))
			}
			if !pr.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(pr.ResetAfter.Seconds())))
				metrics.ProcessingTimeChecksTotal.WithLabelValues(def.Type, r.Header.Get(h.userTypeHeader), "denied").Inc()
				writeError(w, http.StatusTooManyRequests, "processing time budget exceeded")
				return
			}
			metrics.ProcessingTimeChecksTotal.WithLabelValues(def.Type, r.Header.Get(h.userTypeHeader), "allowed").Inc()
		}
	}

	if h.semaphore != nil {
		isPriority := h.priorityHeader != "" && r.Header.Get(h.priorityHeader) != ""
		acquired, usedReserved := h.semaphore.TryAcquire(def.Model, isPriority)
		if !acquired {
			writeError(w, http.StatusServiceUnavailable, "model too busy, retry later")
			return
		}
		defer h.semaphore.Release(def.Model, usedReserved)
	}

	bodyRC, contentType, err := reconstructMultipart(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rebuild request: "+err.Error())
		return
	}
	defer bodyRC.Close()
	bodyBytes, err := io.ReadAll(bodyRC)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read request body: "+err.Error())
		return
	}
	h.proxyToInference(w, r, def, bodyBytes, contentType)
}

func (h *SyncHandler) handleJSON(w http.ResponseWriter, r *http.Request) {
	limit := h.maxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	// MaxBytesReader rejects oversized bodies with a typed error instead of
	// silently truncating (as io.LimitReader would), so a too-large request —
	// e.g. a base64 image on the vision path — gets a clean 413 rather than a
	// confusing "unexpected end of JSON input".
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds limit of %d bytes (%d MiB)", limit, limit>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}

	var payload struct {
		Model string `json:"model"`
	}
	// model may be empty for path-pattern routes (e.g. /v2/models/{model}/infer).
	// Report malformed JSON bodies; an empty body is treated as no model specified.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	def, err := h.registry.RouteSync(r.URL.Path, payload.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Cross-model fallback: when the primary model's backends are all
	// circuit-open, route to its configured fallback model instead. Done before
	// visibility/authz so those are enforced against the model actually served.
	if def.FallbackModel != "" && backendsDegraded(h.breaker, def.Backends) {
		if fb, fbErr := h.registry.RouteSync(r.URL.Path, def.FallbackModel); fbErr == nil && fb.IsLLM() {
			slog.WarnContext(r.Context(), "primary model degraded, routing to fallback",
				"service_type", def.Type, "model", def.Model, "fallback", fb.Model)
			metrics.LLMFallbackTotal.WithLabelValues(def.Type, def.Model, fb.Model).Inc()
			def = fb
		}
	}

	if !checkModelVisible(w, r, def, h.userTypeHeader) {
		return
	}

	r, accessOK := h.checkAccess(w, r, def)
	if !accessOK {
		return
	}

	if h.rateLimiter != nil {
		rl, err := h.rateLimiter.Check(r.Context(), r, def.Type)
		if err != nil {
			slog.ErrorContext(r.Context(), "rate limit check failed", "error", err)
		} else {
			setRateLimitHeaders(w, rl)
			if !rl.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(rl.ResetAfter.Seconds())))
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
		}
	}

	if h.processingLimiter != nil {
		pr, err := h.processingLimiter.CheckProcessingTime(r.Context(), r, def.Type)
		if err != nil {
			slog.ErrorContext(r.Context(), "processing time check failed", "error", err)
			// fail open
		} else {
			if pr.Limit > 0 {
				w.Header().Set("X-ProcessingTime-Limit", strconv.Itoa(pr.Limit))
				w.Header().Set("X-ProcessingTime-Remaining", strconv.Itoa(pr.Remaining))
			}
			if !pr.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(pr.ResetAfter.Seconds())))
				metrics.ProcessingTimeChecksTotal.WithLabelValues(def.Type, r.Header.Get(h.userTypeHeader), "denied").Inc()
				writeError(w, http.StatusTooManyRequests, "processing time budget exceeded")
				return
			}
			metrics.ProcessingTimeChecksTotal.WithLabelValues(def.Type, r.Header.Get(h.userTypeHeader), "allowed").Inc()
		}
	}

	if h.semaphore != nil {
		isPriority := h.priorityHeader != "" && r.Header.Get(h.priorityHeader) != ""
		acquired, usedReserved := h.semaphore.TryAcquire(def.Model, isPriority)
		if !acquired {
			writeError(w, http.StatusServiceUnavailable, "model too busy, retry later")
			return
		}
		defer h.semaphore.Release(def.Model, usedReserved)
	}

	// JSON requests: route through LLM proxy if configured, else direct proxy.
	// NOTE: backend_pools concurrency/rate limiting for the LLM branch below is
	// enforced inside llmproxy.Handler itself (WithPoolSemaphore/WithPoolLimiter);
	// duplicating it here would double-acquire one pool slot per request. The
	// direct-proxy branch (proxyToInference, reached both here and from
	// handleMultipart) enforces it itself instead — see proxyToInference.
	if h.llm != nil && def.IsLLM() {
		if def.Guardrails.Input.Enabled {
			g := def.Guardrails.Input
			metrics.GuardrailsEvaluationsTotal.WithLabelValues(def.Type, def.Model, "input").Inc()
			consumer := ""
			if h.consumerHeader != "" {
				consumer = r.Header.Get(h.consumerHeader)
			}

			// ── Regex checks (deterministic, zero-latency) ────────────────────
			if len(g.Checks) > 0 && h.piiChecker != nil {
				switch g.Action {
				case "redact":
					cleaned, found := h.piiChecker.Redact(raw, g.Checks)
					if len(found) > 0 {
						raw = cleaned
						slog.WarnContext(r.Context(), "llm request redacted by guardrails",
							"service_type", def.Type, "model", def.Model, "consumer", consumer, "violations", found)
						metrics.GuardrailsTotal.WithLabelValues(def.Type, def.Model, "input", "redact", "redacted").Inc()
						guardrails.MarkSpanFlagged(r.Context(), "input", "redact", found)
					}
				case "flag":
					if found := h.piiChecker.Scan(raw, g.Checks); len(found) > 0 {
						slog.WarnContext(r.Context(), "llm request flagged by guardrails",
							"service_type", def.Type, "model", def.Model, "consumer", consumer, "violations", found)
						metrics.GuardrailsTotal.WithLabelValues(def.Type, def.Model, "input", "flag", "flagged").Inc()
						guardrails.MarkSpanFlagged(r.Context(), "input", "flag", found)
					}
				default: // "block"
					if found := h.piiChecker.Scan(raw, g.Checks); len(found) > 0 {
						slog.WarnContext(r.Context(), "llm request blocked by guardrails",
							"service_type", def.Type, "model", def.Model, "consumer", consumer, "violations", found)
						metrics.GuardrailsTotal.WithLabelValues(def.Type, def.Model, "input", "block", "blocked").Inc()
						metrics.GuardrailsPiiBlockedTotal.WithLabelValues(def.Type, def.Model).Inc()
						guardrails.MarkSpanFlagged(r.Context(), "input", "block", found)
						writeError(w, http.StatusUnprocessableEntity, "guardrails violation: "+strings.Join(found, ", "))
						return
					}
				}
			}

			// ── Model-backed detectors (semantic; sync can block, async shadows) ──
			//
			// Classifiers (block/flag) judge the ORIGINAL text; redaction only
			// mutates what is forwarded. Order matters: classify first (so a
			// blocked request never forwards), then redact the survivors — and
			// crucially, classifiers must not see redaction placeholders, which
			// otherwise inflate scores (e.g. an injection classifier false-firing
			// on "[REDACTED_EMAIL]").
			if len(g.Models) > 0 {
				texts := guardrails.MessageTexts(raw) // original, pre-redaction

				// async shadow: observe only, detached from the request lifetime.
				asyncCtx := context.WithoutCancel(r.Context())
				guardrails.FireAsync(asyncCtx, g.Models, texts, func(name string, cats []string, score float64) {
					slog.WarnContext(r.Context(), "llm request flagged by async guardrail model (shadow)",
						"service_type", def.Type, "model", def.Model, "detector", name, "consumer", consumer, "violations", cats)
					metrics.GuardrailsModelDetectionsTotal.WithLabelValues(def.Type, def.Model, "input", name, "async", "flagged").Inc()
					// Correlated, PII-redacted record so the shadow flag can be reviewed
					// (the request span has already ended, so span tagging can't reach it).
					if def.Guardrails.Sample.Enabled {
						guardrails.EmitFlaggedSample(asyncCtx, guardrails.FlaggedSample{
							Stage: "input", Detector: name, Categories: cats, Score: score,
							ServiceType: def.Type, Model: def.Model, Consumer: consumer,
						}, texts, def.Guardrails.Sample.RedactGroups)
					}
				})

				// sync block/flag classifiers (on the original text).
				for _, res := range guardrails.EvaluateSync(r.Context(), g.Models, texts) {
					if res.Err != nil || res.Action == guardrails.ActionBlock {
						reason := res.Categories
						if res.Err != nil {
							reason = []string{"detector unavailable"}
						}
						slog.WarnContext(r.Context(), "llm request blocked by guardrail model",
							"service_type", def.Type, "model", def.Model, "detector", res.Name, "consumer", consumer, "violations", reason, "error", res.Err)
						metrics.GuardrailsModelDetectionsTotal.WithLabelValues(def.Type, def.Model, "input", res.Name, "sync", "blocked").Inc()
						guardrails.MarkSpanFlagged(r.Context(), "input", "block", []string{res.Name})
						writeError(w, http.StatusUnprocessableEntity, "guardrails violation: "+strings.Join(reason, ", "))
						return
					}
					slog.WarnContext(r.Context(), "llm request flagged by guardrail model",
						"service_type", def.Type, "model", def.Model, "detector", res.Name, "consumer", consumer, "violations", res.Categories)
					metrics.GuardrailsModelDetectionsTotal.WithLabelValues(def.Type, def.Model, "input", res.Name, "sync", "flagged").Inc()
					guardrails.MarkSpanFlagged(r.Context(), "input", "flag", []string{res.Name})
				}

				// sync NER redaction (mutates the forwarded body) — reached only
				// when the request was not blocked above.
				cleaned, redResults := guardrails.EvaluateRedact(r.Context(), g.Models, raw)
				for _, rr := range redResults {
					if rr.Err != nil {
						slog.WarnContext(r.Context(), "llm request blocked by guardrail model (redactor unavailable)",
							"service_type", def.Type, "model", def.Model, "detector", rr.Name, "consumer", consumer, "error", rr.Err)
						metrics.GuardrailsModelDetectionsTotal.WithLabelValues(def.Type, def.Model, "input", rr.Name, "sync", "blocked").Inc()
						guardrails.MarkSpanFlagged(r.Context(), "input", "block", []string{rr.Name})
						writeError(w, http.StatusUnprocessableEntity, "guardrails violation: redaction unavailable")
						return
					}
					slog.WarnContext(r.Context(), "llm request redacted by guardrail model",
						"service_type", def.Type, "model", def.Model, "detector", rr.Name, "consumer", consumer, "violations", rr.Categories)
					metrics.GuardrailsModelDetectionsTotal.WithLabelValues(def.Type, def.Model, "input", rr.Name, "sync", "redacted").Inc()
					guardrails.MarkSpanFlagged(r.Context(), "input", "redact", []string{rr.Name})
				}
				raw = cleaned
			}
		}
		start := time.Now()
		consumer, userType := h.resolveConsumerAndType(r)
		sw := &statusWriter{ResponseWriter: w}
		h.llm.ServeJSON(sw, r, def, raw, consumer)
		metrics.RequestsTotal.WithLabelValues("llm", def.Type, def.Model, strconv.Itoa(sw.Status())).Inc()
		metrics.ObserveWithExemplar(r.Context(), metrics.RequestDuration.WithLabelValues("llm", def.Type, def.Model), time.Since(start).Seconds())
		if h.usageTracker != nil && consumer != "" && sw.Status() < 500 {
			h.usageTracker.TrackRequest(r.Context(), consumer, def.Type)
			h.usageTracker.TrackActive(r.Context(), consumer)
			h.usageTracker.TrackUserType(r.Context(), consumer, def.Type, userType)
		}
		return
	}

	h.proxyToInference(w, r, def, raw, r.Header.Get("Content-Type"))
}

// proxyToInference forwards the request body directly to the inference backend.
// When def.Retries > 0, the full backend cycle is repeated up to that many
// additional times (with 500ms exponential backoff) before giving up.
func (h *SyncHandler) proxyToInference(w http.ResponseWriter, r *http.Request, def *service.Def, body []byte, contentType string) {
	start := time.Now()
	defer func() {
		metrics.ObserveWithExemplar(r.Context(), metrics.RequestDuration.WithLabelValues("sync-direct", def.Type, def.Model), time.Since(start).Seconds())
	}()

	captureBody := h.processingLimiter != nil || (h.tokenLimiter != nil && !def.IsLLM())

	if h.tokenLimiter != nil && !def.IsLLM() {
		tr, err := h.tokenLimiter.CheckTokens(r.Context(), r, def.Type)
		if err != nil {
			slog.ErrorContext(r.Context(), "token limit check failed", "error", err)
			// fail open
		} else {
			if tr.Limit > 0 {
				w.Header().Set("X-TokenRateLimit-Limit", strconv.Itoa(tr.Limit))
				w.Header().Set("X-TokenRateLimit-Remaining", strconv.Itoa(tr.Remaining))
			}
			if !tr.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(tr.ResetAfter.Seconds())))
				metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "429").Inc()
				writeError(w, http.StatusTooManyRequests, "token rate limit exceeded")
				return
			}
		}
	}

	if len(def.Backends) == 0 {
		metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "500").Inc()
		writeError(w, http.StatusInternalServerError, "no backends configured")
		return
	}

	// Pool concurrency is acquired once per logical request (like the per-model
	// semaphore above), covering the whole retry loop below. The single defer
	// here correctly releases on every exit path (success, all-backends-failed,
	// or a cancelled retry wait).
	if def.PoolName != "" && h.poolSemaphore != nil {
		isPriority := h.priorityHeader != "" && r.Header.Get(h.priorityHeader) != ""
		acquired, usedReserved := h.poolSemaphore.TryAcquire(def.PoolName, isPriority)
		if !acquired {
			metrics.BackendPoolConcurrencyRejectedTotal.WithLabelValues(def.PoolName).Inc()
			metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "503").Inc()
			writeError(w, http.StatusServiceUnavailable, "backend pool too busy, retry later")
			return
		}
		defer h.poolSemaphore.Release(def.PoolName, usedReserved)
	}

	auth := r.Header.Get("Authorization")
	maxAttempts := 1 + def.Retries
	backoff := h.retryBackoff

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-r.Context().Done():
				metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "502").Inc()
				writeError(w, http.StatusBadGateway, "request cancelled during retry")
				return
			case <-time.After(backoff):
				backoff *= 2
			}
			slog.InfoContext(r.Context(), "retrying backend cycle",
				"attempt", attempt+1,
				"max_attempts", maxAttempts,
				"service", def.Type,
				"model", def.Model,
			)
		}

		var lastErr string
		for i, backend := range service.OrderedBackends(def.Backends) {
			if def.PoolName != "" && h.poolLimiter != nil {
				res, err := h.poolLimiter.CheckBackendPool(r.Context(), def.PoolName, backend.URL)
				if err != nil {
					slog.WarnContext(r.Context(), "backend pool rate limit check error", "pool", def.PoolName, "error", err)
				}
				if !res.Allowed {
					lastErr = "backend pool rate limit exceeded"
					continue
				}
			}
			target, err := url.Parse(backend.URL)
			if err != nil {
				slog.WarnContext(r.Context(), "invalid backend url, skipping",
					"backend_index", i, "url", backend.URL, "error", err)
				lastErr = "invalid backend url: " + err.Error()
				continue
			}
			target.Path = r.URL.Path
			target.RawQuery = r.URL.RawQuery

			upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
			if err != nil {
				lastErr = "failed to build upstream request: " + err.Error()
				continue
			}
			// Propagate W3C trace context so the inference service receives traceparent.
			otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(upstreamReq.Header))
			upstreamReq.Header.Set("Content-Type", contentType)
			if auth != "" {
				upstreamReq.Header.Set("Authorization", auth)
			}
			for k, v := range def.InferenceHeaders {
				upstreamReq.Header.Set(k, v)
			}
			for k, v := range backend.Headers {
				upstreamReq.Header.Set(k, v)
			}

			resp, err := h.httpClient.Do(upstreamReq)
			if err != nil {
				slog.WarnContext(r.Context(), "backend network error, trying next",
					"backend_index", i, "url", backend.URL, "error", err)
				metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "502").Inc()
				lastErr = "upstream error: " + err.Error()
				continue
			}

			if resp.StatusCode >= 500 {
				slog.WarnContext(r.Context(), "backend returned 5xx, trying next",
					"backend_index", i, "url", backend.URL, "status", resp.StatusCode)
				metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, strconv.Itoa(resp.StatusCode)).Inc()
				lastErr = fmt.Sprintf("backend returned %d", resp.StatusCode)
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				continue
			}

			// Success or 4xx: forward immediately, do not retry.
			defer resp.Body.Close()
			metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, strconv.Itoa(resp.StatusCode)).Inc()
			if h.usageTracker != nil && resp.StatusCode < 500 {
				consumer, userType := h.resolveConsumerAndType(r)
				if consumer != "" {
					h.usageTracker.TrackRequest(r.Context(), consumer, def.Type)
					h.usageTracker.TrackActive(r.Context(), consumer)
					h.usageTracker.TrackUserType(r.Context(), consumer, def.Type, userType)
				}
			}
			for key, values := range resp.Header {
				for _, v := range values {
					w.Header().Add(key, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			if captureBody && resp.StatusCode < 400 {
				respBody, _ := io.ReadAll(resp.Body)
				_, _ = w.Write(respBody)
				consumer, userType := h.resolveConsumerAndType(r)
				if h.processingLimiter != nil || h.usageTracker != nil {
					pt := extractProcessingTimeFromResponse(respBody)
					if pt == 0 {
						pt = time.Since(start).Seconds()
					}
					if consumer != "" {
						if h.processingLimiter != nil {
							if err := h.processingLimiter.AddProcessingTime(r.Context(), consumer, userType, def.Type, pt); err != nil {
								slog.ErrorContext(r.Context(), "failed to add processing time", "error", err)
							}
						}
						if h.usageTracker != nil {
							h.usageTracker.TrackProcessingTime(r.Context(), consumer, def.Type, pt)
						}
					}
				}
				if h.tokenLimiter != nil && !def.IsLLM() {
					prompt, tcompletion := extractTokensFromResponse(respBody)
					total := prompt + tcompletion
					if total > 0 {
						if err := h.tokenLimiter.AddTokens(r.Context(), r, def.Type, int(total)); err != nil {
							slog.ErrorContext(r.Context(), "failed to add tokens", "error", err)
						}
						if h.usageTracker != nil && consumer != "" {
							h.usageTracker.TrackTokens(r.Context(), consumer, def.Type, prompt, tcompletion)
						}
					}
				}
			} else {
				_, _ = io.Copy(w, resp.Body)
			}
			return
		}

		if attempt < maxAttempts-1 {
			slog.WarnContext(r.Context(), "all backends failed, will retry",
				"attempt", attempt+1,
				"error", lastErr,
				"service", def.Type,
				"model", def.Model,
			)
		} else {
			metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "502").Inc()
			writeError(w, http.StatusBadGateway, "all backends failed: "+lastErr)
		}
	}
}

// statusWriter wraps http.ResponseWriter to capture the written status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(status int) {
	sw.status = status
	sw.ResponseWriter.WriteHeader(status)
}

// Status returns the captured status code, defaulting to 200 if WriteHeader was never called.
func (sw *statusWriter) Status() int {
	if sw.status == 0 {
		return http.StatusOK
	}
	return sw.status
}

// extractProcessingTimeFromResponse parses the processing_time field from a
// JSON response body. Returns 0 if absent or not parseable.
func extractProcessingTimeFromResponse(body []byte) float64 {
	if len(body) == 0 {
		return 0
	}
	var v struct {
		ProcessingTime float64 `json:"processing_time"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return 0
	}
	return v.ProcessingTime
}

// extractTokensFromResponse parses OpenAI-compatible usage.prompt_tokens /
// usage.completion_tokens from a JSON response body. If both are absent but
// usage.total_tokens is present, the total is attributed to prompt tokens.
// Returns (0, 0) if absent or not parseable.
func extractTokensFromResponse(body []byte) (prompt, completion int64) {
	if len(body) == 0 {
		return 0, 0
	}
	var v struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return 0, 0
	}
	prompt = v.Usage.PromptTokens
	completion = v.Usage.CompletionTokens
	if prompt == 0 && completion == 0 && v.Usage.TotalTokens > 0 {
		prompt = v.Usage.TotalTokens
	}
	return prompt, completion
}

// resolveConsumerAndType reads consumer name and user type from request headers.
func (h *SyncHandler) resolveConsumerAndType(r *http.Request) (consumer, userType string) {
	if h.consumerHeader != "" {
		consumer = r.Header.Get(h.consumerHeader)
	}
	userType = "*"
	if h.userTypeHeader != "" {
		if v := r.Header.Get(h.userTypeHeader); v != "" {
			userType = v
		}
	}
	return
}

// reconstructMultipart rebuilds the multipart body from the already-parsed form,
// streaming file parts via an io.Pipe to avoid loading large files into memory.
func reconstructMultipart(r *http.Request) (io.ReadCloser, string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		err := func() error {
			for key, values := range r.MultipartForm.Value {
				for _, value := range values {
					if err := mw.WriteField(key, value); err != nil {
						return err
					}
				}
			}
			for fieldName, fileHeaders := range r.MultipartForm.File {
				for _, fh := range fileHeaders {
					part, err := mw.CreateFormFile(fieldName, fh.Filename)
					if err != nil {
						return err
					}
					f, err := fh.Open()
					if err != nil {
						return err
					}
					_, err = io.Copy(part, f)
					f.Close()
					if err != nil {
						return err
					}
				}
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	return pr, mw.FormDataContentType(), nil
}
