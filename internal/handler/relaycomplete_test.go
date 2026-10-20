package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/storage"
)

func newTestRedis(t *testing.T) (*storage.RedisClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := storage.NewRedis(config.RedisConfig{Addr: mr.Addr()}, config.LifecycleConfig{})
	if err != nil {
		t.Fatalf("failed to create redis client: %v", err)
	}
	return c, mr
}

func seedJob(t *testing.T, mr *miniredis.Miniredis, job *model.Job) {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	mr.Set("job:"+job.ID, string(data))
}

func newCompleteRequest(id string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/-/relay/jobs/"+id+"/complete", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

type stubS3 struct {
	getData  []byte
	getErr   error
	deleted  []string
	uploaded map[string][]byte
}

func (s *stubS3) GetObject(_ context.Context, _ string) ([]byte, error) {
	return s.getData, s.getErr
}

func (s *stubS3) DeleteObject(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	return nil
}

func (s *stubS3) Upload(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	if s.uploaded == nil {
		s.uploaded = map[string][]byte{}
	}
	b, _ := io.ReadAll(r)
	s.uploaded[key] = b
	return nil
}

type addProcessingTimeCall struct {
	consumer, userType, serviceType string
	seconds                         float64
}

type stubProcessingTimeLimiter struct {
	addCalls []addProcessingTimeCall
}

func (s *stubProcessingTimeLimiter) CheckProcessingTime(context.Context, *http.Request, string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}

func (s *stubProcessingTimeLimiter) AddProcessingTime(_ context.Context, consumer, userType, serviceType string, seconds float64) error {
	s.addCalls = append(s.addCalls, addProcessingTimeCall{consumer, userType, serviceType, seconds})
	return nil
}

type addTokensForCall struct {
	consumer, userType, serviceType string
	total                           int
}

type stubTokenLimiter struct {
	addForCalls []addTokensForCall
}

func (s *stubTokenLimiter) CheckTokens(context.Context, *http.Request, string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}
func (s *stubTokenLimiter) AddTokens(context.Context, *http.Request, string, int) error { return nil }
func (s *stubTokenLimiter) CheckModelTokens(context.Context, *http.Request, string, string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}
func (s *stubTokenLimiter) AddModelTokens(context.Context, *http.Request, string, string, int) error {
	return nil
}
func (s *stubTokenLimiter) AddTokensFor(_ context.Context, consumer, userType, serviceType string, total int) error {
	s.addForCalls = append(s.addForCalls, addTokensForCall{consumer, userType, serviceType, total})
	return nil
}

type trackProcessingTimeCall struct {
	consumer, serviceType string
	seconds               float64
}
type trackTokensCall struct {
	consumer, serviceType string
	prompt, completion    int64
}
type trackUserTypeCall struct {
	consumer, serviceType, userType string
}

type stubUsageTracker struct {
	trackProcessingTimeCalls []trackProcessingTimeCall
	trackTokensCalls         []trackTokensCall
	trackActiveCalls         []string
	trackUserTypeCalls       []trackUserTypeCall
}

func (s *stubUsageTracker) TrackRequest(context.Context, string, string) {}
func (s *stubUsageTracker) TrackJob(context.Context, string, string)     {}
func (s *stubUsageTracker) TrackProcessingTime(_ context.Context, consumer, serviceType string, seconds float64) {
	s.trackProcessingTimeCalls = append(s.trackProcessingTimeCalls, trackProcessingTimeCall{consumer, serviceType, seconds})
}
func (s *stubUsageTracker) TrackTokens(_ context.Context, consumer, serviceType string, prompt, completion int64) {
	s.trackTokensCalls = append(s.trackTokensCalls, trackTokensCall{consumer, serviceType, prompt, completion})
}
func (s *stubUsageTracker) TrackActive(_ context.Context, consumer string) {
	s.trackActiveCalls = append(s.trackActiveCalls, consumer)
}
func (s *stubUsageTracker) TrackUserType(_ context.Context, consumer, serviceType, userType string) {
	s.trackUserTypeCalls = append(s.trackUserTypeCalls, trackUserTypeCall{consumer, serviceType, userType})
}
func (s *stubUsageTracker) UpdateRetention(time.Duration) {}

func TestComplete_JobNotFound_Returns404(t *testing.T) {
	rc, _ := newTestRedis(t)
	h := NewRelayCompleteHandler(rc, &stubS3{}, false, config.WebhookConfig{})

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("missing"))

	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestComplete_DebitsAndTracksUsage(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-1", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted, ConsumerName: "acme", UserType: "user",
		ProcessingTime: 12.5, PromptTokens: 100, CompletionTokens: 50,
	}
	seedJob(t, mr, job)

	h := NewRelayCompleteHandler(rc, &stubS3{}, false, config.WebhookConfig{})
	ptLimiter := &stubProcessingTimeLimiter{}
	tokLimiter := &stubTokenLimiter{}
	tracker := &stubUsageTracker{}
	h.WithProcessingTimeLimiter(ptLimiter)
	h.WithTokenLimiter(tokLimiter)
	h.WithUsageTracker(tracker)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-1"))
	h.Wait() // debit/usage work runs in the claimed background goroutine

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if len(ptLimiter.addCalls) != 1 || ptLimiter.addCalls[0].seconds != 12.5 {
		t.Errorf("processing time debit: got %+v", ptLimiter.addCalls)
	}
	if len(tokLimiter.addForCalls) != 1 || tokLimiter.addForCalls[0].total != 150 {
		t.Errorf("token debit: got %+v", tokLimiter.addForCalls)
	}
	if len(tracker.trackProcessingTimeCalls) != 1 {
		t.Errorf("expected TrackProcessingTime call, got %+v", tracker.trackProcessingTimeCalls)
	}
	if len(tracker.trackTokensCalls) != 1 {
		t.Errorf("expected TrackTokens call, got %+v", tracker.trackTokensCalls)
	}
	if len(tracker.trackUserTypeCalls) != 1 || tracker.trackUserTypeCalls[0].userType != "user" {
		t.Errorf("expected TrackUserType call, got %+v", tracker.trackUserTypeCalls)
	}
}

