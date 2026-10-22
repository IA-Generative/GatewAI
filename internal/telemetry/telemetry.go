package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	metricapi "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// slogErrorHandler routes OTel SDK internal errors (e.g. OTLP export failures)
// through slog so they appear in structured JSON logs instead of raw stderr.
type slogErrorHandler struct{}

func (slogErrorHandler) Handle(err error) {
	slog.Error("otel sdk error", "error", err)
}

// splitOTLPEndpoint parses an OTLP/HTTP base endpoint ("http://host:4318") into
// the host:port to target, any explicit non-root path, and whether the transport
// is plaintext. A path-less endpoint is applied via WithEndpoint so the per-signal
// default path (/v1/traces, /v1/metrics, /v1/logs) is appended — WithEndpointURL on
// a path-less URL instead posts to "/" and the collector 404s.
func splitOTLPEndpoint(endpoint string, insecureCfg bool) (host, path string, insecure bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return endpoint, "", insecureCfg
	}
	if p := u.Path; p != "" && p != "/" {
		path = p
	}
	return u.Host, path, insecureCfg || u.Scheme != "https"
}

// Telemetry holds pre-created Tracer and Meter for the service.
// All global OTel providers are registered when Setup returns — components can
// call otel.Tracer(name) directly without holding this struct.
type Telemetry struct {
	Tracer trace.Tracer
	Meter  metricapi.Meter
}

// Setup initialises OTel providers based on cfg and registers them as globals.
// Returns a shutdown function that must be deferred by the caller to flush and
// close all exporters (critical for the relay one-shot process).
// When cfg.Enabled is false, returns no-op providers and a no-op shutdown.
func Setup(ctx context.Context, cfg OtelConfig, svcName, svcVersion string) (*Telemetry, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }

	if !cfg.Enabled {
		slog.Info("opentelemetry disabled")
		return &Telemetry{
			Tracer: otel.GetTracerProvider().Tracer(svcName),
			Meter:  otel.GetMeterProvider().Meter(svcName),
		}, noop, nil
	}

	// Route OTel SDK internal errors (export failures, etc.) through slog.
	otel.SetErrorHandler(slogErrorHandler{})

	tracesEndpoint, _ := resolveSignal(cfg.Exporter, cfg.Traces.Endpoint, cfg.Traces.Headers)
	slog.Info("opentelemetry initialising",
		"service", svcName,
		"version", svcVersion,
		"traces_enabled", cfg.Traces.Enabled,
		"traces_endpoint", tracesEndpoint,
		"metrics_enabled", cfg.Metrics.Enabled,
		"logs_enabled", cfg.Logs.Enabled,
		"insecure", cfg.Exporter.Insecure,
		"sample_ratio", cfg.Traces.SampleRatio,
	)

	// W3C TraceContext + Baggage propagation — must be set before any span is created.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(svcName),
			semconv.ServiceVersion(svcVersion),
		),
	)
	if err != nil {
		return nil, noop, fmt.Errorf("otel resource: %w", err)
	}

	var shutdowns []func(context.Context) error
	shutdown := func(ctx context.Context) error {
		var last error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			if err := shutdowns[i](ctx); err != nil {
				last = err
			}
		}
		return last
	}

	tel := &Telemetry{}

	// ── Traces ────────────────────────────────────────────────────────────────
	if cfg.Traces.Enabled {
		endpoint, headers := resolveSignal(cfg.Exporter, cfg.Traces.Endpoint, cfg.Traces.Headers)
		host, path, insecure := splitOTLPEndpoint(endpoint, cfg.Exporter.Insecure)
		opts := []otlptracehttp.Option{otlptracehttp.WithHeaders(headers)}
		if path != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlptracehttp.WithEndpoint(host))
		}
		if insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, shutdown, fmt.Errorf("otlp trace exporter: %w", err)
		}

		sampler := sdktrace.Sampler(sdktrace.AlwaysSample())
		if r := cfg.Traces.SampleRatio; r > 0 && r < 1.0 {
			sampler = sdktrace.TraceIDRatioBased(r)
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sampler),
		)
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
		tel.Tracer = tp.Tracer(svcName, trace.WithInstrumentationVersion(svcVersion))
	} else {
		tel.Tracer = otel.GetTracerProvider().Tracer(svcName)
	}

	// ── Metrics ───────────────────────────────────────────────────────────────
	if cfg.Metrics.Enabled {
		endpoint, headers := resolveSignal(cfg.Exporter, cfg.Metrics.Endpoint, cfg.Metrics.Headers)
		interval := cfg.Metrics.IntervalDuration()
		host, path, insecure := splitOTLPEndpoint(endpoint, cfg.Exporter.Insecure)
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithHeaders(headers)}
		if path != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlpmetrichttp.WithEndpoint(host))
		}
		if insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, shutdown, fmt.Errorf("otlp metric exporter: %w", err)
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(mp)
		shutdowns = append(shutdowns, mp.Shutdown)
		tel.Meter = mp.Meter(svcName, metricapi.WithInstrumentationVersion(svcVersion))
	} else {
		tel.Meter = otel.GetMeterProvider().Meter(svcName)
	}

	// ── Logs ──────────────────────────────────────────────────────────────────
	if cfg.Logs.Enabled {
		endpoint, headers := resolveSignal(cfg.Exporter, cfg.Logs.Endpoint, cfg.Logs.Headers)
		host, path, insecure := splitOTLPEndpoint(endpoint, cfg.Exporter.Insecure)
		opts := []otlploghttp.Option{otlploghttp.WithHeaders(headers)}
		if path != "" {
			opts = append(opts, otlploghttp.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlploghttp.WithEndpoint(host))
		}
		if insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
		exp, err := otlploghttp.New(ctx, opts...)
		if err != nil {
			return nil, shutdown, fmt.Errorf("otlp log exporter: %w", err)
		}
		lp := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
			sdklog.WithResource(res),
		)
		shutdowns = append(shutdowns, lp.Shutdown)
		// Replace the default slog handler with the OTel bridge.
		// All existing slog.Info/Error/Warn calls flow through OTel automatically.
		handler := otelslog.NewHandler(svcName, otelslog.WithLoggerProvider(lp))
		slog.SetDefault(slog.New(handler))
	}

	return tel, shutdown, nil
}
