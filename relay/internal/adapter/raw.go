package adapter

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"gatewai/relay/internal/config"
)

// rawAdapter POSTs the stored input object verbatim (the gateway already built
// the final request body, e.g. a chat-completions JSON) with its own content type.
type rawAdapter struct {
	inf    config.InferenceConfig
	client *http.Client
}

func (a *rawAdapter) Call(ctx context.Context, input CallInput) ([]byte, error) {
	url := endpointURL(a.inf, input)
	if url == "" {
		return nil, fmt.Errorf("cannot build endpoint URL: inference.base_url or InferenceURL is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, input.Body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	// Set only when the size is known. The production S3 layer returns -1 for
	// encrypted objects, so raw requests are normally chunked, which
	// OpenAI-compatible backends accept.
	if input.Size > 0 {
		req.ContentLength = input.Size
	}
	ct := input.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	req.Header.Set("Content-Type", ct)
	if a.inf.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.inf.APIKey)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling inference endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("inference endpoint returned %d: %s", resp.StatusCode, body)
	}

	return body, nil
}
