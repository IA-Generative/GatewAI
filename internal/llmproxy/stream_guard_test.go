package llmproxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gatewai/gateway/internal/guardrails"
)

func feed(g *streamGuard, lines []string) {
	for _, l := range lines {
		if !g.line(l) {
			break
		}
	}
	g.finish()
}

func TestStreamGuard_RedactsEmailAcrossDeltas(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 64, "")
	feed(g, []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`, ``,
		`data: {"choices":[{"delta":{"content":"contact "}}]}`, ``,
		`data: {"choices":[{"delta":{"content":"bob@"}}]}`, ``,
		`data: {"choices":[{"delta":{"content":"example.com"}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if strings.Contains(out, "bob@example.com") {
		t.Errorf("email should have been redacted in stream: %s", out)
	}
	if !g.violated {
		t.Error("violated should be true after redacting a match")
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("[DONE] should pass through: %s", out)
	}
}

func TestStreamGuard_BlocksOnViolation(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, false, 64, "")
	feed(g, []string{
		`data: {"choices":[{"delta":{"content":"reach me at bob@example.com now"}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if !g.aborted {
		t.Error("stream should have aborted on a block violation")
	}
	if !strings.Contains(out, "blocked by guardrails") {
		t.Errorf("expected a block error event: %s", out)
	}
	if strings.Contains(out, "bob@example.com") {
		t.Errorf("blocked content must not be forwarded: %s", out)
	}
}

func TestStreamGuard_CleanContentPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 64, "")
	feed(g, []string{
		`data: {"choices":[{"delta":{"content":"the weather is nice today"}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if g.violated {
		t.Error("clean content should not be flagged")
	}
	if !strings.Contains(out, "the weather is nice today") {
		t.Errorf("clean content should be forwarded: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("[DONE] should pass through")
	}
}

func TestStreamGuard_WindowFlush(t *testing.T) {
	// A tiny window forces a flush mid-content (no control chunk needed).
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 1, "") // 1 token → 4 chars
	feed(g, []string{
		`data: {"choices":[{"delta":{"content":"hello world this is long"}}]}`, ``,
	})
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("windowed content should have been emitted: %s", buf.String())
	}
}

// chunkLines turns content parts into SSE content-delta lines (each followed by
// the blank separator), then a finish chunk and [DONE].
func chunkLines(parts ...string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, `data: {"id":"c1","model":"m","choices":[{"delta":{"content":"`+p+`"}}]}`, ``)
	}
	out = append(out, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``, `data: [DONE]`, ``)
	return out
}

// reassemble extracts and concatenates all delta.content from an SSE dump.
func reassemble(sse string) string {
	var b strings.Builder
	for _, ln := range strings.Split(sse, "\n") {
		s, ok := strings.CutPrefix(ln, "data: ")
		if !ok || s == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(s), &c) == nil && len(c.Choices) > 0 {
			b.WriteString(c.Choices[0].Delta.Content)
		}
	}
	return b.String()
}

// emailByChar returns the email split into one-character deltas — the pathological
// streaming case where a match arrives token-by-token and straddles boundaries.
func emailByChar() []string {
	var cs []string
	for _, r := range "bob@example.com" {
		cs = append(cs, string(r))
	}
	return cs
}

func TestStreamGuard_RedactStraddlesWindowBoundary(t *testing.T) {
	// Window (8 tokens = 32 chars) comfortably exceeds the 15-char email, but the
	// email is fed one char at a time between two 40-char fillers, so it straddles
	// an incremental release boundary. It must be fully redacted, never leaked.
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 8, "")
	parts := append([]string{strings.Repeat("a", 40) + " "}, emailByChar()...)
	parts = append(parts, " "+strings.Repeat("b", 40))
	feed(g, chunkLines(parts...))
	got := reassemble(buf.String())
	if strings.Contains(got, "bob@example.com") {
		t.Fatalf("email leaked across a window boundary: %q", got)
	}
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Fatalf("email not redacted: %q", got)
	}
	if !g.violated {
		t.Error("violated should be set")
	}
}

func TestStreamGuard_BlockStraddlesWindowBoundary(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, false, 8, "")
	parts := append([]string{strings.Repeat("a", 40) + " "}, emailByChar()...)
	parts = append(parts, " "+strings.Repeat("b", 40))
	feed(g, chunkLines(parts...))
	got := reassemble(buf.String())
	if !g.aborted {
		t.Error("block should abort on an email straddling a window boundary")
	}
	if strings.Contains(got, "bob@example.com") {
		t.Fatalf("blocked email leaked: %q", got)
	}
}

