package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/service"
)

// probeVia is a test helper that creates a minimal Checker and probes d directly.
func probeVia(t *testing.T, d *service.Def, defaultTimeout string) string {
	t.Helper()
	cfg := config.HealthConfig{Timeout: defaultTimeout}
	c := &Checker{
		defaultTimeout: cfg.TimeoutDuration(),
		httpClient:     &http.Client{},
	}
	if c.defaultTimeout <= 0 {
		c.defaultTimeout = 5e9 // 5s
	}
	return c.probe(t.Context(), d)
}

func def(inferenceURL string, healthCfg config.ServiceHealthConfig) *service.Def {
	return &service.Def{
		Type:         "audio",
		Model:        "test-model",
		InferenceURL: inferenceURL,
		HealthCheck:  healthCfg,
	}
}

func TestProbe_Up_2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got := probeVia(t, def(srv.URL, config.ServiceHealthConfig{}), "5s")
	if got != "up" {
		t.Errorf("expected up, got %s", got)
	}
}

func TestProbe_Method_Override(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got := probeVia(t, def(srv.URL, config.ServiceHealthConfig{Method: "HEAD"}), "5s")
	if got != "up" {
		t.Errorf("expected up, got %s", got)
	}
}

func TestProbe_Down_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	got := probeVia(t, def(srv.URL, config.ServiceHealthConfig{}), "5s")
	if got != "down" {
		t.Errorf("expected down, got %s", got)
	}
}

func TestProbe_Down_ConnectionRefused(t *testing.T) {
	got := probeVia(t, def("http://127.0.0.1:1", config.ServiceHealthConfig{}), "200ms")
	if got != "down" {
		t.Errorf("expected down, got %s", got)
	}
}

func TestProbe_Up_4xx(t *testing.T) {
	// 404 means the backend is responding — treat as "up".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got := probeVia(t, def(srv.URL, config.ServiceHealthConfig{}), "5s")
	if got != "up" {
		t.Errorf("expected up for 404, got %s", got)
	}
}

func TestProbe_CustomPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svcDef := def(srv.URL, config.ServiceHealthConfig{Path: "/readyz"})
	probeVia(t, svcDef, "5s")

	if gotPath != "/readyz" {
		t.Errorf("expected /readyz, got %s", gotPath)
	}
}

func TestProbe_ServiceTimeout_Overrides_Default(t *testing.T) {
	// Backend responds after 300ms; service timeout is 50ms → should time out → "down".
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer slow.Close()

	svcDef := def(slow.URL, config.ServiceHealthConfig{Timeout: "50ms"})
	got := probeVia(t, svcDef, "10s") // global default is generous
	if got != "down" {
		t.Errorf("expected down on timeout, got %s", got)
	}
}

func TestProbeURL_DefaultPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probeVia(t, def(srv.URL, config.ServiceHealthConfig{}), "5s")
	if gotPath != "/health" {
		t.Errorf("expected /health, got %s", gotPath)
	}
}

func TestProbe_ScaleToZero_NeverProbed(t *testing.T) {
	probed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &service.Def{
		Type:         "llm",
		Model:        "kserve-model",
		InferenceURL: srv.URL,
		HealthCheck:  config.ServiceHealthConfig{ScaleToZero: true},
	}
	got := probeVia(t, d, "5s")
	if probed {
		t.Error("scale_to_zero: backend should not have been probed")
	}
	if got != "dormant" {
		t.Errorf("expected dormant, got %s", got)
	}
}

func TestProbe_InferenceHeaders_Sent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &service.Def{
		Type:             "llm",
		Model:            "test",
		InferenceURL:     srv.URL,
		InferenceHeaders: map[string]string{"Authorization": "Bearer svc-token"},
		HealthCheck:      config.ServiceHealthConfig{},
	}
	probeVia(t, d, "5s")
	if gotAuth != "Bearer svc-token" {
		t.Errorf("expected inference header Authorization=Bearer svc-token, got %q", gotAuth)
	}
}

func TestProbe_BackendHeaders_Sent(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &service.Def{
		Type:         "llm",
		Model:        "test",
		InferenceURL: srv.URL,
		Backends: []service.Backend{
			{URL: srv.URL, Weight: 1, Headers: map[string]string{"X-Api-Key": "backend-key"}},
		},
		HealthCheck: config.ServiceHealthConfig{},
	}
	probeVia(t, d, "5s")
	if gotKey != "backend-key" {
		t.Errorf("expected X-Api-Key=backend-key, got %q", gotKey)
	}
}

func TestProbe_HealthHeaders_Override(t *testing.T) {
	var gotAuth, gotExtra string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotExtra = r.Header.Get("X-Health-Check")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &service.Def{
		Type:             "llm",
		Model:            "test",
		InferenceURL:     srv.URL,
		InferenceHeaders: map[string]string{"Authorization": "Bearer svc-token"},
		HealthCheck: config.ServiceHealthConfig{
			// Override the service-level Authorization and add an extra header.
			Headers: map[string]string{
				"Authorization":  "Bearer health-token",
				"X-Health-Check": "true",
			},
		},
	}
	probeVia(t, d, "5s")
	if gotAuth != "Bearer health-token" {
		t.Errorf("expected health-check header to override: got Authorization=%q", gotAuth)
	}
	if gotExtra != "true" {
		t.Errorf("expected X-Health-Check=true, got %q", gotExtra)
	}
}

func TestProbeURL_TrailingSlashInInferenceURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// InferenceURL has a trailing slash — should not produce double slash.
	svcDef := def(srv.URL+"/", config.ServiceHealthConfig{})
	probeVia(t, svcDef, "5s")
	if strings.Contains(gotPath, "//") {
		t.Errorf("double slash in probe URL: %s", gotPath)
	}
}
