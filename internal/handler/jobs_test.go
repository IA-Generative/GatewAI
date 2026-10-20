package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/authz"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/model"
	"gatewai/gateway/internal/ratelimit"
	"gatewai/gateway/internal/service"
	"gatewai/gateway/internal/storage"
)

// ── Mocks ────────────────────────────────────────────────────────────────────

type mockJobS3 struct {
	uploadErr   error
	uploaded    bool
	mu          sync.Mutex
	deletedKeys []string
}

func (m *mockJobS3) Upload(_ context.Context, _ string, _ io.Reader, _ int64, _ string) error {
	m.uploaded = true
	return m.uploadErr
}
func (m *mockJobS3) GetObject(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (m *mockJobS3) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	m.deletedKeys = append(m.deletedKeys, key)
	m.mu.Unlock()
	return nil
}

type mockAsyncStore struct {
	saveErr         error
	saved           bool
	updateCalled    bool
	deleteJobCalled bool
	job             *model.Job // returned by GetJob
	getJobErr       error
	jobs            []*model.Job // returned by ListJobsByConsumer
	jobsTotal       int64
	queuePos        int64
	queuePosFound   bool
	staleJobs       []*model.Job // returned by ListStalePendingJobs
	staleJobsErr    error
	idem            map[string]string // idempotency key → jobID (SETNX semantics)
	everExisted     bool              // returned by JobEverExisted
	scanGate        *storage.ScanGate // non-nil = an armed result DONE-gate
	scanGateSet     bool              // SetScanGate was called
	scanGateCleared bool              // ClearScanGate was called
	overrideStatus  model.JobStatus   // last OverrideJobResult status
	overrideCalled  bool              // OverrideJobResult was called
}

func (m *mockAsyncStore) SetScanGate(_ context.Context, _ string, timeout time.Duration, failClosed bool) error {
	m.scanGate = &storage.ScanGate{Timeout: timeout, FailClosed: failClosed}
	m.scanGateSet = true
	return nil
}

func (m *mockAsyncStore) GetScanGate(_ context.Context, _ string) (storage.ScanGate, bool, error) {
	if m.scanGate == nil {
		return storage.ScanGate{}, false, nil
	}
	return *m.scanGate, true, nil
}

func (m *mockAsyncStore) ClearScanGate(_ context.Context, _ string) error {
	m.scanGate = nil
	m.scanGateCleared = true
	return nil
}

func (m *mockAsyncStore) OverrideJobResult(_ context.Context, _ string, status model.JobStatus, _, errMsg string) error {
	m.overrideCalled = true
	m.overrideStatus = status
	if m.job != nil {
		m.job.Status = status
		m.job.Error = errMsg
	}
	return nil
}

func (m *mockAsyncStore) SaveJob(_ context.Context, _ *model.Job) error {
	m.saved = true
	return m.saveErr
}
func (m *mockAsyncStore) GetJob(_ context.Context, _ string) (*model.Job, error) {
	return m.job, m.getJobErr
}
func (m *mockAsyncStore) DeleteJob(_ context.Context, _ string) error {
	m.deleteJobCalled = true
	return nil
}
func (m *mockAsyncStore) UpdateJobResult(_ context.Context, _ string, _ model.JobStatus, _, _ string) error {
	m.updateCalled = true
	return nil
}
func (m *mockAsyncStore) MarkJobCancelled(_ context.Context, _, _ string) error {
	return nil
}
func (m *mockAsyncStore) ListJobsByConsumer(_ context.Context, _ string, _, _ int64) ([]*model.Job, int64, error) {
	return m.jobs, m.jobsTotal, nil
}
func (m *mockAsyncStore) GetQueuePosition(_ context.Context, _, _ string) (int64, bool, error) {
	return m.queuePos, m.queuePosFound, nil
}
func (m *mockAsyncStore) ListStalePendingJobs(_ context.Context, _ time.Duration) ([]*model.Job, error) {
	return m.staleJobs, m.staleJobsErr
}
func (m *mockAsyncStore) ReserveIdempotencyKey(_ context.Context, consumer, key, jobID string, _ time.Duration) (bool, error) {
	if m.idem == nil {
		m.idem = map[string]string{}
	}
	k := consumer + ":" + key
	if _, exists := m.idem[k]; exists {
		return false, nil
	}
	m.idem[k] = jobID
	return true, nil
}
func (m *mockAsyncStore) GetIdempotencyKey(_ context.Context, consumer, key string) (string, error) {
	return m.idem[consumer+":"+key], nil
}
func (m *mockAsyncStore) ReleaseIdempotencyKey(_ context.Context, consumer, key string) error {
	delete(m.idem, consumer+":"+key)
	return nil
}
func (m *mockAsyncStore) JobEverExisted(_ context.Context, _ string) (bool, error) {
	return m.everExisted, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// multiOpRegistry builds a registry with one model that has two operations
// (transcription + translation), reproducing the exact bug scenario.
func multiOpRegistry() *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{{
		Type:  "transcription",
		Model: "faster-whisper",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
			"translation":   {"/v1/audio/translations"},
		},
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
	}})
}

