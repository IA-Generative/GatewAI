package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/service"
)

// realtimeReadLimit bounds a single WebSocket message. Audio frames and transcript
// frames are both well under this; it guards against a runaway allocation.
const realtimeReadLimit = 4 << 20 // 4 MiB

// realtimeSessionLimiter caps concurrent realtime sessions per consumer. It is
// per-replica (in-memory): with multiple gateway replicas the effective cap is
// the configured value times the replica count. That's an acceptable soft bound
// for a connection-count guard; cumulative audio quota is metered cross-replica
// via the usage store.
type realtimeSessionLimiter struct {
	mu     sync.Mutex
	active map[string]int
}

func newRealtimeSessionLimiter() *realtimeSessionLimiter {
	return &realtimeSessionLimiter{active: make(map[string]int)}
}

// acquire reserves a session slot for consumer. max <= 0 means unlimited. An empty
// consumer is never capped (anonymous callers are not tracked).
func (l *realtimeSessionLimiter) acquire(consumer string, max int) bool {
	if consumer == "" || max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[consumer] >= max {
		return false
	}
	l.active[consumer]++
	return true
}

func (l *realtimeSessionLimiter) release(consumer string, max int) {
	if consumer == "" || max <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[consumer] > 0 {
		l.active[consumer]--
	}
	if l.active[consumer] == 0 {
		delete(l.active, consumer)
	}
}

// ServeRealtimeWS proxies a bidirectional WebSocket connection (e.g. real-time
// transcription) to the backend, applying GatewAI's policy layer that an opaque
// APISIX passthrough cannot: model visibility and authz are enforced BEFORE the
// upgrade, a per-consumer concurrent-session cap is applied at the handshake, and
// streamed audio-seconds are metered for usage/quota on close.
//
// Slices 1–2: transport + handshake gating + concurrency cap + audio-seconds
// metering + usage/metrics. Transcript guardrails follow in a later slice.
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
	rt := def.Realtime

	// Gate before upgrading — fail closed on visibility/authz.
	if !checkModelVisible(w, r, def, h.userTypeHeader) {
		return
	}
	if _, ok := h.checkAccess(w, r, def); !ok {
		return
	}

	consumer, userType := h.resolveConsumerAndType(r)

	// Per-consumer concurrent-session cap, enforced before the upgrade.
	if !h.realtimeSessions.acquire(consumer, rt.MaxConcurrentPerConsumer) {
		metrics.RealtimeSessionsTotal.WithLabelValues(def.Type, def.Model, "rejected").Inc()
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "too many concurrent realtime sessions")
		return
	}
	defer h.realtimeSessions.release(consumer, rt.MaxConcurrentPerConsumer)

	backendURL, backendHeaders, ok := realtimeBackend(def)
	if !ok {
		metrics.RealtimeSessionsTotal.WithLabelValues(def.Type, def.Model, "error").Inc()
		writeError(w, http.StatusInternalServerError, "no backend configured")
		return
	}
	dialURL, err := realtimeDialURL(backendURL, rt.BackendPath, r.URL.RawQuery)
	if err != nil {
		metrics.RealtimeSessionsTotal.WithLabelValues(def.Type, def.Model, "error").Inc()
		writeError(w, http.StatusInternalServerError, "invalid backend URL")
		return
	}

	clientConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		metrics.RealtimeSessionsTotal.WithLabelValues(def.Type, def.Model, "error").Inc()
		slog.WarnContext(r.Context(), "realtime: client websocket accept failed", "error", err)
		return
	}
	clientConn.SetReadLimit(realtimeReadLimit)
	defer clientConn.CloseNow()

	// Detach from the request context (the server cancels it once this handler
	// returns) but keep our own cancel for teardown, plus the optional session cap.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	if secs := rt.MaxSessionSeconds; secs > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer stop()
	}

	backendConn, _, err := websocket.Dial(ctx, dialURL, &websocket.DialOptions{
		HTTPHeader: realtimeDialHeaders(r, def.InferenceHeaders, backendHeaders),
	})
	if err != nil {
		metrics.RealtimeSessionsTotal.WithLabelValues(def.Type, def.Model, "error").Inc()
		slog.WarnContext(r.Context(), "realtime: backend dial failed", "url", dialURL, "error", err)
		_ = clientConn.Write(ctx, websocket.MessageText, []byte(`{"type":"error","message":"backend unavailable"}`))
		clientConn.Close(websocket.StatusInternalError, "backend unavailable")
		return
	}
	backendConn.SetReadLimit(realtimeReadLimit)
	defer backendConn.CloseNow()

	// Count the session as a request in per-consumer usage (like the sync path).
	if h.usageTracker != nil && consumer != "" {
		h.usageTracker.TrackRequest(ctx, consumer, def.Type)
		h.usageTracker.TrackActive(ctx, consumer)
		h.usageTracker.TrackUserType(ctx, consumer, def.Type, userType)
	}
	metrics.RealtimeActiveSessions.WithLabelValues(def.Type, def.Model).Inc()
	defer metrics.RealtimeActiveSessions.WithLabelValues(def.Type, def.Model).Dec()

	sess := &realtimeSession{
		start:         time.Now(),
		bytesPerSec:   rt.BytesPerSecond(),
		maxAudioBytes: int64(rt.MaxAudioSeconds) * int64(rt.BytesPerSecond()),
	}

	// Relay both directions until either side closes/errors or the audio cap trips.
	done := make(chan struct{}, 2)
	go func() { relayWS(ctx, clientConn, backendConn, sess.onClientFrame); done <- struct{}{} }() // client → backend
	go func() { relayWS(ctx, backendConn, clientConn, nil); done <- struct{}{} }()                // backend → client
	<-done
	cancel()
	clientConn.Close(websocket.StatusNormalClosure, "")
	backendConn.Close(websocket.StatusNormalClosure, "")
	<-done // wait for the second pump so session counters are stable

	h.recordRealtimeUsage(ctx, def, consumer, userType, sess)
}

