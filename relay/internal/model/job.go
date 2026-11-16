package model

// JobStatus represents the terminal state of a relay job.
type JobStatus string

const (
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
)

// Job holds the fields the relay needs to process a job.
// Unmarshaled from the gateway's JSON blob at job:{id}.
type Job struct {
	ID           string            `json:"id"`
	ServiceType  string            `json:"service_type"`
	Model        string            `json:"model"`
	Status       JobStatus         `json:"status"`
	InputRef     string            `json:"input_ref"`
	InferenceURL string            `json:"inference_url,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	// InputMode "raw" means the stored input is the final request body, sent verbatim.
	InputMode string `json:"input_mode,omitempty"`
	// TraceContext carries the W3C traceparent injected by the gateway at
	// submit time, enabling the relay to create a child span in the same trace.
	TraceContext string `json:"trace_context,omitempty"`
	// PromptTokens and CompletionTokens are written back by UpdateJobResult
	// after being extracted from the inference result JSON's usage object.
	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
}