// singleOpRegistry builds a registry with one model and one operation.
func singleOpRegistry() *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{{
		Type:  "transcription",
		Model: "faster-whisper",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
	}})
}

// submitReq builds a multipart POST /jobs/{serviceType} request and injects
// the chi route context so chi.URLParam works outside a real router.
func submitReq(t *testing.T, serviceType, modelName, operation, filename string, body []byte) *http.Request {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if modelName != "" {
		_ = mw.WriteField("model", modelName)
	}
	if operation != "" {
		_ = mw.WriteField("operation", operation)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(body)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/jobs/"+serviceType, buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("service_type", serviceType)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func newAsyncHandler(reg *service.Registry, s3 *mockJobS3, store *mockAsyncStore) *handler.JobHandler {
	return handler.NewJobHandler(reg, s3, store, "", "", nil, config.LifecycleConfig{})
}

// asyncInputGuardrailRegistry adds a blocking input-stage regex guardrail (pii)
// to the single transcription model, for testing async-submit param scanning.
func asyncInputGuardrailRegistry() *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{{
		Type:  "transcription",
		Model: "faster-whisper",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
		Guardrails:    config.GuardrailsConfig{Action: "block", Checks: []string{"pii"}},
	}})
}

// submitReqWithParam builds a submit request that also carries one extra text
// form field (e.g. a prompt) alongside the file.
func submitReqWithParam(t *testing.T, serviceType, modelName, paramKey, paramVal, filename string, body []byte) *http.Request {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if modelName != "" {
		_ = mw.WriteField("model", modelName)
	}
	_ = mw.WriteField(paramKey, paramVal)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(body)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/jobs/"+serviceType, buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("service_type", serviceType)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestSubmit_InvalidOperation_NoSideEffects is the regression test for the bug
// where S3 and Redis were written before the operation was validated.
func TestSubmit_InvalidOperation_NoSideEffects(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "transcription", "faster-whisper", "translations", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(multiOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when operation is invalid")
	}
	if store.saved {
		t.Error("Redis save must not be called when operation is invalid")
	}
}

// TestSubmit_MultipleOperations_MissingField verifies that omitting the operation
// field when a model has multiple operations returns 400 without side effects.
func TestSubmit_MultipleOperations_MissingField(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(multiOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when operation field is missing")
	}
	if store.saved {
		t.Error("Redis save must not be called when operation field is missing")
	}
}

// TestSubmit_NominalPath verifies the full success path: S3 → Redis → 202.
func TestSubmit_NominalPath(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if !s3.uploaded {
		t.Error("S3 upload should have been called")
	}
	if !store.saved {
		t.Error("Redis save should have been called")
	}
}

// TestSubmit_AsyncInputGuardrail_BlocksPIIParam verifies that a text param with
// PII is rejected (422) by the input stage before any S3/Redis side effects.
func TestSubmit_AsyncInputGuardrail_BlocksPIIParam(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReqWithParam(t, "transcription", "faster-whisper", "prompt", "transcribe for alice@example.com", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(asyncInputGuardrailRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("blocked submit must not upload to S3")
	}
	if store.saved {
		t.Error("blocked submit must not save the job")
	}
}

// TestSubmit_AsyncInputGuardrail_AllowsCleanParam verifies a clean text param
// passes the input stage and the job is accepted.
func TestSubmit_AsyncInputGuardrail_AllowsCleanParam(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReqWithParam(t, "transcription", "faster-whisper", "prompt", "please transcribe the meeting", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(asyncInputGuardrailRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if !s3.uploaded || !store.saved {
		t.Error("clean submit should upload and save")
	}
}

// TestSubmit_IdempotencyKey_ReplaysExistingJob verifies that a second submission
// carrying the same Idempotency-Key returns the original job (200 + replay
// header) without saving a duplicate — i.e. no second inference is enqueued.
func TestSubmit_IdempotencyKey_ReplaysExistingJob(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{job: &model.Job{ID: "orig-123", ServiceType: "transcription", Model: "faster-whisper", Status: model.JobStatusPending}}
	reg := singleOpRegistry()

	req1 := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	req1.Header.Set("Idempotency-Key", "abc-key")
	w1 := httptest.NewRecorder()
	newAsyncHandler(reg, s3, store).Submit(w1, req1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first submit: expected 202, got %d: %s", w1.Code, w1.Body.String())
	}
	if !store.saved {
		t.Fatal("first submit should have saved the job")
	}

	store.saved = false // reset to detect a duplicate save
	req2 := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	req2.Header.Set("Idempotency-Key", "abc-key")
	w2 := httptest.NewRecorder()
	newAsyncHandler(reg, s3, store).Submit(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("replay: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("X-Idempotent-Replay") != "true" {
		t.Error("replay: expected X-Idempotent-Replay: true")
	}
	if store.saved {
		t.Error("replay must NOT save a second job (no duplicate inference)")
	}
}

// TestSubmit_SingleOperation_AutoSelect verifies that omitting the operation
// field auto-selects the only configured operation.
func TestSubmit_SingleOperation_AutoSelect(t *testing.T) {
	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, &mockAsyncStore{}).Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 when operation auto-selected, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSubmit_UnknownServiceType verifies that an unknown service type returns
// 404 before any side effects.
func TestSubmit_UnknownServiceType(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "unknown-type", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called for unknown service type")
	}
	if store.saved {
		t.Error("Redis save must not be called for unknown service type")
	}
}

// TestSubmit_MissingFile verifies that a missing file field returns 400.
func TestSubmit_MissingFile(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("model", "faster-whisper")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/jobs/transcription", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("service_type", "transcription")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing file, got %d", w.Code)
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when file is missing")
	}
}

// TestSubmit_InvalidExtension verifies that a rejected file extension returns
// 400 before any side effects.
func TestSubmit_InvalidExtension(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "transcription", "faster-whisper", "", "document.pdf", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid extension, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called for an invalid file extension")
	}
	if store.saved {
		t.Error("Redis save must not be called for an invalid file extension")
	}
}

// TestSubmit_S3Failure verifies that an S3 upload error returns 500 without
// writing to Redis.
func TestSubmit_S3Failure(t *testing.T) {
	s3 := &mockJobS3{uploadErr: fmt.Errorf("s3 unavailable")}
	store := &mockAsyncStore{}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on S3 failure, got %d", w.Code)
	}
	if store.saved {
		t.Error("Redis save must not be called after S3 failure")
	}
}

