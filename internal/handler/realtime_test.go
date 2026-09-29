package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/service"
)

// echoWSBackend is a stub realtime backend: it sends a {"type":"ready"} frame on
// connect, then echoes every frame back with its type preserved.
func echoWSBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"ready"}`))
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}))
}

func realtimeTestServer(t *testing.T, svc config.ServiceConfig) (*httptest.Server, *SyncHandler) {
	t.Helper()
	reg := service.NewRegistry([]config.ServiceConfig{svc})
	h := NewSyncHandler(reg, "", nil, nil)
	h.userTypeHeader = "X-User-Type"
	gw := httptest.NewServer(http.HandlerFunc(h.ServeRealtimeWS))
	t.Cleanup(gw.Close)
	return gw, h
}

func wsURL(base, path string) string { return "ws" + strings.TrimPrefix(base, "http") + path }

func TestServeRealtimeWS_RelaysFramesBothWays(t *testing.T) {
	backend := echoWSBackend(t)
	defer backend.Close()

	gw, _ := realtimeTestServer(t, config.ServiceConfig{
		Type:         "transcription",
		Model:        "rt",
		InferenceURL: backend.URL,
		Realtime:     &config.RealtimeConfig{Path: "/v1/audio/stream"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cc, _, err := websocket.Dial(ctx, wsURL(gw.URL, "/v1/audio/stream"), nil)
	if err != nil {
		t.Fatalf("dial through gateway failed: %v", err)
	}
	defer cc.CloseNow()

	// 1. ready frame forwarded from the backend.
	typ, data, err := cc.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(data) != `{"type":"ready"}` {
		t.Fatalf("expected ready text frame, got typ=%v data=%q err=%v", typ, data, err)
	}

	// 2. client → backend text frame is relayed (echoed back).
	start := `{"type":"start","language":"fr-FR"}`
	if err := cc.Write(ctx, websocket.MessageText, []byte(start)); err != nil {
		t.Fatalf("write start: %v", err)
	}
	typ, data, err = cc.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(data) != start {
		t.Fatalf("text echo mismatch: typ=%v data=%q err=%v", typ, data, err)
	}

	// 3. binary audio frame is relayed with its type preserved.
	pcm := []byte{0x01, 0x00, 0xff, 0x7f}
	if err := cc.Write(ctx, websocket.MessageBinary, pcm); err != nil {
		t.Fatalf("write pcm: %v", err)
	}
	typ, data, err = cc.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != string(pcm) {
		t.Fatalf("binary echo mismatch: typ=%v data=%q err=%v", typ, data, err)
	}

	cc.Close(websocket.StatusNormalClosure, "")
}

func TestServeRealtimeWS_VisibilityGate_ClosedBeforeUpgrade(t *testing.T) {
	backend := echoWSBackend(t)
	defer backend.Close()

	gw, _ := realtimeTestServer(t, config.ServiceConfig{
		Type:         "transcription",
		Model:        "rt",
		InferenceURL: backend.URL,
		Realtime:     &config.RealtimeConfig{Path: "/v1/audio/stream"},
		Visibility:   config.VisibilityConfig{UserTypes: []string{"tier-5"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// No X-User-Type header → caller is not in the model's audience → 404 at the
	// handshake, before any upgrade.
	_, resp, err := websocket.Dial(ctx, wsURL(gw.URL, "/v1/audio/stream"), nil)
	if err == nil {
		t.Fatal("expected handshake to be rejected for an unauthorized caller")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		got := 0
		if resp != nil {
			got = resp.StatusCode
		}
		t.Fatalf("expected 404 at handshake, got status=%d err=%v", got, err)
	}
}

func TestServeRealtimeWS_UnknownPath_404(t *testing.T) {
	backend := echoWSBackend(t)
	defer backend.Close()
	gw, _ := realtimeTestServer(t, config.ServiceConfig{
		Type: "transcription", Model: "rt", InferenceURL: backend.URL,
		Realtime: &config.RealtimeConfig{Path: "/v1/audio/stream"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(gw.URL, "/v1/does-not-exist"), nil)
	if err == nil {
		t.Fatal("expected 404 for unknown realtime path")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %v (err %v)", resp, err)
	}
}
