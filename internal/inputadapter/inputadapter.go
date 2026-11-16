// Package inputadapter turns a multipart file upload into the request body a
// backend actually speaks, so uploads reuse the existing JSON/provider pipeline.
package inputadapter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// TypeMultipartToChat wraps the uploaded file as a base64 data: URL in an
// OpenAI chat-completions request.
const TypeMultipartToChat = "multipart_to_chat"

// ChatCompletionsSuffix is the operation path an adapted service must expose.
const ChatCompletionsSuffix = "/v1/chat/completions"

// Config is the per-service input_adapter block.
type Config struct {
	Type             string         `yaml:"type"`
	PromptField      string         `yaml:"prompt_field"`
	DefaultPrompt    string         `yaml:"default_prompt"`
	MaxTokensField   string         `yaml:"max_tokens_field"`
	MaxTokensDefault int            `yaml:"max_tokens_default"`
	MaxTokensCap     int            `yaml:"max_tokens_cap"`
	ExtraJSONFields  map[string]any `yaml:"extra_json_fields"`
}

// Validate rejects unknown adapter types.
func (c *Config) Validate() error {
	switch c.Type {
	case TypeMultipartToChat:
		return nil
	case "":
		return fmt.Errorf("input_adapter.type is required (valid: %s)", TypeMultipartToChat)
	default:
		return fmt.Errorf("input_adapter.type %q is invalid (valid: %s)", c.Type, TypeMultipartToChat)
	}
}

// Transform builds the chat-completions JSON body for an uploaded file.
// params are the extra multipart form values. extra_json_fields never override
// "model" or "messages".
func (c *Config) Transform(fileBytes []byte, contentType, model string, params map[string]string) ([]byte, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	prompt := c.DefaultPrompt
	if c.PromptField != "" {
		if v := params[c.PromptField]; v != "" {
			prompt = v
		}
	}

	maxTokens := c.MaxTokensDefault
	if c.MaxTokensField != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(params[c.MaxTokensField])); err == nil && n > 0 {
			maxTokens = n
		}
	}
	if c.MaxTokensCap > 0 && maxTokens > c.MaxTokensCap {
		maxTokens = c.MaxTokensCap
	}

	body := make(map[string]any, len(c.ExtraJSONFields)+3)
	for k, v := range c.ExtraJSONFields {
		body[k] = v
	}
	body["model"] = model
	body["messages"] = []any{map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": prompt},
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(fileBytes),
			}},
		},
	}}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	return json.Marshal(body)
}