// statusReq builds a GET /jobs/{serviceType}/{id} request with chi route context.
func statusReq(t *testing.T, serviceType, id string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/jobs/"+serviceType+"/"+id, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("service_type", serviceType)
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// listReq builds a GET /jobs request with the given consumer header.
func listReq(t *testing.T, consumer string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	req.Header.Set("X-Consumer-Username", consumer)
	return req
}

func newHandlerWithConsumer(reg *service.Registry, s3 *mockJobS3, store *mockAsyncStore) *handler.JobHandler {
	return handler.NewJobHandler(reg, s3, store, "", "X-Consumer-Username", nil, config.LifecycleConfig{})
}

// ── GetStatus tests ───────────────────────────────────────────────────────────

// TestGetStatus_Pending_HasQueuePosition verifies that a pending job response
// includes queue_position when the store returns one.
func TestGetStatus_Pending_HasQueuePosition(t *testing.T) {
	now := time.Now().UTC()
	store := &mockAsyncStore{
		job: &model.Job{
			ID:          "abc",
			ServiceType: "transcription",
			Model:       "faster-whisper",
			Status:      model.JobStatusPending,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		queuePos:      3,
		queuePosFound: true,
	}

	w := httptest.NewRecorder()
	newHandlerWithConsumer(singleOpRegistry(), &mockJobS3{}, store).
		GetStatus(w, statusReq(t, "transcription", "abc"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	pos, ok := resp["queue_position"]
	if !ok {
		t.Fatal("queue_position missing from pending job response")
	}
	if pos.(float64) != 3 {
		t.Errorf("expected queue_position=3, got %v", pos)
	}
}

// TestGetStatus_Completed_NoQueuePosition verifies that a completed job response
// does not include queue_position.
func TestGetStatus_Completed_NoQueuePosition(t *testing.T) {
	now := time.Now().UTC()
	store := &mockAsyncStore{
		job: &model.Job{
			ID:          "abc",
			ServiceType: "transcription",
			Model:       "faster-whisper",
			Status:      model.JobStatusCompleted,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}

	w := httptest.NewRecorder()
	newHandlerWithConsumer(singleOpRegistry(), &mockJobS3{}, store).
		GetStatus(w, statusReq(t, "transcription", "abc"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := resp["queue_position"]; ok {
		t.Error("queue_position must not be present for a completed job")
	}
}

// TestGetStatus_NotFound verifies that an unknown job ID returns 404.
func TestGetStatus_NotFound(t *testing.T) {
	store := &mockAsyncStore{getJobErr: fmt.Errorf("job not found")}

	w := httptest.NewRecorder()
	newHandlerWithConsumer(singleOpRegistry(), &mockJobS3{}, store).
		GetStatus(w, statusReq(t, "transcription", "unknown"))

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// TestGetStatus_Expired_Returns410 verifies that a job whose record is gone but
// that provably existed (tombstone present) returns 410 with status "expired",
// distinguishing it from a never-existed 404.
func TestGetStatus_Expired_Returns410(t *testing.T) {
	store := &mockAsyncStore{getJobErr: fmt.Errorf("job not found"), everExisted: true}

	w := httptest.NewRecorder()
	newHandlerWithConsumer(singleOpRegistry(), &mockJobS3{}, store).
		GetStatus(w, statusReq(t, "transcription", "abc"))

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if body["status"] != "expired" {
		t.Errorf("expected status \"expired\", got %q", body["status"])
	}
}

// ── ListJobs tests ────────────────────────────────────────────────────────────

// TestListJobs_PendingJobsHaveQueuePosition verifies that pending jobs in the
// list response carry queue_position when pre-populated by the store.
func TestListJobs_PendingJobsHaveQueuePosition(t *testing.T) {
	now := time.Now().UTC()
	pos := int64(2)
	store := &mockAsyncStore{
		jobs: []*model.Job{
			{
				ID: "j1", ServiceType: "transcription", Model: "faster-whisper",
				Status: model.JobStatusPending, QueuePosition: &pos,
				CreatedAt: now, UpdatedAt: now,
			},
			{
				ID: "j2", ServiceType: "transcription", Model: "faster-whisper",
				Status:    model.JobStatusCompleted,
				CreatedAt: now, UpdatedAt: now,
			},
		},
		jobsTotal: 2,
	}

	w := httptest.NewRecorder()
	newHandlerWithConsumer(singleOpRegistry(), &mockJobS3{}, store).
		ListJobs(w, listReq(t, "consumer-a"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(resp.Jobs))
	}

	// Pending job must have queue_position.
	if _, ok := resp.Jobs[0]["queue_position"]; !ok {
		t.Error("queue_position missing from pending job in list")
	}
	if resp.Jobs[0]["queue_position"].(float64) != 2 {
		t.Errorf("expected queue_position=2, got %v", resp.Jobs[0]["queue_position"])
	}

	// Completed job must NOT have queue_position.
	if _, ok := resp.Jobs[1]["queue_position"]; ok {
		t.Error("queue_position must not be present for a completed job in list")
	}
}

// TestListJobs_MissingConsumerHeader verifies that a missing consumer header
// returns 400.
func TestListJobs_MissingConsumerHeader(t *testing.T) {
	store := &mockAsyncStore{}
	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)

	w := httptest.NewRecorder()
	newHandlerWithConsumer(singleOpRegistry(), &mockJobS3{}, store).
		ListJobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ── Cancel tests ──────────────────────────────────────────────────────────────

// cancelReq builds a DELETE /jobs/{serviceType}/{id} request with chi route context.
func cancelReq(t *testing.T, serviceType, id string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/jobs/"+serviceType+"/"+id, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("service_type", serviceType)
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestCancel_Pending_Success verifies that cancelling a pending job returns 202,
// marks the job as cancelled in Redis (keeping the record for GC), and does NOT
// immediately delete the S3 file (GC handles cleanup).
func TestCancel_Pending_Success(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{
		job: &model.Job{
			ID:          "job-1",
			ServiceType: "transcription",
			Model:       "faster-whisper",
			Status:      model.JobStatusPending,
			InputRef:    "inputs/job-1.wav",
		},
	}

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store).
		Cancel(w, cancelReq(t, "transcription", "job-1"))

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if store.deleteJobCalled {
		t.Error("DeleteJob must NOT be called — job record is kept for GC")
	}
}

// TestCancel_Processing_Returns202 verifies that a processing job is signalled for
// cancellation and returns 202 Accepted (relay stops inference asynchronously).
func TestCancel_Processing_Returns202(t *testing.T) {
	store := &mockAsyncStore{
		job: &model.Job{
			ID:          "job-2",
			ServiceType: "transcription",
			Model:       "faster-whisper",
			Status:      model.JobStatusProcessing,
		},
	}

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, store).
		Cancel(w, cancelReq(t, "transcription", "job-2"))

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 for processing job, got %d: %s", w.Code, w.Body.String())
	}
	if store.deleteJobCalled {
		t.Error("DeleteJob must not be called when job is in processing state")
	}
}

// TestCancel_Completed_Returns409 verifies that a completed job cannot be cancelled.
func TestCancel_Completed_Returns409(t *testing.T) {
	store := &mockAsyncStore{
		job: &model.Job{
			ID:          "job-3",
			ServiceType: "transcription",
			Model:       "faster-whisper",
			Status:      model.JobStatusCompleted,
		},
	}

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, store).
		Cancel(w, cancelReq(t, "transcription", "job-3"))

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for completed job, got %d: %s", w.Code, w.Body.String())
	}
	if store.deleteJobCalled {
		t.Error("DeleteJob must not be called when job is already completed")
	}
}

// TestCancel_Failed_Returns409 verifies that a failed job cannot be cancelled.
func TestCancel_Failed_Returns409(t *testing.T) {
	store := &mockAsyncStore{
		job: &model.Job{
			ID:          "job-4",
			ServiceType: "transcription",
			Model:       "faster-whisper",
			Status:      model.JobStatusFailed,
		},
	}

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, store).
		Cancel(w, cancelReq(t, "transcription", "job-4"))

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for failed job, got %d: %s", w.Code, w.Body.String())
	}
	if store.deleteJobCalled {
		t.Error("DeleteJob must not be called when job is already failed")
	}
}

// TestCancel_NotFound_Returns404 verifies that cancelling a missing job returns 404.
func TestCancel_NotFound_Returns404(t *testing.T) {
	store := &mockAsyncStore{getJobErr: fmt.Errorf("not found")}

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, store).
		Cancel(w, cancelReq(t, "transcription", "missing-id"))

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing job, got %d", w.Code)
	}
}

