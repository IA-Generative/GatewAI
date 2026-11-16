package adapter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gatewai/relay/internal/config"
)

func rawInput(body, ct string) CallInput {
	return CallInput{
		JobID:        "job-1",
		ContentType:  ct,
		Size:         int64(len(body)),
		Body:         strings.NewReader(body),
		InferenceURL: "/v1/chat/completions",
		InputMode:    InputModeRaw,
	}
}

func TestRawAdapter_PostsBodyVerbatim(t *testing.T) {
	const payload = `{"model":"m","messages":[]}`
	var gotBody, gotCT, gotAuth, gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT, gotAuth, gotPath = string(b), r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()

	a := &rawAdapter{
		inf:    config.InferenceConfig{BaseURL: backend.URL, APIKey: "k"},
		client: &http.Client{Timeout: 5 * time.Second},
	}
	got, err := a.Call(context.Background(), rawInput(payload, "application/json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("unexpected response: %s", got)
	}
	if gotBody != payload {
		t.Errorf("body not verbatim: %q", gotBody)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestRawAdapter_EmptyContentTypeFallsBack(t *testing.T) {
	var gotCT string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer backend.Close()

	a := &rawAdapter{inf: config.InferenceConfig{BaseURL: backend.URL}, client: &http.Client{Timeout: 5 * time.Second}}
	if _, err := a.Call(context.Background(), rawInput("x", "")); err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("content-type = %q", gotCT)
	}
}

func TestRawAdapter_NonSuccessStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad"}`)
	}))
	defer backend.Close()

	a := &rawAdapter{inf: config.InferenceConfig{BaseURL: backend.URL}, client: &http.Client{Timeout: 5 * time.Second}}
	_, err := a.Call(context.Background(), rawInput("x", "application/json"))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 error, got %v", err)
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("expected response body in error, got %v", err)
	}
}

// Production S3 returns size -1, so the request goes out chunked.
func TestRawAdapter_UnknownSizeStillSucceeds(t *testing.T) {
	const payload = `{"a":1}`
	var gotBody string
	var chunked bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		chunked = r.ContentLength == -1
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()

	a := &rawAdapter{inf: config.InferenceConfig{BaseURL: backend.URL}, client: &http.Client{Timeout: 5 * time.Second}}
	in := rawInput(payload, "application/json")
	in.Size = -1
	// Hide the concrete type so net/http can't infer a length, like the S3 stream.
	in.Body = io.MultiReader(strings.NewReader(payload))
	got, err := a.Call(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"ok":true}` || gotBody != payload {
		t.Errorf("got response %q body %q", got, gotBody)
	}
	if !chunked {
		t.Error("expected chunked request (unknown content length)")
	}
}

type recAdapter struct {
	name string
	hit  *string
}

func (r recAdapter) Call(context.Context, CallInput) ([]byte, error) {
	*r.hit = r.name
	return nil, nil
}

func TestDispatch_SelectsByInputMode(t *testing.T) {
	cases := map[string]string{"": "multipart", "multipart": "multipart", "raw": "raw", "bogus": "multipart"}
	for mode, want := range cases {
		var hit string
		d := &dispatchAdapter{multipart: recAdapter{"multipart", &hit}, raw: recAdapter{"raw", &hit}}
		if _, err := d.Call(context.Background(), CallInput{InputMode: mode}); err != nil {
			t.Fatal(err)
		}
		if hit != want {
			t.Errorf("mode %q -> %s, want %s", mode, hit, want)
		}
	}
}
