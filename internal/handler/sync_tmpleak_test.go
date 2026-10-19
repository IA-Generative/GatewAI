package handler_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/handler"
	"gatewai/gateway/internal/service"
)

// A multipart upload over the 32 MiB in-memory limit spills to a temp file in
// os.TempDir(); the handler must remove it when the request ends, or uploads
// accumulate in the pod's ephemeral storage until the kubelet evicts it.
func TestSyncHandler_MultipartTempFilesCleanedUp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
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
	if _, err := fw.Write(make([]byte, 40<<20)); err != nil { // 40 MiB forces a disk spill
		t.Fatal(err)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(httptest.NewRecorder(), req)

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