// ── AdminPurge tests ──────────────────────────────────────────────────────────

// purgeReq builds a POST /-/jobs/purge request with the given query params.
func purgeReq(t *testing.T, olderThan, limit string) *http.Request {
	t.Helper()
	url := "/-/jobs/purge"
	sep := "?"
	if olderThan != "" {
		url += sep + "older_than=" + olderThan
		sep = "&"
	}
	if limit != "" {
		url += sep + "limit=" + limit
	}
	return httptest.NewRequest(http.MethodPost, url, nil)
}

// TestAdminPurge_MissingOlderThan_Returns400 verifies that omitting older_than returns 400.
func TestAdminPurge_MissingOlderThan_Returns400(t *testing.T) {
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, &mockAsyncStore{}).
		AdminPurge(w, purgeReq(t, "", ""))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing older_than, got %d", w.Code)
	}
}

// TestAdminPurge_InvalidDuration_Returns400 verifies that a malformed duration returns 400.
func TestAdminPurge_InvalidDuration_Returns400(t *testing.T) {
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, &mockAsyncStore{}).
		AdminPurge(w, purgeReq(t, "not-a-duration", ""))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid duration, got %d", w.Code)
	}
}

// TestAdminPurge_InvalidLimit_Returns400 verifies that a non-integer limit returns 400.
func TestAdminPurge_InvalidLimit_Returns400(t *testing.T) {
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, &mockAsyncStore{}).
		AdminPurge(w, purgeReq(t, "2h", "abc"))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid limit, got %d", w.Code)
	}
}

