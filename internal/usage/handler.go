package usage

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"gatewai/gateway/internal/service"
)

// reportDateLayout is the accepted format for the from/to query params on
// GET /-/usage/report ("2006-01-02", interpreted as UTC).
const reportDateLayout = "2006-01-02"

// UsageHandler serves GET /usage (consumer) and GET /-/usage (admin).
type UsageHandler struct {
	store          UsageStore
	reg            *service.Registry // may be nil in tests
	consumerHeader string
	userTypeHeader string
}

// NewUsageHandler returns a configured UsageHandler.
func NewUsageHandler(store UsageStore, reg *service.Registry, consumerHeader, userTypeHeader string) *UsageHandler {
	return &UsageHandler{
		store:          store,
		reg:            reg,
		consumerHeader: consumerHeader,
		userTypeHeader: userTypeHeader,
	}
}

// UpdateRegistry swaps the service registry atomically (called on hot-reload).
func (h *UsageHandler) UpdateRegistry(reg *service.Registry) {
	h.reg = reg
}

// GetMyUsage handles GET /usage.
// Requires consumer_header to be configured; returns 501 otherwise.
// Returns 400 if the header is missing from the request.
func (h *UsageHandler) GetMyUsage(w http.ResponseWriter, r *http.Request) {
	if h.consumerHeader == "" {
		writeJSONError(w, http.StatusNotImplemented, "GET /usage requires consumer_header to be configured")
		return
	}
	consumer := r.Header.Get(h.consumerHeader)
	if consumer == "" {
		writeJSONError(w, http.StatusBadRequest, "missing consumer header: "+h.consumerHeader)
		return
	}
	userType := "*"
	if h.userTypeHeader != "" {
		if v := r.Header.Get(h.userTypeHeader); v != "" {
			userType = v
		}
	}

	result, err := h.store.GetConsumerUsage(r.Context(), consumer, userType, h.serviceTypes(), h.modelsByType())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to retrieve usage")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// AdminListUsage handles GET /-/usage.
// Query params: consumer, type, limit (default 20, max 100), offset (default 0).
func (h *UsageHandler) AdminListUsage(w http.ResponseWriter, r *http.Request) {
	limit := int64(20)
	offset := int64(0)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			offset = n
		}
	}

	filterConsumer := r.URL.Query().Get("consumer")
	filterType := r.URL.Query().Get("type")

	userType := "*"
	if h.userTypeHeader != "" {
		if v := r.Header.Get(h.userTypeHeader); v != "" {
			userType = v
		}
	}

	var consumers []string
	var total int64

	switch {
	case filterConsumer != "":
		consumers = []string{filterConsumer}
		total = 1
	case filterType != "":
		var err error
		consumers, total, err = h.store.ListConsumersByType(r.Context(), filterType, limit, offset)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to list consumers")
			return
		}
	default:
		var err error
		consumers, total, err = h.store.ListConsumers(r.Context(), limit, offset)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to list consumers")
			return
		}
	}

	serviceTypes := h.serviceTypes()
	if filterType != "" {
		serviceTypes = []string{filterType}
	}

	result := AdminUsageResponse{
		Total:     total,
		Limit:     limit,
		Offset:    offset,
		Consumers: make([]ConsumerUsage, 0, len(consumers)),
	}
	modelsByType := h.modelsByType()
	for _, consumer := range consumers {
		cu, err := h.store.GetConsumerUsage(r.Context(), consumer, userType, serviceTypes, modelsByType)
		if err != nil {
			continue
		}
		result.Consumers = append(result.Consumers, *cu)
	}

	writeJSON(w, http.StatusOK, result)
}

// AdminUsageReport handles GET /-/usage/report: cross-consumer calendar-aligned
// totals for one service type, for finance/BI reporting.
// Query params: type, period (daily|weekly|monthly), from, to (YYYY-MM-DD,
// UTC, inclusive) are all required; total=true additionally sums every
// bucket into the response's top-level "total" field.
func (h *UsageHandler) AdminUsageReport(w http.ResponseWriter, r *http.Request) {
	svcType := r.URL.Query().Get("type")
	if svcType == "" {
		writeJSONError(w, http.StatusBadRequest, "missing required query param: type")
		return
	}

	period := r.URL.Query().Get("period")
	if !isValidPeriod(period) {
		writeJSONError(w, http.StatusBadRequest, "invalid period: must be one of daily, weekly, monthly")
		return
	}

	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	from, err := time.Parse(reportDateLayout, fromStr)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid from: expected YYYY-MM-DD")
		return
	}
	to, err := time.Parse(reportDateLayout, toStr)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid to: expected YYYY-MM-DD")
		return
	}
	if to.Before(from) {
		writeJSONError(w, http.StatusBadRequest, "to must not be before from")
		return
	}

	result, err := h.store.GetUsageReport(r.Context(), svcType, period, from, to)
	if err != nil {
		if errors.Is(err, ErrTooManyBuckets) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to retrieve usage report")
		return
	}

	if r.URL.Query().Get("total") == "true" {
		result.Total = sumBuckets(result.Buckets)
	}

	writeJSON(w, http.StatusOK, result)
}

// sumBuckets adds up every PeriodUsage bucket into a single TotalUsage.
// Tokens stays nil if no bucket carried any (mirrors PeriodUsage's own
// omit-when-zero convention).
func sumBuckets(buckets []PeriodUsage) *TotalUsage {
	total := &TotalUsage{}
	var tokens TokenUsage
	haveTokens := false
	for _, b := range buckets {
		total.Requests += b.Requests
		total.Jobs += b.Jobs
		total.ProcessingTime += b.ProcessingTime
		if b.Tokens != nil {
			haveTokens = true
			tokens.Prompt += b.Tokens.Prompt
			tokens.Completion += b.Tokens.Completion
		}
	}
	if haveTokens {
		total.Tokens = &tokens
	}
	return total
}

func (h *UsageHandler) serviceTypes() []string {
	if h.reg == nil {
		return nil
	}
	return h.reg.Types()
}

// modelsByType builds a service_type → model names map from the registry,
// used to surface a per-model token-quota breakdown in usage responses.
func (h *UsageHandler) modelsByType() map[string][]string {
	if h.reg == nil {
		return nil
	}
	budgetKeys := h.reg.ModelBudgetKeys()
	out := make(map[string][]string)
	for _, t := range h.reg.Types() {
		seen := make(map[string]bool) // one row per budget key — grouped aliases collapse
		for _, m := range h.reg.ModelsForType(t) {
			key := budgetKeys[m]
			if key == "" {
				key = m
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out[t] = append(out[t], m)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