func TestComplete_ZeroProcessingTimeAndTokens_NoDebitOrTrack(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-3", ServiceType: "transcription", Status: model.JobStatusCompleted,
		ConsumerName: "acme", UserType: "user",
	}
	seedJob(t, mr, job)

	h := NewRelayCompleteHandler(rc, &stubS3{}, false, config.WebhookConfig{})
	ptLimiter := &stubProcessingTimeLimiter{}
	tokLimiter := &stubTokenLimiter{}
	tracker := &stubUsageTracker{}
	h.WithProcessingTimeLimiter(ptLimiter)
	h.WithTokenLimiter(tokLimiter)
	h.WithUsageTracker(tracker)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-3"))
	h.Wait() // debit/usage work runs in the claimed background goroutine

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if len(ptLimiter.addCalls) != 0 {
		t.Errorf("expected no processing-time debit, got %+v", ptLimiter.addCalls)
	}
	if len(tokLimiter.addForCalls) != 0 {
		t.Errorf("expected no token debit, got %+v", tokLimiter.addForCalls)
	}
	// TrackUserType still fires unconditionally when a consumer is set.
	if len(tracker.trackUserTypeCalls) != 1 {
		t.Errorf("expected TrackUserType call, got %+v", tracker.trackUserTypeCalls)
	}
	if len(tracker.trackProcessingTimeCalls) != 0 || len(tracker.trackTokensCalls) != 0 {
		t.Errorf("expected no TrackProcessingTime/TrackTokens calls, got %+v / %+v", tracker.trackProcessingTimeCalls, tracker.trackTokensCalls)
	}
}

// asyncGuardrailRegistry builds a registry with one transcription model whose
// async (result) stage runs the given regex check groups with the given action
// ("" defaults to flag/shadow).
func asyncGuardrailRegistry(action string, checks []string) *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{{
		Type: "transcription", Model: "whisper-large-v3",
		Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		InferenceURL: "http://svc",
		Guardrails:   config.GuardrailsConfig{Async: &config.GuardrailsAsyncConfig{Action: action, Checks: checks}},
	}})
}

func TestComplete_AsyncGuardrail_ShadowFlagsPII(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-async", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted, ResultRef: "job-async/result.json",
	}
	seedJob(t, mr, job)

	// Result text carries an email → the pii regex group flags it (shadow).
	s3 := &stubS3{getData: []byte(`{"text":"contact me at alice@example.com"}`)}
	h := NewRelayCompleteHandler(rc, s3, false, config.WebhookConfig{})
	h.WithRegistry(asyncGuardrailRegistry("", []string{"pii"}))

	m := metrics.GuardrailsAsyncTotal.WithLabelValues("transcription", "whisper-large-v3", "regex", "flagged")
	before := testutil.ToFloat64(m)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-async"))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	h.Wait() // drain the scan goroutine

	if got := testutil.ToFloat64(m); got != before+1 {
		t.Errorf("async regex flag metric: got %v, want %v", got, before+1)
	}
}

