package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"gatewai/relay/internal/adapter"
	"gatewai/relay/internal/metrics"
	"gatewai/relay/internal/model"
	"gatewai/relay/internal/storage"
)

type objectStore interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, string, error)
	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	DeleteObject(ctx context.Context, key string) error
}

// eventPublisher wraps the Redis result pipeline: UpdateJobResult + Publish + Done.
type eventPublisher interface {
	PublishResult(ctx context.Context, jobID string, status model.JobStatus, resultRef, errMsg string, processingTime float64, promptTokens, completionTokens int64) error
}

// Processor runs the full processing pipeline for a single Job pulled from the Redis queue.
type Processor struct {
	adapter   adapter.Adapter
	s3        objectStore
	publisher eventPublisher
	tracer    trace.Tracer
}

// New creates a Processor. pub handles persisting the result and notifying the gateway.
func New(adp adapter.Adapter, s3 *storage.S3Client, pub eventPublisher) *Processor {
	return &Processor{
		adapter:   adp,
		s3:        s3,
		publisher: pub,
		tracer:    otel.Tracer("gatewai/relay"),
	}
}

// Process runs the full job pipeline for the given Job.
// It returns an error only for transient infrastructure failures (S3, Redis,
// network) so the caller can exit 1 and let the orchestrator retry the Job.
// Inference errors are published as failed results and return nil.
func (p *Processor) Process(ctx context.Context, job *model.Job) error {
	return p.process(ctx, job)
}