// TestAdminPurge_Empty_NoJobs verifies that an empty stale-job list returns 200
// with purged=0 and truncated=false.
func TestAdminPurge_Empty_NoJobs(t *testing.T) {
	store := &mockAsyncStore{staleJobs: nil}
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, store).
		AdminPurge(w, purgeReq(t, "2h", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["purged"].(float64) != 0 {
		t.Errorf("expected purged=0, got %v", resp["purged"])
	}
	if resp["truncated"].(bool) {
		t.Error("expected truncated=false when no jobs found")
	}
}

// TestAdminPurge_WithJobs_PurgesAndCleansS3 verifies that stale jobs are deleted
// from Redis and their S3 input files are removed asynchronously.
func TestAdminPurge_WithJobs_PurgesAndCleansS3(t *testing.T) {
	s3 := &mockJobS3{}
	now := time.Now().UTC()
	store := &mockAsyncStore{
		staleJobs: []*model.Job{
			{ID: "s1", Model: "faster-whisper", InputRef: "inputs/s1.wav", CreatedAt: now, UpdatedAt: now},
			{ID: "s2", Model: "faster-whisper", InputRef: "inputs/s2.wav", CreatedAt: now, UpdatedAt: now},
		},
	}

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store).
		AdminPurge(w, purgeReq(t, "2h", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["purged"].(float64) != 2 {
		t.Errorf("expected purged=2, got %v", resp["purged"])
	}
	if resp["truncated"].(bool) {
		t.Error("expected truncated=false")
	}
	// S3 deletes run in goroutines — give them a moment.
	time.Sleep(30 * time.Millisecond)
	s3.mu.Lock()
	deletedCount := len(s3.deletedKeys)
	s3.mu.Unlock()
	if deletedCount != 2 {
		t.Errorf("expected 2 S3 deletes, got %d", deletedCount)
	}
}

// TestAdminPurge_Truncated_LimitRespected verifies that when stale jobs exceed
// the limit, only limit jobs are purged and truncated=true is returned.
func TestAdminPurge_Truncated_LimitRespected(t *testing.T) {
	now := time.Now().UTC()
	store := &mockAsyncStore{
		staleJobs: []*model.Job{
			{ID: "t1", Model: "faster-whisper", CreatedAt: now, UpdatedAt: now},
			{ID: "t2", Model: "faster-whisper", CreatedAt: now, UpdatedAt: now},
			{ID: "t3", Model: "faster-whisper", CreatedAt: now, UpdatedAt: now},
		},
	}

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, store).
		AdminPurge(w, purgeReq(t, "1h", "2"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["purged"].(float64) != 2 {
		t.Errorf("expected purged=2 (limit), got %v", resp["purged"])
	}
	if !resp["truncated"].(bool) {
		t.Error("expected truncated=true when result exceeds limit")
	}
}

// TestSubmit_RedisSaveFailure verifies that a Redis save error returns 500
// without returning a successful response to the client.
func TestSubmit_RedisSaveFailure(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{saveErr: fmt.Errorf("redis unavailable")}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on Redis save failure, got %d", w.Code)
	}
	if !s3.uploaded {
		t.Error("S3 upload should have been called before Redis save")
	}
}

// mockConcurrentLimiter is a test double for ratelimit.ConcurrentChecker.
type mockConcurrentLimiter struct {
	result       ratelimit.CheckResult
	checkErr     error
	releaseCalls int
}

func (m *mockConcurrentLimiter) CheckConcurrent(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return m.result, m.checkErr
}

func (m *mockConcurrentLimiter) ReleaseSlot(_ context.Context, _ *http.Request, _ string) error {
	m.releaseCalls++
	return nil
}

// TestSubmit_ConcurrentLimit_Denied verifies that a 429 is returned when the
// concurrent job limit is reached.
func TestSubmit_ConcurrentLimit_Denied(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	limiter := &mockConcurrentLimiter{
		result: ratelimit.CheckResult{Allowed: false, Limit: 5, Remaining: 0},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithConcurrentLimiter(limiter, "X-User-Type").
		Submit(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 when concurrent limit exceeded, got %d", w.Code)
	}
	if store.saved {
		t.Error("Redis SaveJob must not be called when concurrent limit is exceeded")
	}
	if s3.uploaded {
		t.Error("S3 upload must not happen when concurrent limit is exceeded")
	}
}

// mockProcessingTimeLimiter is a test double for ratelimit.ProcessingTimeChecker.
type mockProcessingTimeLimiter struct {
	result   ratelimit.CheckResult
	checkErr error
}

func (m *mockProcessingTimeLimiter) CheckProcessingTime(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return m.result, m.checkErr
}

func (m *mockProcessingTimeLimiter) AddProcessingTime(_ context.Context, _, _, _ string, _ float64) error {
	return nil
}

// mockTokenLimiter is a test double for ratelimit.TokenChecker, shared by
// jobs_test.go and sync_test.go (both package handler_test).
type mockTokenLimiter struct {
	result   ratelimit.CheckResult
	checkErr error
	addCalls []struct {
		serviceType string
		total       int
	}
}

func (m *mockTokenLimiter) CheckTokens(_ context.Context, _ *http.Request, _ string) (ratelimit.CheckResult, error) {
	return m.result, m.checkErr
}

func (m *mockTokenLimiter) AddTokens(_ context.Context, _ *http.Request, serviceType string, total int) error {
	m.addCalls = append(m.addCalls, struct {
		serviceType string
		total       int
	}{serviceType, total})
	return nil
}

func (m *mockTokenLimiter) CheckModelTokens(_ context.Context, _ *http.Request, _, _ string) (ratelimit.CheckResult, error) {
	return ratelimit.CheckResult{Allowed: true}, nil
}

func (m *mockTokenLimiter) AddModelTokens(_ context.Context, _ *http.Request, _, _ string, _ int) error {
	return nil
}

func (m *mockTokenLimiter) AddTokensFor(_ context.Context, _, _, _ string, _ int) error {
	return nil
}

// TestSubmit_ConcurrentLimit_ReleaseOnSaveFailure verifies that ReleaseSlot is
// called to free the in-flight slot when Redis save fails after a successful check.
func TestSubmit_ConcurrentLimit_ReleaseOnSaveFailure(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{saveErr: fmt.Errorf("redis down")}
	limiter := &mockConcurrentLimiter{
		result: ratelimit.CheckResult{Allowed: true, Limit: 1, Remaining: 0},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithConcurrentLimiter(limiter, "X-User-Type").
		Submit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on Redis save failure, got %d", w.Code)
	}
	if limiter.releaseCalls != 1 {
		t.Errorf("expected ReleaseSlot called once to free the in-flight slot, got %d", limiter.releaseCalls)
	}
}

// TestSubmit_ConcurrentLimit_ReleaseOnSuccess verifies that ReleaseSlot is called
// on the success path so the in-flight slot is freed after the job is persisted.
func TestSubmit_ConcurrentLimit_ReleaseOnSuccess(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	limiter := &mockConcurrentLimiter{
		result: ratelimit.CheckResult{Allowed: true, Limit: 1, Remaining: 0},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithConcurrentLimiter(limiter, "X-User-Type").
		Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if limiter.releaseCalls != 1 {
		t.Errorf("expected ReleaseSlot called once after successful save, got %d", limiter.releaseCalls)
	}
}

// TestSubmit_ConcurrentLimit_NoReleaseWhenDenied verifies that ReleaseSlot is NOT
// called when the concurrent check rejects the request (no slot was acquired).
func TestSubmit_ConcurrentLimit_NoReleaseWhenDenied(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	limiter := &mockConcurrentLimiter{
		result: ratelimit.CheckResult{Allowed: false, Limit: 1, Remaining: 0},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithConcurrentLimiter(limiter, "X-User-Type").
		Submit(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}
	if limiter.releaseCalls != 0 {
		t.Errorf("expected ReleaseSlot not called when denied, got %d", limiter.releaseCalls)
	}
}

// ── Authz tests ───────────────────────────────────────────────────────────────

// buildJobAuthzEngine returns an Engine that allows "faster-whisper" for any
// principal and denies everything else (default deny).
func buildJobAuthzEngine() *authz.Engine {
	return authz.New(config.PoliciesConfig{
		Rules: []config.PolicyRule{
			{
				Match:       config.PolicyMatch{},
				AllowModels: []string{"faster-whisper"},
			},
		},
	})
}

// singleOpRegistryWithExtra adds a "blocked-model" service alongside the
// default singleOpRegistry model so we have a model the engine will deny.
func singleOpRegistryWithExtra() *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{
		{
			Type:  "transcription",
			Model: "faster-whisper",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			AcceptedExts:  []string{".mp3", ".wav"},
			MaxFileSizeMB: 100,
		},
		{
			Type:  "transcription",
			Model: "blocked-model",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			AcceptedExts:  []string{".mp3", ".wav"},
			MaxFileSizeMB: 100,
		},
	})
}

// TestSubmit_Authz_NilEngine_NoEnforcement verifies backward compatibility:
// with a nil engine requests proceed regardless of model.
func TestSubmit_Authz_NilEngine_NoEnforcement(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{Consumer: "alice", Authenticated: true}))
	w := httptest.NewRecorder()
	// No WithAuthz → nil engine
	newAsyncHandler(singleOpRegistry(), s3, store).Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("nil engine: expected 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSubmit_Authz_DeniedModel_Returns403 verifies that submitting to a denied
// model returns 403 with no S3 or Redis side effects.
func TestSubmit_Authz_DeniedModel_Returns403(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	engine := buildJobAuthzEngine()
	req := submitReq(t, "transcription", "blocked-model", "", "audio.wav", []byte("data"))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{Consumer: "alice", Authenticated: true}))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistryWithExtra(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithAuthz(engine).
		Submit(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for denied model, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when model is denied")
	}
	if store.saved {
		t.Error("Redis SaveJob must not be called when model is denied")
	}
	if !strings.Contains(w.Body.String(), "denied") {
		t.Errorf("expected 'denied' in response body, got: %s", w.Body.String())
	}
}

// TestSubmit_Authz_AllowedModel_Proceeds verifies that the allowed model
// proceeds to the normal 202 path when authz is configured.
func TestSubmit_Authz_AllowedModel_Proceeds(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	engine := buildJobAuthzEngine()
	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{Consumer: "alice", Authenticated: true}))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithAuthz(engine).
		Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 for allowed model, got %d: %s", w.Code, w.Body.String())
	}
	if !s3.uploaded {
		t.Error("S3 upload should have been called for allowed model")
	}
	if !store.saved {
		t.Error("Redis SaveJob should have been called for allowed model")
	}
}