func TestComplete_AsyncGuardrail_CleanResult_NoFlag(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-clean", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted, ResultRef: "job-clean/result.json",
	}
	seedJob(t, mr, job)

	s3 := &stubS3{getData: []byte(`{"text":"the weather is nice today"}`)}
	h := NewRelayCompleteHandler(rc, s3, false, config.WebhookConfig{})
	h.WithRegistry(asyncGuardrailRegistry("", []string{"pii"}))

	m := metrics.GuardrailsAsyncTotal.WithLabelValues("transcription", "whisper-large-v3", "regex", "flagged")
	before := testutil.ToFloat64(m)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-clean"))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	h.Wait()

	if got := testutil.ToFloat64(m); got != before {
		t.Errorf("clean result must not flag: metric moved %v → %v", before, got)
	}
}

func TestComplete_AsyncGuardrail_BlockFailsJobAndClearsResult(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-block", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted, ResultRef: "job-block/result.json",
	}
	seedJob(t, mr, job)

	s3 := &stubS3{getData: []byte(`{"text":"contact alice@example.com"}`)}
	h := NewRelayCompleteHandler(rc, s3, false, config.WebhookConfig{})
	h.WithRegistry(asyncGuardrailRegistry("block", []string{"pii"}))

	m := metrics.GuardrailsAsyncTotal.WithLabelValues("transcription", "whisper-large-v3", "regex", "blocked")
	before := testutil.ToFloat64(m)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-block"))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	h.Wait()

	if got := testutil.ToFloat64(m); got != before+1 {
		t.Errorf("blocked metric: got %v, want %v", got, before+1)
	}
	updated, err := rc.GetJob(context.Background(), "job-block")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.Status != model.JobStatusFailed {
		t.Errorf("status: got %q, want failed", updated.Status)
	}
	if updated.ResultRef != "" {
		t.Errorf("result_ref should be cleared, got %q", updated.ResultRef)
	}
	if updated.Error == "" {
		t.Error("expected a violation reason on the failed job")
	}
}

func TestComplete_AsyncGuardrail_RedactsAndRepoints(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-redact", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted, ResultRef: "job-redact/result.json",
	}
	seedJob(t, mr, job)

	s3 := &stubS3{getData: []byte(`{"text":"reach me at alice@example.com"}`)}
	h := NewRelayCompleteHandler(rc, s3, false, config.WebhookConfig{})
	h.WithRegistry(asyncGuardrailRegistry("redact", []string{"pii"}))

	m := metrics.GuardrailsAsyncTotal.WithLabelValues("transcription", "whisper-large-v3", "regex", "redacted")
	before := testutil.ToFloat64(m)

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-redact"))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	h.Wait()

	if got := testutil.ToFloat64(m); got != before+1 {
		t.Errorf("redacted metric: got %v, want %v", got, before+1)
	}
	sibling := "job-redact/result.json.redacted"
	red, ok := s3.uploaded[sibling]
	if !ok {
		t.Fatalf("expected redacted sibling %q to be uploaded (uploaded %d objects)", sibling, len(s3.uploaded))
	}
	if strings.Contains(string(red), "alice@example.com") {
		t.Errorf("email should be redacted in sibling, got %s", red)
	}
	updated, err := rc.GetJob(context.Background(), "job-redact")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.ResultRef != sibling {
		t.Errorf("result_ref: got %q, want %q", updated.ResultRef, sibling)
	}
	if updated.Status != model.JobStatusCompleted {
		t.Errorf("status should remain completed, got %q", updated.Status)
	}
}

func TestComplete_AsyncGuardrail_NoRegistry_NoScan(t *testing.T) {
	// Without WithRegistry the completion path must still succeed and not scan.
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-noreg", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted, ResultRef: "job-noreg/result.json",
	}
	seedJob(t, mr, job)

	s3 := &stubS3{getData: []byte(`{"text":"alice@example.com"}`)}
	h := NewRelayCompleteHandler(rc, s3, false, config.WebhookConfig{})

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-noreg"))
	h.Wait()

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
}