// realtimeSession accumulates per-session accounting.
type realtimeSession struct {
	start         time.Time
	bytesPerSec   int
	maxAudioBytes int64 // 0 = no cap

	audioBytes  int64 // client → backend binary bytes (single writer: the client pump)
	capExceeded bool
}

// onClientFrame meters audio on the client→backend direction and enforces the cap.
func (s *realtimeSession) onClientFrame(typ websocket.MessageType, data []byte) error {
	if typ != websocket.MessageBinary {
		return nil
	}
	s.audioBytes += int64(len(data))
	if s.maxAudioBytes > 0 && s.audioBytes > s.maxAudioBytes {
		s.capExceeded = true
		return errRealtimeAudioCap
	}
	return nil
}

func (s *realtimeSession) audioSeconds() float64 {
	if s.bytesPerSec <= 0 {
		return 0
	}
	return float64(s.audioBytes) / float64(s.bytesPerSec)
}

var errRealtimeAudioCap = &realtimeError{"audio duration cap exceeded"}

type realtimeError struct{ msg string }

func (e *realtimeError) Error() string { return e.msg }

func (h *SyncHandler) recordRealtimeUsage(ctx context.Context, def *service.Def, consumer, userType string, sess *realtimeSession) {
	audioSecs := sess.audioSeconds()
	metrics.RealtimeSessionDuration.WithLabelValues(def.Type, def.Model).Observe(time.Since(sess.start).Seconds())
	if audioSecs > 0 {
		metrics.RealtimeAudioSecondsTotal.WithLabelValues(def.Type, def.Model, userType).Add(audioSecs)
	}
	outcome := "completed"
	if sess.capExceeded {
		outcome = "rejected"
	}
	metrics.RealtimeSessionsTotal.WithLabelValues(def.Type, def.Model, outcome).Inc()
	if h.usageTracker != nil && consumer != "" && audioSecs > 0 {
		// Audio-seconds are the realtime analogue of processing time.
		h.usageTracker.TrackProcessingTime(ctx, consumer, def.Type, audioSecs)
	}
}

// relayWS copies whole WebSocket messages from src to dst, preserving the message
// type (text control/transcript frames vs binary audio). onMsg, when non-nil, is
// called with each message before it is forwarded; returning an error stops the
// pump (and tears down the session), e.g. when the audio cap is exceeded.
func relayWS(ctx context.Context, src, dst *websocket.Conn, onMsg func(websocket.MessageType, []byte) error) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if onMsg != nil {
			if err := onMsg(typ, data); err != nil {
				return
			}
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
	out.Del("Upgrade")
	out.Del("Connection")
	return out
}
