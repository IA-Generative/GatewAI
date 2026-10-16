package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/metrics"
	"gatewai/gateway/internal/service"
)

// callerAudience extracts the caller's user type and groups for model-visibility
// gating. user_type comes from the configured header (bridged by proxy/oauth2 auth
// and by upstream trust); groups come from the authenticated Principal.
func callerAudience(r *http.Request, userTypeHeader string) (userType string, groups []string) {
	if userTypeHeader != "" {
		userType = r.Header.Get(userTypeHeader)
	}
	if p, _ := auth.FromContext(r.Context()); p != nil {
		if userType == "" {
			userType = p.UserType
		}
		groups = p.Groups
	}
	return userType, groups
}

// checkModelVisible enforces model visibility on the request path. Returns false
// (and writes 404) when the model is gated to an audience the caller isn't in —
// a hidden model must be indistinguishable from a non-existent one.
func checkModelVisible(w http.ResponseWriter, r *http.Request, def *service.Def, userTypeHeader string) bool {
	if !def.IsRestricted() {
		return true
	}
	userType, groups := callerAudience(r, userTypeHeader)
	if def.VisibleTo(userType, groups) {
		return true
	}
	metrics.ModelHiddenTotal.WithLabelValues(def.Type, def.Model).Inc()
	writeError(w, http.StatusNotFound, "model not found")
	return false
}

var modelsProxyClient = &http.Client{Timeout: 30 * time.Second}

// BackendHealth reports per-backend circuit state so GET /v1/models can mark a
// model whose backends are all unavailable as degraded. Satisfied by the LLM
// proxy's circuit breaker. A nil BackendHealth means no health info — nothing is
// marked degraded.
type BackendHealth interface {
	IsOpen(url string) bool
}

// backendsDegraded reports whether every backend of a model has an open circuit
// (so requests would fast-fail). False when there is no health source or the
// model has no backends.
//
// Deliberately circuit-breaker-only: backend_pools rate-limit/concurrency
// exhaustion is transient backpressure (the pool recovers on its own as the
// window rolls over or in-flight requests complete), not a degraded backend,
// so it is not reflected here.
func backendsDegraded(health BackendHealth, backends []service.Backend) bool {
	if health == nil || len(backends) == 0 {
		return false
	}
	for _, b := range backends {
		if !health.IsOpen(b.URL) {
			return false
		}
	}
	return true
}

// modelCapabilities describes what a model supports.
type modelCapabilities struct {
	SupportsAsync     bool     `json:"supports_async"`
	SupportsSync      bool     `json:"supports_sync"`
	SupportsStreaming bool     `json:"supports_streaming"`
	AcceptedFormats   []string `json:"accepted_formats,omitempty"`
	MaxFileSizeMB     int64    `json:"max_file_size_mb,omitempty"`
	Operations        []string `json:"operations,omitempty"`
	Deprecated        bool     `json:"deprecated,omitempty"`
	// Degraded is true when every backend behind this model has an open circuit
	// (the model is currently failing fast). Omitted when healthy.
	Degraded bool `json:"degraded,omitempty"`
}

// modelObject mirrors the OpenAI model object returned by GET /v1/models,
// extended with GatewAI-specific capability metadata.
type modelObject struct {
	ID             string            `json:"id"`
	Object         string            `json:"object"`
	OwnedBy        string            `json:"owned_by"`
	ServiceType    string            `json:"service_type"`
	Provider       string            `json:"provider,omitempty"`
	BackendModel   string            `json:"backend_model,omitempty"`   // real model this alias forwards to
	CanonicalModel string            `json:"canonical_model,omitempty"` // version-pinned name reported back in responses (e.g. "code" → "code-1.0")
	Capabilities   modelCapabilities `json:"capabilities"`
}

type modelsListResponse struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// ListModels handles GET /v1/models.
// Without query params, returns all configured models in OpenAI-compatible format.
// With ?model=<name>, proxies to the underlying model backend to retrieve its native info.
func ListModels(registry *service.Registry, userTypeHeader string, health BackendHealth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if modelName := r.URL.Query().Get("model"); modelName != "" {
			proxyModelsToBackend(w, r, registry, modelName, userTypeHeader)
			return
		}

		userType, groups := callerAudience(r, userTypeHeader)
		defs := registry.Models()

		data := make([]modelObject, 0, len(defs))
		for _, d := range defs {
			if !d.VisibleTo(userType, groups) {
				continue // model gated to another audience — hide it entirely
			}
			mo := modelObject{
				ID:          d.Model,
				Object:      "model",
				OwnedBy:     "gatewai",
				ServiceType: d.Type,
				Provider:    d.Provider,
				Capabilities: modelCapabilities{
					SupportsAsync:     d.SupportsAsync,
					SupportsSync:      len(d.Backends) > 0 || d.InferenceURL != "",
					SupportsStreaming: d.Provider != "",
					AcceptedFormats:   sortedExts(d.AcceptedExts),
					MaxFileSizeMB:     d.MaxFileSizeMB,
					Operations:        sortedKeys(d.Operations),
					Deprecated:        d.Deprecated,
					Degraded:          backendsDegraded(health, d.Backends),
				},
			}
			if d.BackendModel != "" {
				mo.BackendModel = d.BackendModel
			}
			if d.CanonicalModel != "" {
				mo.CanonicalModel = d.CanonicalModel
			}
			data = append(data, mo)
		}
		sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(modelsListResponse{Object: "list", Data: data})
	}
}

func proxyModelsToBackend(w http.ResponseWriter, r *http.Request, reg *service.Registry, modelName, userTypeHeader string) {
	var found *service.Def
	for _, d := range reg.Models() {
		if d.Model == modelName && d.InferenceURL != "" {
			found = d
			break
		}
	}
	if found == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"model not found or has no backend"}`))
		return
	}
	if !checkModelVisible(w, r, found, userTypeHeader) {
		return // hidden model — 404, indistinguishable from non-existent
	}

	u, err := url.Parse(found.InferenceURL)
	if err != nil || u.Host == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"invalid backend URL"}`))
		return
	}
	backendURL := u.Scheme + "://" + u.Host + "/v1/models"

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, backendURL, nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"failed to build backend request"}`))
		return
	}
	for k, v := range found.InferenceHeaders {
		req.Header.Set(k, v)
	}

	resp, err := modelsProxyClient.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"backend unreachable"}`))
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func sortedExts(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string][]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