func TestComplete_DispatchesWebhookAndDrainsViaWait(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-2", ServiceType: "transcription", Status: model.JobStatusCompleted,
		CallbackURL: srv.URL, ResultRef: "job-2/result.json",
	}
	seedJob(t, mr, job)

	h := NewRelayCompleteHandler(rc, &stubS3{getData: []byte(`{"text":"hi"}`)}, false, config.WebhookConfig{})

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-2"))
	h.Wait()

	select {
	case <-received:
	default:
		t.Error("expected webhook to be delivered before Wait() returned")
	}
}

// TestProcessCompletion_ExactlyOnce verifies the Redis claim dedupes the two
// triggers (pub/sub broadcast + /complete fast-path): the completion work — a
// webhook here — runs exactly once no matter how many times ProcessCompletion
// is invoked for the same job.
func TestProcessCompletion_ExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	deliveries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deliveries++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-once", ServiceType: "transcription", Status: model.JobStatusCompleted,
		CallbackURL: srv.URL, ResultRef: "job-once/result.json",
	}
	seedJob(t, mr, job)

	h := NewRelayCompleteHandler(rc, &stubS3{getData: []byte(`{"text":"hi"}`)}, false, config.WebhookConfig{})

	// Fire the completion trigger three times (as pub/sub on N replicas plus the
	// HTTP fast-path would); the claim must let only the first through.
	for i := 0; i < 3; i++ {
		h.ProcessCompletion(context.Background(), "job-once")
	}
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if deliveries != 1 {
		t.Fatalf("expected exactly one webhook delivery, got %d", deliveries)
	}
}

// TestProcessCompletion_UnknownJob is a no-op (job never existed) and must not
// panic or deliver anything.
func TestProcessCompletion_UnknownJob(t *testing.T) {
	rc, _ := newTestRedis(t)
	h := NewRelayCompleteHandler(rc, &stubS3{}, false, config.WebhookConfig{})
	h.ProcessCompletion(context.Background(), "nope")
	h.Wait()
}

// TestProcessCompletion_AfterWait_NoOp verifies that once Wait has latched
// shutdown, a late completion trigger (e.g. a pub/sub broadcast racing the drain)
// does not start new work or deliver — closing the wg.Add-vs-Wait window.
func TestProcessCompletion_AfterWait_NoOp(t *testing.T) {
	var delivered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-late", ServiceType: "transcription", Status: model.JobStatusCompleted,
		CallbackURL: srv.URL, ResultRef: "job-late/result.json",
	}
	seedJob(t, mr, job)

	h := NewRelayCompleteHandler(rc, &stubS3{getData: []byte(`{"text":"hi"}`)}, false, config.WebhookConfig{})
	h.Wait() // shutdown latched before any trigger

	h.ProcessCompletion(context.Background(), "job-late")
	h.Wait() // drains nothing

	if delivered != 0 {
		t.Fatalf("no work should run after shutdown, got %d deliveries", delivered)
	}
}

// TestComplete_AsyncGuardrail_ClearsScanGate verifies the completion scan clears
// the armed DONE-gate (so the poll path can deliver) and applies the block.
func TestComplete_AsyncGuardrail_ClearsScanGate(t *testing.T) {
	rc, mr := newTestRedis(t)
	job := &model.Job{
		ID: "job-gate", ServiceType: "transcription", Model: "whisper-large-v3",
		Status: model.JobStatusCompleted, ResultRef: "job-gate/result.json",
	}
	seedJob(t, mr, job)
	// Arm the gate as Submit would for an enforcing (block) async model.
	if err := rc.SetScanGate(context.Background(), "job-gate", time.Minute, false); err != nil {
		t.Fatalf("arm gate: %v", err)
	}

	s3 := &stubS3{getData: []byte(`{"text":"contact alice@example.com"}`)}
	h := NewRelayCompleteHandler(rc, s3, false, config.WebhookConfig{})
	h.WithRegistry(asyncGuardrailRegistry("block", []string{"pii"}))

	w := httptest.NewRecorder()
	h.Complete(w, newCompleteRequest("job-gate"))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	h.Wait()

	if _, ok, err := rc.GetScanGate(context.Background(), "job-gate"); err != nil || ok {
		t.Errorf("scan gate should be cleared after the scan (ok=%v err=%v)", ok, err)
	}
	updated, err := rc.GetJob(context.Background(), "job-gate")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.Status != model.JobStatusFailed {
		t.Errorf("blocked job should be failed, got %q", updated.Status)
	}
}
