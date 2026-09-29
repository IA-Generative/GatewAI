package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"gatewai/gateway/internal/service"
)

// realtimeReadLimit bounds a single WebSocket message. Audio frames and transcript
// frames are both well under this; it guards against a runaway allocation.
const realtimeReadLimit = 4 << 20 // 4 MiB

// ServeRealtimeWS proxies a bidirectional WebSocket connection (e.g. real-time
// transcription) to the backend, applying the same access controls as the sync
// path at the handshake: model visibility and authz are enforced BEFORE the
// connection is upgraded, so an unauthorized caller never opens a socket.
//
// Slice 1: transport + handshake gating + verbatim frame relay. Session quotas,
// audio-seconds metering, and transcript guardrails are layered on in later slices.
func (h *SyncHandler) ServeRealtimeWS(w http.ResponseWriter, r *http.Request) {
	def, err := h.registry.RouteRealtime(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown realtime endpoint")
		return
	}
	if def.Realtime == nil {
		writeError(w, http.StatusInternalServerError, "service is not realtime-capable")
		return
	}

	// Gate before upgrading — fail closed on visibility/authz.
	if !checkModelVisible(w, r, def, h.userTypeHeader) {
		return
	}
	if _, ok := h.checkAccess(w, r, def); !ok {
		return
	}

	backendURL, backendHeaders, ok := realtimeBackend(def)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no backend configured")
		return
	}
	dialURL, err := realtimeDialURL(backendURL, def.Realtime.BackendPath, r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid backend URL")
		return
	}

	clientConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept has already written the failure response to the client.
		slog.WarnContext(r.Context(), "realtime: client websocket accept failed", "error", err)
		return
	}
	clientConn.SetReadLimit(realtimeReadLimit)
	defer clientConn.CloseNow()

	// Detach from the request context (the server cancels it once this handler
	// returns) but keep our own cancel for teardown, plus the optional session cap.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	if secs := def.Realtime.MaxSessionSeconds; secs > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer stop()
	}

	backendConn, _, err := websocket.Dial(ctx, dialURL, &websocket.DialOptions{
		HTTPHeader: realtimeDialHeaders(r, def.InferenceHeaders, backendHeaders),
	})
	if err != nil {
		slog.WarnContext(r.Context(), "realtime: backend dial failed", "url", dialURL, "error", err)
		_ = clientConn.Write(ctx, websocket.MessageText, []byte(`{"type":"error","message":"backend unavailable"}`))
		clientConn.Close(websocket.StatusInternalError, "backend unavailable")
		return
	}
	backendConn.SetReadLimit(realtimeReadLimit)
	defer backendConn.CloseNow()

	// Relay both directions until either side closes or errors. The first pipe to
	// end cancels ctx, unblocking the other.
	done := make(chan struct{}, 2)
	go func() { relayWS(ctx, clientConn, backendConn); done <- struct{}{} }() // client → backend
	go func() { relayWS(ctx, backendConn, clientConn); done <- struct{}{} }() // backend → client
	<-done
	cancel()
	clientConn.Close(websocket.StatusNormalClosure, "")
	backendConn.Close(websocket.StatusNormalClosure, "")
}

// relayWS copies whole WebSocket messages from src to dst, preserving the message
// type (text control/transcript frames vs binary audio), until an error occurs.
func relayWS(ctx context.Context, src, dst *websocket.Conn) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

// realtimeBackend picks the backend URL and its headers for a realtime service:
// the first ordered inline backend, else the legacy inference_url.
func realtimeBackend(def *service.Def) (backendURL string, headers map[string]string, ok bool) {
	if len(def.Backends) > 0 {
		b := service.OrderedBackends(def.Backends)[0]
		return b.URL, b.Headers, true
	}
	if def.InferenceURL != "" {
		return def.InferenceURL, def.InferenceHeaders, true
	}
	return "", nil, false
}

// realtimeDialURL builds the backend WebSocket URL: the backend base with its
// scheme mapped to ws/wss, the configured backend path, and the client's query.
func realtimeDialURL(backendURL, backendPath, rawQuery string) (string, error) {
	u, err := url.Parse(backendURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default: // http or already ws
		if u.Scheme != "wss" && u.Scheme != "ws" {
			u.Scheme = "ws"
		}
	}
	u.Path = backendPath
	u.RawQuery = rawQuery
	return u.String(), nil
}

// realtimeDialHeaders builds the headers sent on the backend handshake: the
// client's Authorization, then service-level inference headers, then per-backend
// headers (highest precedence) — mirroring the sync direct-proxy precedence.
// Hop-by-hop WebSocket headers are managed by the dialer and never copied.
func realtimeDialHeaders(r *http.Request, inference, backend map[string]string) http.Header {
	out := http.Header{}
	if auth := r.Header.Get("Authorization"); auth != "" {
		out.Set("Authorization", auth)
	}
	for k, v := range inference {
		out.Set(k, v)
	}
	for k, v := range backend {
		out.Set(k, v)
	}
	// Never forward the client's Upgrade/Connection/Sec-WebSocket-* headers.
	for _, hop := range []string{"Upgrade", "Connection"} {
		out.Del(hop)
	}
	if strings.EqualFold(out.Get("Connection"), "upgrade") {
		out.Del("Connection")
	}
	return out
}
