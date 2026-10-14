package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"gatewai/gateway/internal/metrics"
)

// QuotaResetter is implemented by *ratelimit.Limiter.
type QuotaResetter interface {
	ResetQuota(ctx context.Context, consumer, serviceType, model string) (int, error)
}

// QuotaHandler serves POST /-/quota/reset.
type QuotaHandler struct {
	resetter QuotaResetter
}

// NewQuotaHandler returns a configured QuotaHandler.
func NewQuotaHandler(resetter QuotaResetter) *QuotaHandler {
	return &QuotaHandler{resetter: resetter}
}

// ResetQuota handles POST /-/quota/reset.
// Deletes the rate-limit and token-budget Redis keys for one consumer/service_type
// pair (across all user types), so the next request starts with a fresh window.
// Restricted to the /-/ admin namespace; caller is responsible for upstream auth.
//
// Query params:
//
//	consumer (required) – consumer identifier (as set by consumer_header)
//	type     (required) – service type, e.g. "audio", "llm"
//	model    (optional) – also reset this consumer's per-model token budget
//	                       (trl:{consumer}:model:{model}:*), which is keyed by
//	                       model rather than service type and is otherwise
//	                       untouched by a reset
func (h *QuotaHandler) ResetQuota(w http.ResponseWriter, r *http.Request) {
	consumer := r.URL.Query().Get("consumer")
	serviceType := r.URL.Query().Get("type")
	model := r.URL.Query().Get("model")
	if consumer == "" {
		writeError(w, http.StatusBadRequest, "query param 'consumer' is required")
		return
	}
	if serviceType == "" {
		writeError(w, http.StatusBadRequest, "query param 'type' is required")
		return
	}

	deleted, err := h.resetter.ResetQuota(r.Context(), consumer, serviceType, model)
	if err != nil {
		slog.ErrorContext(r.Context(), "admin quota reset failed", "consumer", consumer, "type", serviceType, "model", model, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to reset quota")
		return
	}

	metrics.QuotaResetsTotal.WithLabelValues(serviceType).Inc()
	slog.InfoContext(r.Context(), "admin quota reset", "consumer", consumer, "type", serviceType, "model", model, "deleted_keys", deleted)

	resp := map[string]any{
		"consumer":     consumer,
		"type":         serviceType,
		"deleted_keys": deleted,
	}
	if model != "" {
		resp["model"] = model
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}
