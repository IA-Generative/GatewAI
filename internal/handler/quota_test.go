package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gatewai/gateway/internal/handler"
)

type capturingResetter struct{ model string }

func (c *capturingResetter) ResetQuota(_ context.Context, _, _, model string) (int, error) {
	c.model = model
	return 1, nil
}

func resetReq(t *testing.T, h *handler.QuotaHandler, query string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/-/quota/reset?"+query, nil)
	h.ResetQuota(httptest.NewRecorder(), req)
}

// A grouped alias resolves to its shared budget key so the right counter is cleared.
func TestQuotaHandler_ResetResolvesBudgetGroup(t *testing.T) {
	cr := &capturingResetter{}
	h := handler.NewQuotaHandler(cr, map[string]string{"code": "qwen", "code-completion": "qwen"})

	resetReq(t, h, "consumer=alice&type=llm&model=code")
	if cr.model != "qwen" {
		t.Errorf("grouped alias: resetter got model %q, want qwen", cr.model)
	}

	// Ungrouped model is passed through unchanged.
	resetReq(t, h, "consumer=alice&type=llm&model=chat")
	if cr.model != "chat" {
		t.Errorf("ungrouped: resetter got model %q, want chat", cr.model)
	}

	// Nil budget-key map (default deployments) → unchanged.
	h2 := handler.NewQuotaHandler(cr, nil)
	resetReq(t, h2, "consumer=alice&type=llm&model=code")
	if cr.model != "code" {
		t.Errorf("nil budgetKeys: resetter got model %q, want code", cr.model)
	}
}