func TestStreamGuard_PreservesFinishReasonWithContent(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 64, "")
	// One chunk carrying BOTH content and finish_reason.
	feed(g, []string{
		`data: {"id":"c1","model":"m","choices":[{"delta":{"content":"all good"},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if !strings.Contains(reassemble(out), "all good") {
		t.Errorf("content lost: %q", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason must be preserved: %q", out)
	}
}

func TestStreamGuard_DataNoSpaceStillScanned(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 64, "")
	// "data:" with no following space must still be parsed + redacted.
	feed(g, []string{
		`data:{"choices":[{"delta":{"content":"mail bob@example.com"}}]}`,
		`data:[DONE]`,
	})
	out := buf.String()
	if strings.Contains(out, "bob@example.com") {
		t.Fatalf("no-space data: line bypassed scanning: %q", out)
	}
}

// TestStreamGuard_MultibyteNotSplitAcrossFlush drives multi-byte runes one byte
// at a time is impossible over JSON, so instead we push accented text through a
// tiny window that forces the release cut to land mid-string; the reassembled
// output must be valid UTF-8 (no U+FFFD from a rune split at the window edge).
func TestStreamGuard_MultibyteNotSplitAcrossFlush(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 2, "") // 2 tokens → 8 bytes
	text := "café déçu à Noël — garçon éphémère" // many 2-byte runes
	feed(g, []string{
		`data: {"choices":[{"delta":{"content":"` + text + `"}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	got := reassemble(buf.String())
	if got != text {
		t.Fatalf("multibyte content mangled across flush: got %q want %q", got, text)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("rune split at window boundary produced U+FFFD: %q", got)
	}
}

// TestStreamGuard_PreservesToolCallsWithContent ensures a delta carrying BOTH
// content and a sibling field (tool_calls) forwards the sibling — the content is
// buffered/scanned, the tool_calls chunk is emitted with content stripped.
func TestStreamGuard_PreservesToolCallsWithContent(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 64, "")
	feed(g, []string{
		`data: {"id":"c1","model":"m","choices":[{"delta":{"content":"hi","tool_calls":[{"index":0,"function":{"name":"f"}}]}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if !strings.Contains(reassemble(out), "hi") {
		t.Errorf("content lost: %q", out)
	}
	if !strings.Contains(out, `"tool_calls"`) || !strings.Contains(out, `"name":"f"`) {
		t.Errorf("tool_calls sibling must be preserved: %q", out)
	}
}

// TestStreamGuard_InterleavedControlChunksDoNotDefeatWindow reproduces the live
// failure: some backends (vLLM/OpenAI reasoning models) emit a content-less
// reasoning_content chunk after EVERY content token. Those must not flush the
// buffer, or a match split across content deltas (an email here) is released
// piecemeal and never redacted. Only a real finish_reason may flush.
func TestStreamGuard_InterleavedControlChunksDoNotDefeatWindow(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 24, "")
	// Email split across content deltas, each followed by a reasoning control chunk.
	pieces := []string{"mail ", "bob", "@", "exa", "mple", ".com", " now"}
	var lines []string
	// leading role chunk (content-less, with siblings)
	lines = append(lines, `data: {"choices":[{"delta":{"role":"assistant","content":"","reasoning_content":null},"index":0}]}`, ``)
	for _, p := range pieces {
		pb, _ := json.Marshal(p)
		lines = append(lines,
			`data: {"choices":[{"delta":{"content":`+string(pb)+`},"index":0}]}`, ``,
			`data: {"choices":[{"delta":{"reasoning_content":null},"finish_reason":null,"index":0}]}`, ``,
		)
	}
	lines = append(lines,
		`data: {"choices":[{"delta":{"content":"","reasoning_content":null},"finish_reason":"stop","index":0}]}`, ``,
		`data: [DONE]`, ``,
	)
	feed(g, lines)
	got := reassemble(buf.String())
	if strings.Contains(got, "bob@example.com") {
		t.Fatalf("email leaked despite interleaved control chunks: %q", got)
	}
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Fatalf("email not redacted: %q", got)
	}
	if !g.violated {
		t.Error("violated should be set")
	}
}

// TestStreamGuard_ArrayContentScanned ensures array-of-parts content
// (delta.content: [{type:text,text:…}]) is flattened and scanned, not passed
// through unscanned as if it were a control chunk.
func TestStreamGuard_ArrayContentScanned(t *testing.T) {
	var buf bytes.Buffer
	g := newStreamGuard(&buf, nil, guardrails.New(), []string{"pii"}, true, 64, "")
	feed(g, []string{
		`data: {"choices":[{"delta":{"content":[{"type":"text","text":"mail bob@example.com"}]}}]}`, ``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, ``,
		`data: [DONE]`, ``,
	})
	out := buf.String()
	if strings.Contains(out, "bob@example.com") {
		t.Fatalf("array-form content bypassed scanning: %q", out)
	}
}
