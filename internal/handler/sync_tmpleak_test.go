package handler_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/service"
)

// The multi-model path parses the upload (spilling >32 MiB to /tmp). net/http's
// own cleanup checks the server's original *Request, but chi and middleware hand
// the handler a WithContext copy, so the handler must remove the spill itself.
// This runs through a real server + WithContext wrapper to prove it does.
func TestSyncHandler_MultipartParsePath_NoTempLeak(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	reg := service.NewRegistry([]config.ServiceConfig{
		{Type: "transcription", Model: "whisper-large-v3", Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}}, InferenceURL: upstream.URL, AcceptedExts: []string{".wav"}, MaxFileSizeMB: 100},
		{Type: "transcription", Model: "whisper-small", Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}}, InferenceURL: upstream.URL, AcceptedExts: []string{".wav"}, MaxFileSizeMB: 100},
	})
	h := handler.NewSyncHandler(reg, "", nil, nil)
	// Mimic chi/middleware: hand the handler a WithContext copy of the request.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey("k"), "v")))
	})
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("model", "whisper-large-v3")
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write(make([]byte, 40<<20)) // > 32 MiB → spills to /tmp during parse
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	srv.Close() // all finishRequest calls have run

	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "multipart-") {
			t.Errorf("multipart temp file leaked despite RemoveAll: %s", e.Name())
		}
	}
}

type ctxKey string

// On an unambiguous (single-model) path the sync handler must stream the upload
// straight to the backend: the backend receives the full raw multipart body,
// and nothing is buffered to a temp file in os.TempDir().
func TestSyncHandler_MultipartStreamsWithoutBuffering(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	var gotBytes int64
	var gotContentType atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType.Store(r.Header.Get("Content-Type"))
		n, _ := io.Copy(io.Discard, r.Body)
		atomic.StoreInt64(&gotBytes, n)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:          "transcription",
		Model:         "whisper-large-v3",
		Operations:    map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		InferenceURL:  upstream.URL,
		AcceptedExts:  []string{".wav"},
		MaxFileSizeMB: 100,
	}})
	h := handler.NewSyncHandler(reg, "", nil, nil)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("model", "whisper-large-v3")
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	if _, err := fw.Write(make([]byte, 40<<20)); err != nil { // 40 MiB: would spill to disk if parsed
		t.Fatal(err)
	}
	_ = mw.Close()
	wantLen := body.Len()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// Backend received the full raw multipart body, unmodified.
	if got := atomic.LoadInt64(&gotBytes); got != int64(wantLen) {
		t.Errorf("backend received %d bytes, want %d (body not streamed verbatim)", got, wantLen)
	}
	if ct, _ := gotContentType.Load().(string); !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("backend Content-Type = %q, want multipart/form-data with boundary", ct)
	}
	// Nothing spilled to a temp file.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "multipart-") {
			t.Errorf("multipart temp file leaked in TMPDIR: %s", e.Name())
		}
	}
}

// On a multi-model path the "model" form field selects the backend: the form is
// parsed, then the request is reconstructed and proxied to the chosen model.
func TestSyncHandler_MultipartMultiModel_RoutesByField(t *testing.T) {
	var hitA, hitB int64
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hitA, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hitB, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upB.Close()

	reg := service.NewRegistry([]config.ServiceConfig{
		{Type: "transcription", Model: "whisper-large-v3", Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}}, InferenceURL: upA.URL, AcceptedExts: []string{".wav"}, MaxFileSizeMB: 100},
		{Type: "transcription", Model: "whisper-small", Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}}, InferenceURL: upB.URL, AcceptedExts: []string{".wav"}, MaxFileSizeMB: 100},
	})
	h := handler.NewSyncHandler(reg, "", nil, nil)

	req := multipartRequest(t, "/v1/audio/transcriptions", "whisper-small", []byte("audio"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if atomic.LoadInt64(&hitB) != 1 || atomic.LoadInt64(&hitA) != 0 {
		t.Errorf("routing wrong: whisper-small(upB) hits=%d, whisper-large-v3(upA) hits=%d", hitB, hitA)
	}
}

// A streamed body cannot be replayed, so the sync handler makes a single attempt
// even when retries are configured — the backend must be hit exactly once.
func TestSyncHandler_MultipartStream_SingleAttemptNoRetry(t *testing.T) {
	var hits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:          "transcription",
		Model:         "whisper-large-v3",
		Operations:    map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		InferenceURL:  upstream.URL,
		AcceptedExts:  []string{".wav"},
		MaxFileSizeMB: 100,
		Retries:       2, // would be 3 attempts for a replayable (JSON) body
	}})
	h := handler.NewSyncHandler(reg, "", nil, nil)

	// Body must exceed the stream threshold so it streams (single-shot); a small
	// body is buffered in memory and would be retried across attempts.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("model", "whisper-large-v3")
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write(make([]byte, 40<<20))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("backend hit %d times, want 1 (streamed body must not be retried)", got)
	}
}