// TestSubmit_ProcessingTimeLimit_Denied verifies that a 429 is returned when the
// processing time budget is exhausted, with no S3/Redis side effects.
func TestSubmit_ProcessingTimeLimit_Denied(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	ptLimiter := &mockProcessingTimeLimiter{
		result: ratelimit.CheckResult{Allowed: false, Limit: 10, Remaining: 0, ResetAfter: time.Hour},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithConcurrentLimiter(&mockConcurrentLimiter{result: ratelimit.CheckResult{Allowed: true}}, "X-User-Type").
		WithProcessingTimeLimiter(ptLimiter).
		Submit(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 when processing time budget exceeded, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when processing time budget is exceeded")
	}
	if store.saved {
		t.Error("Redis SaveJob must not be called when processing time budget is exceeded")
	}
}

// TestSubmit_ProcessingTimeLimit_DeniedBeforeConcurrent verifies that the
// processing time check fires before the concurrent check, so a denied PT
// request also skips the concurrent check (no S3/Redis side effects).
func TestSubmit_ProcessingTimeLimit_DeniedBeforeConcurrent(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	concLimiter := &mockConcurrentLimiter{result: ratelimit.CheckResult{Allowed: true, Limit: 5, Remaining: 4}}
	ptLimiter := &mockProcessingTimeLimiter{
		result: ratelimit.CheckResult{Allowed: false, Limit: 10, Remaining: 0, ResetAfter: time.Hour},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithConcurrentLimiter(concLimiter, "X-User-Type").
		WithProcessingTimeLimiter(ptLimiter).
		Submit(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when PT budget is exceeded")
	}
	if store.saved {
		t.Error("Redis SaveJob must not be called when PT budget is exceeded")
	}
}

// TestSubmit_ProcessingTimeLimit_FailOpen verifies that a check error is
// treated as allowed (fail-open) so transient Redis errors don't block jobs.
func TestSubmit_ProcessingTimeLimit_FailOpen(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	ptLimiter := &mockProcessingTimeLimiter{
		checkErr: fmt.Errorf("redis unavailable"),
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithProcessingTimeLimiter(ptLimiter).
		Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 on check error (fail-open), got %d: %s", w.Code, w.Body.String())
	}
}

// TestSubmit_ProcessingTimeLimit_Allowed verifies that a request within budget
// proceeds to S3 and Redis normally.
func TestSubmit_ProcessingTimeLimit_Allowed(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	ptLimiter := &mockProcessingTimeLimiter{
		result: ratelimit.CheckResult{Allowed: true, Limit: 10, Remaining: 7},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithProcessingTimeLimiter(ptLimiter).
		Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 when budget available, got %d: %s", w.Code, w.Body.String())
	}
	if !s3.uploaded {
		t.Error("S3 upload should have been called")
	}
	if !store.saved {
		t.Error("Redis SaveJob should have been called")
	}
}

// TestSubmit_TokenLimit_Denied verifies that a 429 is returned when the
// token budget is exhausted, with no S3/Redis side effects.
func TestSubmit_TokenLimit_Denied(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	tLimiter := &mockTokenLimiter{
		result: ratelimit.CheckResult{Allowed: false, Limit: 10000, Remaining: 0, ResetAfter: time.Hour},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithTokenLimiter(tLimiter).
		Submit(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 when token budget exceeded, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when token budget is exceeded")
	}
	if store.saved {
		t.Error("Redis SaveJob must not be called when token budget is exceeded")
	}
}

// TestSubmit_TokenLimit_FailOpen verifies that a check error is treated as
// allowed (fail-open) so transient Redis errors don't block jobs.
func TestSubmit_TokenLimit_FailOpen(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	tLimiter := &mockTokenLimiter{checkErr: fmt.Errorf("redis unavailable")}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithTokenLimiter(tLimiter).
		Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 on check error (fail-open), got %d: %s", w.Code, w.Body.String())
	}
}

// TestSubmit_TokenLimit_Allowed verifies that a request within budget
// proceeds to S3 and Redis normally.
func TestSubmit_TokenLimit_Allowed(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	tLimiter := &mockTokenLimiter{
		result: ratelimit.CheckResult{Allowed: true, Limit: 10000, Remaining: 9000},
	}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	handler.NewJobHandler(singleOpRegistry(), s3, store, "", "", nil, config.LifecycleConfig{}).
		WithTokenLimiter(tLimiter).
		Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 when budget available, got %d: %s", w.Code, w.Body.String())
	}
	if !s3.uploaded {
		t.Error("S3 upload should have been called")
	}
	if !store.saved {
		t.Error("Redis SaveJob should have been called")
	}
}

// ── async result DONE-gate ─────────────────────────────────────────────────────

func gatedStore(gate *storage.ScanGate) *mockAsyncStore {
	now := time.Now().UTC()
	return &mockAsyncStore{
		job: &model.Job{
			ID: "g1", ServiceType: "transcription", Model: "faster-whisper",
			Status: model.JobStatusCompleted, ResultRef: "g1/result.json",
			CreatedAt: now, UpdatedAt: now,
		},
		scanGate: gate,
	}
}

func gatedStatus(t *testing.T, store *mockAsyncStore) string {
	t.Helper()
	w := httptest.NewRecorder()
	newHandlerWithConsumer(singleOpRegistry(), &mockJobS3{}, store).
		GetStatus(w, statusReq(t, "transcription", "g1"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	s, _ := resp["status"].(string)
	return s
}

func TestGetStatus_ScanGate_Armed_WithholdsResult(t *testing.T) {
	// Completion just now + a live timeout = deadline in the future (scan in
	// progress); a completed job must read as processing.
	store := gatedStore(&storage.ScanGate{Timeout: time.Minute})
	if s := gatedStatus(t, store); s != string(model.JobStatusProcessing) {
		t.Fatalf("armed gate should withhold (processing), got %q", s)
	}
}

func TestGetStatus_ScanGate_FailOpen_Delivers(t *testing.T) {
	store := gatedStore(&storage.ScanGate{Timeout: time.Minute, FailClosed: false})
	store.job.UpdatedAt = time.Now().Add(-time.Hour) // completed long ago → deadline lapsed
	if s := gatedStatus(t, store); s != string(model.JobStatusCompleted) {
		t.Fatalf("lapsed fail-open gate should deliver (completed), got %q", s)
	}
	if !store.scanGateCleared {
		t.Error("lapsed fail-open gate should be cleared")
	}
}

func TestGetStatus_ScanGate_FailClosed_ReportsFailed(t *testing.T) {
	store := gatedStore(&storage.ScanGate{Timeout: time.Minute, FailClosed: true})
	store.job.UpdatedAt = time.Now().Add(-time.Hour) // completed long ago → deadline lapsed
	if s := gatedStatus(t, store); s != string(model.JobStatusFailed) {
		t.Fatalf("lapsed fail-closed gate should report failed, got %q", s)
	}
	if !store.overrideCalled || store.overrideStatus != model.JobStatusFailed {
		t.Error("lapsed fail-closed gate must durably persist the job as failed")
	}
	if !store.scanGateCleared {
		t.Error("lapsed fail-closed gate should be cleared")
	}
}

func asyncActionRegistry(action string) *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{{
		Type: "transcription", Model: "faster-whisper",
		Operations:    map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		AcceptedExts:  []string{".wav"},
		MaxFileSizeMB: 100,
		Guardrails:    config.GuardrailsConfig{Async: &config.GuardrailsAsyncConfig{Checks: []string{"pii"}, Action: action}},
	}})
}

func TestSubmit_EnforcingAsync_ArmsScanGate(t *testing.T) {
	store := &mockAsyncStore{}
	req := submitReq(t, "transcription", "faster-whisper", "transcription", "a.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(asyncActionRegistry("block"), &mockJobS3{}, store).Submit(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if !store.scanGateSet {
		t.Error("enforcing (block) async must arm the result scan gate at submit")
	}
}

func TestSubmit_ShadowAsync_NoScanGate(t *testing.T) {
	store := &mockAsyncStore{}
	req := submitReq(t, "transcription", "faster-whisper", "transcription", "a.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(asyncActionRegistry("flag"), &mockJobS3{}, store).Submit(w, req)
	if store.scanGateSet {
		t.Error("shadow (flag) async must not arm a scan gate")
	}
}