// process orchestrates the complete pipeline. It returns an error only for
// infrastructure pannes (S3 unavailable, Redis unreachable) so the pod can
// exit 1 and be restarted. Inference failures are published as failed results
// and return nil (the job is definitively terminated).
//
// Retry strategy: each transient step (inference, S3 put result, result publish)
// is retried once immediately before escalating. The inference retry requires a
// fresh S3 download (the previous stream is exhausted).
// The initial GetObject is not retried — an infra error there is escalated directly.
func (p *Processor) process(ctx context.Context, job *model.Job) error {
	// Restore trace context from the gateway so this span is a child of the
	// submit request even though the relay runs in a separate process.
	if job.TraceContext != "" {
		carrier := propagation.MapCarrier{"traceparent": job.TraceContext}
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}
	ctx, span := p.tracer.Start(ctx, "relay.process_job",
		trace.WithAttributes(
			attribute.String("job_id", job.ID),
			attribute.String("service_type", job.ServiceType),
			attribute.String("model", job.Model),
		))
	defer span.End()

	log := slog.With("job_id", job.ID, "service_type", job.ServiceType)
	if sc := span.SpanContext(); sc.IsValid() {
		log = log.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	log.Info("processing job", "input_ref", job.InputRef)

	s3Ctx, s3Span := p.tracer.Start(ctx, "relay.s3.get_input",
		trace.WithAttributes(attribute.String("s3.key", job.InputRef)))
	body, size, contentType, err := p.s3.GetObject(s3Ctx, job.InputRef)
	if err != nil {
		s3Span.RecordError(err)
		s3Span.SetStatus(codes.Error, err.Error())
	} else {
		log.Debug("s3 input downloaded", "s3.key", job.InputRef)
	}
	s3Span.End()
	if err != nil {
		if storage.IsNotFound(err) {
			log.Error("input file not found, publishing permanent failure", "input_ref", job.InputRef)
			metrics.JobsTotal.WithLabelValues(job.ServiceType, "failed").Inc()
			if perr := p.publisher.PublishResult(context.Background(), job.ID, model.JobStatusFailed, "", "input file not found: "+job.InputRef, 0, 0, 0); perr != nil {
				return fmt.Errorf("publishing not-found failure: %w", perr)
			}
			return nil
		}
		return fmt.Errorf("s3 get: %w", err)
	}
	defer body.Close()

	if size > 0 {
		metrics.InputSizeBytes.WithLabelValues(job.ServiceType).Observe(float64(size))
	}

	result, inferErr := p.runInference(ctx, job, body, size, contentType)
	if inferErr != nil {
		if errors.Is(inferErr, context.Canceled) {
			return inferErr // job cancelled by gateway; main.go calls q.Done
		}
		// Retry inference once immediately: re-download for a fresh stream.
		log.Warn("inference attempt failed, retrying immediately", "error", inferErr)
		s3RetryCtx, s3RetrySpan := p.tracer.Start(ctx, "relay.s3.get_input",
			trace.WithAttributes(
				attribute.String("s3.key", job.InputRef),
				attribute.Bool("retry", true),
			))
		body2, size2, ct2, getErr := p.s3.GetObject(context.WithoutCancel(s3RetryCtx), job.InputRef)
		if getErr != nil {
			s3RetrySpan.RecordError(getErr)
			s3RetrySpan.SetStatus(codes.Error, getErr.Error())
		} else {
			log.Debug("s3 input downloaded (retry)", "s3.key", job.InputRef)
		}
		s3RetrySpan.End()
		if getErr != nil {
			if storage.IsNotFound(getErr) {
				log.Error("input file not found on inference retry, publishing permanent failure", "input_ref", job.InputRef)
				metrics.JobsTotal.WithLabelValues(job.ServiceType, "failed").Inc()
				if perr := p.publisher.PublishResult(context.Background(), job.ID, model.JobStatusFailed, "", "input file not found on retry: "+job.InputRef, 0, 0, 0); perr != nil {
					return fmt.Errorf("publishing not-found failure: %w", perr)
				}
				return nil
			}
			return fmt.Errorf("s3 get on inference retry: %w", getErr)
		}
		defer body2.Close()
		result, inferErr = p.runInference(ctx, job, body2, size2, ct2)
	}

	if inferErr != nil {
		log.Error("inference failed", "error", inferErr)
		metrics.JobsTotal.WithLabelValues(job.ServiceType, "failed").Inc()
		if perr := p.publisher.PublishResult(context.Background(), job.ID, model.JobStatusFailed, "", fmt.Sprintf("inference: %v", inferErr), 0, 0, 0); perr != nil {
			return fmt.Errorf("publishing failure: %w", perr)
		}
		delCtx := context.WithoutCancel(ctx)
		_, delSpan := p.tracer.Start(delCtx, "relay.s3.delete_input",
			trace.WithAttributes(attribute.String("s3.key", job.InputRef)))
		derr := p.s3.DeleteObject(delCtx, job.InputRef)
		if derr != nil {
			delSpan.RecordError(derr)
			delSpan.SetStatus(codes.Error, derr.Error())
			log.Error("failed to delete input file after failure", "input_ref", job.InputRef, "error", derr)
		} else {
			log.Debug("s3 input deleted (after inference failure)", "s3.key", job.InputRef)
		}
		delSpan.End()
		return nil
	}

	resultKey := job.ID + "/result.json"
	putCtx, putSpan := p.tracer.Start(ctx, "relay.s3.put_result",
		trace.WithAttributes(attribute.String("s3.key", resultKey)))
	if err := p.s3.PutObject(putCtx, resultKey, bytes.NewReader(result), int64(len(result)), "application/json"); err != nil {
		putSpan.RecordError(err)
		log.Warn("s3 put attempt failed, retrying immediately", "error", err)
		if err := p.s3.PutObject(putCtx, resultKey, bytes.NewReader(result), int64(len(result)), "application/json"); err != nil {
			putSpan.RecordError(err)
			putSpan.SetStatus(codes.Error, err.Error())
			putSpan.End()
			return fmt.Errorf("s3 put: %w", err)
		}
	}
	log.Debug("s3 result uploaded", "s3.key", resultKey)
	putSpan.End()

	processingTime := extractProcessingTime(result)
	promptTokens, completionTokens := extractTokens(result)
	pubCtx, pubSpan := p.tracer.Start(ctx, "relay.redis.publish_result",
		trace.WithAttributes(
			attribute.String("job_id", job.ID),
			attribute.String("status", string(model.JobStatusCompleted)),
		))
	if err := p.publisher.PublishResult(pubCtx, job.ID, model.JobStatusCompleted, resultKey, "", processingTime, promptTokens, completionTokens); err != nil {
		pubSpan.RecordError(err)
		log.Warn("publish result attempt failed, retrying immediately", "error", err)
		if err := p.publisher.PublishResult(pubCtx, job.ID, model.JobStatusCompleted, resultKey, "", processingTime, promptTokens, completionTokens); err != nil {
			pubSpan.RecordError(err)
			log.Error("failed to publish result after retry", "error", err)
		}
	} else {
		log.Debug("redis result published", "status", string(model.JobStatusCompleted))
	}
	pubSpan.End()

	metrics.JobsTotal.WithLabelValues(job.ServiceType, "completed").Inc()
	log.Info("job completed", "result_ref", resultKey)

	delCtx := context.WithoutCancel(ctx)
	_, delSpan := p.tracer.Start(delCtx, "relay.s3.delete_input",
		trace.WithAttributes(attribute.String("s3.key", job.InputRef)))
	if err := p.s3.DeleteObject(delCtx, job.InputRef); err != nil {
		delSpan.RecordError(err)
		delSpan.SetStatus(codes.Error, err.Error())
		log.Error("failed to delete input file", "input_ref", job.InputRef, "error", err)
	} else {
		log.Debug("s3 input deleted", "s3.key", job.InputRef)
	}
	delSpan.End()

	return nil
}

// runInference calls the adapter and records timing metrics.
func (p *Processor) runInference(ctx context.Context, job *model.Job, body io.Reader, size int64, contentType string) (_ []byte, err error) {
	ctx, span := p.tracer.Start(ctx, "relay.inference_call",
		trace.WithAttributes(
			attribute.String("job_id", job.ID),
			attribute.String("model", job.Model),
			attribute.String("inference_url", job.InferenceURL),
		))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	inferStart := time.Now()
	result, err := p.adapter.Call(ctx, adapter.CallInput{
		JobID:        job.ID,
		Filename:     filepath.Base(job.InputRef),
		ContentType:  contentType,
		Size:         size,
		Body:         body,
		Model:        job.Model,
		InferenceURL: job.InferenceURL,
		Params:       job.Params,
		InputMode:    job.InputMode,
	})
	metrics.InferenceDuration.WithLabelValues(job.ServiceType, job.Model).Observe(time.Since(inferStart).Seconds())
	return result, err
}

// extractProcessingTime parses the processing_time field (float64 seconds) from
// the inference result JSON. Returns 0 if the field is absent or unparseable.
func extractProcessingTime(result []byte) float64 {
	var v struct {
		ProcessingTime float64 `json:"processing_time"`
	}
	if err := json.Unmarshal(result, &v); err != nil {
		return 0
	}
	return v.ProcessingTime
}

// extractTokens parses the OpenAI-compatible usage.prompt_tokens /
// usage.completion_tokens fields from the inference result JSON. If both are
// absent but usage.total_tokens is present, the total is attributed to prompt
// tokens. Returns (0, 0) if the usage object is absent or unparseable.
func extractTokens(result []byte) (prompt, completion int64) {
	var v struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(result, &v); err != nil {
		return 0, 0
	}
	prompt = v.Usage.PromptTokens
	completion = v.Usage.CompletionTokens
	if prompt == 0 && completion == 0 && v.Usage.TotalTokens > 0 {
		prompt = v.Usage.TotalTokens
	}
	return prompt, completion
}
