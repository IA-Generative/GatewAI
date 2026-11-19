package inputadapter

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

type out struct {
	Model    string `json:"model"`
	MaxTok   *int   `json:"max_tokens"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	} `json:"messages"`
}

func run(t *testing.T, c Config, file []byte, ct string, params map[string]string) (out, map[string]any) {
	t.Helper()
	b, err := c.Transform(file, ct, "m", params)
	if err != nil {
		t.Fatal(err)
	}
	var o out
	var raw map[string]any
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &raw)
	return o, raw
}

func TestDataURLRoundTrip(t *testing.T) {
	file := []byte{0, 1, 2, 250, 255}
	o, _ := run(t, Config{DefaultPrompt: "p"}, file, "image/png", nil)
	url := o.Messages[0].Content[1].ImageURL.URL
	const pre = "data:image/png;base64,"
	if !strings.HasPrefix(url, pre) {
		t.Fatalf("bad url %q", url)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, pre))
	if err != nil || string(dec) != string(file) {
		t.Fatalf("roundtrip mismatch: %v %v", dec, err)
	}
	if o.Model != "m" || o.Messages[0].Role != "user" || o.Messages[0].Content[0].Text != "p" {
		t.Fatalf("unexpected body %+v", o)
	}
}

func TestEmptyContentType(t *testing.T) {
	o, _ := run(t, Config{}, []byte("x"), "", nil)
	if !strings.HasPrefix(o.Messages[0].Content[1].ImageURL.URL, "data:application/octet-stream;base64,") {
		t.Fatal("expected octet-stream default")
	}
}

func TestPromptFallback(t *testing.T) {
	c := Config{PromptField: "prompt", DefaultPrompt: "def"}
	for _, tc := range []struct {
		params map[string]string
		want   string
	}{
		{nil, "def"},
		{map[string]string{"prompt": ""}, "def"},
		{map[string]string{"prompt": "hi"}, "hi"},
		{map[string]string{"other": "hi"}, "def"},
	} {
		o, _ := run(t, c, nil, "image/png", tc.params)
		if got := o.Messages[0].Content[0].Text; got != tc.want {
			t.Errorf("params %v: got %q want %q", tc.params, got, tc.want)
		}
	}
}

func TestPromptLastOrder(t *testing.T) {
	for _, tc := range []struct {
		promptLast bool
		want       [2]string
	}{
		{false, [2]string{"text", "image_url"}},
		{true, [2]string{"image_url", "text"}},
	} {
		o, _ := run(t, Config{DefaultPrompt: "p", PromptLast: tc.promptLast}, []byte("x"), "image/png", nil)
		content := o.Messages[0].Content
		if len(content) != 2 || content[0].Type != tc.want[0] || content[1].Type != tc.want[1] {
			t.Errorf("prompt_last=%v: got %+v want order %v", tc.promptLast, content, tc.want)
		}
	}
}

func TestMaxTokens(t *testing.T) {
	c := Config{MaxTokensField: "max_tokens", MaxTokensDefault: 100, MaxTokensCap: 500}
	for _, tc := range []struct {
		v    string
		want int
	}{
		{"", 100}, {"abc", 100}, {"-5", 100}, {"0", 100}, {"200", 200}, {"9999", 500},
	} {
		o, _ := run(t, c, nil, "image/png", map[string]string{"max_tokens": tc.v})
		if o.MaxTok == nil || *o.MaxTok != tc.want {
			t.Errorf("%q: got %v want %d", tc.v, o.MaxTok, tc.want)
		}
	}
	// default above cap is clamped; no cap leaves value alone; zero default omits.
	o, _ := run(t, Config{MaxTokensDefault: 900, MaxTokensCap: 500}, nil, "x", nil)
	if *o.MaxTok != 500 {
		t.Errorf("default not clamped: %d", *o.MaxTok)
	}
	o, _ = run(t, Config{}, nil, "x", nil)
	if o.MaxTok != nil {
		t.Errorf("max_tokens should be omitted when unset")
	}
}

func TestExtraFields(t *testing.T) {
	c := Config{ExtraJSONFields: map[string]any{
		"temperature": 0, "skip_special_tokens": false,
		"model": "evil", "messages": "evil",
	}}
	o, raw := run(t, c, []byte("x"), "image/png", nil)
	if raw["temperature"] != float64(0) || raw["skip_special_tokens"] != false {
		t.Fatalf("extras not merged: %v", raw)
	}
	if o.Model != "m" || len(o.Messages) != 1 {
		t.Fatalf("model/messages clobbered: %+v", o)
	}
}

func TestValidate(t *testing.T) {
	if err := (&Config{Type: TypeMultipartToChat}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (&Config{Type: "nope"}).Validate() == nil || (&Config{}).Validate() == nil {
		t.Fatal("expected error")
	}
}
