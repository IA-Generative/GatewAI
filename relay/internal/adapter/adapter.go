package adapter

import (
	"context"
	"io"
	"net/http"
	"strings"

	"gatewai/relay/internal/config"
)

// CallInput contains the data passed to the adapter for inference.
type CallInput struct {
	JobID        string
	Filename     string
	ContentType  string
	Size         int64             // -1 if unknown
	Body         io.Reader         // stream from S3; caller closes
	Model        string            // model name from InputEvent (e.g. "whisper-large-v3")
	InferenceURL string            // OpenAI path from InputEvent (e.g. "/v1/audio/transcriptions")
	Params       map[string]string // extra form fields forwarded from the client request
	InputMode    string            // "raw" = post Body verbatim; empty/"multipart" = multipart form
}

// Adapter sends an inference request to the local model and returns the raw JSON response.
type Adapter interface {
	Call(ctx context.Context, input CallInput) ([]byte, error)
}

// InputModeRaw posts the stored input object verbatim; any other value uses multipart.
const InputModeRaw = "raw"

// dispatchAdapter picks the adapter per job from CallInput.InputMode.
type dispatchAdapter struct {
	multipart Adapter
	raw       Adapter
}

func (d *dispatchAdapter) Call(ctx context.Context, input CallInput) ([]byte, error) {
	if input.InputMode == InputModeRaw {
		return d.raw.Call(ctx, input)
	}
	return d.multipart.Call(ctx, input)
}

// New returns an adapter that uses multipart by default and raw for input_mode "raw".
func New(cfg *config.Config) (Adapter, error) {
	client := &http.Client{Timeout: cfg.Inference.TimeoutDuration()}
	return &dispatchAdapter{
		multipart: &multipartAdapter{inf: cfg.Inference, client: client},
		raw:       &rawAdapter{inf: cfg.Inference, client: client},
	}, nil
}

// endpointURL constructs the full inference URL from the base and the per-event path.
func endpointURL(inf config.InferenceConfig, input CallInput) string {
	return strings.TrimRight(inf.BaseURL, "/") + input.InferenceURL
}
