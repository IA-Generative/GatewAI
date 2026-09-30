package handler

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel"
)

// otelResponseWriter wraps http.ResponseWriter to capture the status code.
type otelResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *otelResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the wrapped writer so http.ResponseController and libraries that
// walk the writer chain (e.g. the WebSocket upgrader) can reach optional
// interfaces like http.Hijacker and http.Flusher that this wrapper does not
// itself implement. Without it a WebSocket upgrade through this middleware fails.
func (rw *otelResponseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// Hijack delegates to the underlying writer so a WebSocket upgrade works even for
// callers that type-assert http.Hijacker directly rather than unwrapping.
func (rw *otelResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(rw.ResponseWriter).Hijack()
}

// OtelMiddleware starts a server span for every request.
// It extracts W3C traceparent from incoming headers, creates a child span,
// and updates the span name with the chi route pattern after routing.
// Paths listed in skip are passed through without creating a span.
// Matching is prefix-based: "/docs" skips "/docs" and "/docs/anything".
func OtelMiddleware(tracer trace.Tracer, skip ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			for _, s := range skip {
				if path == s || strings.HasPrefix(path, s+"/") {
					next.ServeHTTP(w, r)
					return
				}
			}

			// Extract parent context from incoming W3C headers (if any).
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			ctx, span := tracer.Start(ctx, r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("net.host.name", r.Host),
				),
			)
			defer span.End()

			rw := &otelResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(rw, r.WithContext(ctx))

			// Route pattern is available after chi routing completes.
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				if route := rctx.RoutePattern(); route != "" {
					span.SetName(r.Method + " " + route)
					span.SetAttributes(attribute.String("http.route", route))
				}
			}
			span.SetAttributes(attribute.Int("http.status_code", rw.statusCode))
			if rw.statusCode >= 500 {
				span.SetStatus(codes.Error, "server error")
			}
		})
	}
}

// StructuredLogger returns a chi-compatible middleware that emits one
// structured slog entry per request (JSON in production, text in dev).
func StructuredLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r)

			args := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
				"remote", r.RemoteAddr,
			}
			if sc := trace.SpanFromContext(r.Context()).SpanContext(); sc.IsValid() {
				args = append(args, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
			}
			logger.InfoContext(r.Context(), "request", args...)
		})
	}
}
