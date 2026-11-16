package model

import "time"

type JobStatus string

const (
	JobStatusPending    JobStatus = "pending"
	JobStatusProcessing JobStatus = "processing"
	JobStatusCompleted  JobStatus = "completed"
	JobStatusFailed     JobStatus = "failed"
	JobStatusCancelled  JobStatus = "cancelled"
	// JobStatusExpired is not stored on a job record — it is reported (with HTTP
	// 410) when polling a job whose record TTL has passed but that provably
	// existed (a lightweight tombstone outlives the record). Lets clients tell
	// "timed out / cleaned up" apart from "never existed" (404).
	JobStatusExpired JobStatus = "expired"
)

// Job is the full record stored in Redis. CallbackURL is kept internal
// (never exposed in API responses) but persisted so the consumer can
// trigger the webhook when the result arrives minutes or hours later.
type Job struct {
	ID           string            `json:"id"`
	ServiceType  string            `json:"service_type"`
	Model        string            `json:"model"`
	Status       JobStatus         `json:"status"`
	InputRef     string            `json:"input_ref"`
	ResultRef    string            `json:"result_ref,omitempty"`
	CallbackURL  string            `json:"callback_url,omitempty"`
	InferenceURL string            `json:"inference_url,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	// InputMode "raw" means the S3 input object is the final request body, to be
	// POSTed verbatim with its stored content-type. Empty = multipart (default).
	InputMode    string `json:"input_mode,omitempty"`
	ConsumerName string `json:"consumer_name,omitempty"` // set from configurable HTTP header (e.g. X-Consumer-Username)
	// UserType is the value of server.user_type_header at submission time ("*" when absent).
	// Persisted so the consumer can call DecrConcurrent/AddProcessingTime without the original request.
	UserType string `json:"user_type,omitempty"`
	Priority bool   `json:"priority,omitempty"` // true = inserted at head of relay queue (LPUSH)
	Error    string `json:"error,omitempty"`
	// ProcessingTime is the inference processing duration in seconds, extracted
	// from the result JSON by the relay and written back into the job record.
	ProcessingTime float64 `json:"processing_time,omitempty"`
	// PromptTokens and CompletionTokens are extracted from the inference result
	// JSON's OpenAI-compatible usage object and written back by the relay.
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	// QueuePosition is not persisted — populated transiently by the storage layer for pending jobs.
	QueuePosition *int64 `json:"queue_position,omitempty"`
	// TraceContext carries the W3C traceparent of the submit request so the relay
	// can create a child span under the gateway's trace.
	TraceContext string `json:"trace_context,omitempty"`
}

// InputEvent is pushed to the model-specific Redis relay queue.
// The relay consumes this to trigger processing.
type InputEvent struct {
	JobID        string            `json:"job_id"`
	ServiceType  string            `json:"service_type"`
	Model        string            `json:"model"`
	InputRef     string            `json:"input_ref"`        // S3 object key: "{job_id}/input.ext"
	InferenceURL string            `json:"inference_url"`    // OpenAI path the relay must append to its local base URL
	Params       map[string]string `json:"params,omitempty"` // extra form fields forwarded to the inference API
	CreatedAt    time.Time         `json:"created_at"`
}

// ResultEvent is published to the jobs:{model}:completed Redis pub/sub channel.
// The inference worker publishes this when processing completes (or fails).
type ResultEvent struct {
	JobID       string    `json:"job_id"`
	ServiceType string    `json:"service_type"`
	Status      JobStatus `json:"status"`               // completed | failed
	ResultRef   string    `json:"result_ref,omitempty"` // MinIO object key for the output
	Error       string    `json:"error,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}
