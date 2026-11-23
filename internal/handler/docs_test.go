package handler

import (
	"testing"

	"gopkg.in/yaml.v3"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/inputadapter"
	"gatewai/gateway/internal/service"
)

func specPaths(t *testing.T, spec []byte) map[string]any {
	t.Helper()
	var s map[string]any
	if err := yaml.Unmarshal(spec, &s); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	paths, _ := s["paths"].(map[string]any)
	return paths
}

func opField(paths map[string]any, path, method, field string) any {
	item, _ := paths[path].(map[string]any)
	op, _ := item[method].(map[string]any)
	return op[field]
}

// syncPathItems isn't currently wired into GenerateSpec's output (sync
// endpoints are documented per-model via the swagger overlay instead), so
// these exercise the helper directly rather than through the full spec.

func TestSyncPathItems_AllModelsDeprecated_MarksOperation(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "reranker",
		Model:        "reranker-v1",
		Deprecated:   true,
		InferenceURL: "http://reranker",
		Operations:   map[string][]string{"rerank": {"/v1/rerank"}},
	}})

	items := syncPathItems(reg)
	item, _ := items["/v1/rerank"].(map[string]any)
	op, _ := item["post"].(map[string]any)
	if op["deprecated"] != true {
		t.Errorf("expected deprecated=true on POST /v1/rerank, got %v", op["deprecated"])
	}
}

func TestSyncPathItems_PartiallyDeprecated_DoesNotMarkOperation(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:         "reranker",
			Model:        "reranker-v1",
			Deprecated:   true,
			InferenceURL: "http://reranker",
			Operations:   map[string][]string{"rerank": {"/v1/rerank"}},
		},
		{
			Type:         "reranker",
			Model:        "reranker-v2",
			InferenceURL: "http://reranker",
			Operations:   map[string][]string{"rerank": {"/v1/rerank"}},
		},
	})

	items := syncPathItems(reg)
	item, _ := items["/v1/rerank"].(map[string]any)
	op, _ := item["post"].(map[string]any)
	if _, ok := op["deprecated"]; ok {
		t.Error("expected no deprecated field when only some models on the shared path are deprecated")
	}
}

func TestGenerateSpec_SubmitJob_AnnotatesDeprecatedModelInDescription(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:         "audio",
			Model:        "whisper-large-v2",
			Deprecated:   true,
			AcceptedExts: []string{".mp3"},
			Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		},
		{
			Type:         "audio",
			Model:        "whisper-large-v3",
			AcceptedExts: []string{".mp3"},
			Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		},
	})

	spec := GenerateSpec(reg, "test", false)
	paths := specPaths(t, spec)

	item, _ := paths["/jobs/{service_type}"].(map[string]any)
	post, _ := item["post"].(map[string]any)
	rb, _ := post["requestBody"].(map[string]any)
	content, _ := rb["content"].(map[string]any)
	mp, _ := content["multipart/form-data"].(map[string]any)
	schema, _ := mp["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	modelField, _ := props["model"].(map[string]any)
	desc, _ := modelField["description"].(string)

	if !containsStr(desc, "whisper-large-v2 (deprecated)") {
		t.Errorf("expected model description to flag whisper-large-v2 as deprecated, got: %s", desc)
	}
	if containsStr(desc, "whisper-large-v3 (deprecated)") {
		t.Errorf("did not expect whisper-large-v3 to be flagged deprecated, got: %s", desc)
	}
}

func TestGenerateSpec_SubmitJob_DocumentsAdapterFields(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:         "llm",
			Model:        "lighton-ocr",
			Provider:     "passthrough",
			InferenceURL: "http://lighton",
			AcceptedExts: []string{".pdf", ".png"},
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
			InputAdapter: &inputadapter.Config{
				Type:             inputadapter.TypeMultipartToChat,
				PromptField:      "prompt",
				DefaultPrompt:    "grounding",
				MaxTokensField:   "max_tokens",
				MaxTokensDefault: 4096,
				MaxTokensCap:     8192,
			},
		},
	})

	spec := GenerateSpec(reg, "test", false)
	paths := specPaths(t, spec)

	item, _ := paths["/jobs/{service_type}"].(map[string]any)
	post, _ := item["post"].(map[string]any)
	rb, _ := post["requestBody"].(map[string]any)
	content, _ := rb["content"].(map[string]any)
	mp, _ := content["multipart/form-data"].(map[string]any)
	schema, _ := mp["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)

	promptField, ok := props["prompt"].(map[string]any)
	if !ok {
		t.Fatalf("expected a 'prompt' form field in the async submit body, got props: %v", props)
	}
	if pdesc, _ := promptField["description"].(string); !containsStr(pdesc, "grounding") || !containsStr(pdesc, "lighton-ocr") {
		t.Errorf("prompt description should name the model and its default, got: %s", pdesc)
	}
	maxTok, ok := props["max_tokens"].(map[string]any)
	if !ok {
		t.Fatalf("expected a 'max_tokens' form field in the async submit body")
	}
	if maxTok["type"] != "integer" {
		t.Errorf("max_tokens should be an integer field, got type %v", maxTok["type"])
	}
}

func TestSyncPathItems_DocumentsAdapterFields(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{{
		Type:         "llm",
		Model:        "lighton-ocr",
		Provider:     "passthrough",
		InferenceURL: "http://lighton",
		AcceptedExts: []string{".pdf"},
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		InputAdapter: &inputadapter.Config{
			Type:          inputadapter.TypeMultipartToChat,
			PromptField:   "prompt",
			DefaultPrompt: "grounding",
		},
	}})

	items := syncPathItems(reg)
	item, _ := items["/v1/chat/completions"].(map[string]any)
	post, _ := item["post"].(map[string]any)
	rb, _ := post["requestBody"].(map[string]any)
	content, _ := rb["content"].(map[string]any)
	mp, _ := content["multipart/form-data"].(map[string]any)
	schema, _ := mp["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)

	if _, ok := props["prompt"].(map[string]any); !ok {
		t.Errorf("expected a 'prompt' form field on sync POST /v1/chat/completions, got props: %v", props)
	}
}
