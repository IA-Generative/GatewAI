package service_test

import (
	"testing"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/guardrails"
	"gatewai/gateway/internal/service"
)

func baseServiceConfig() config.ServiceConfig {
	return config.ServiceConfig{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
			"translation":   {"/v1/audio/translations"},
		},
		InferenceURL:  "http://inference.svc.cluster.local",
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
	}
}

// TestRegistry_NotIndexedWithoutInferenceURL verifies that a service configured without
// inference_url is not indexed for sync routing (direct proxy path required).
func TestRegistry_NotIndexedWithoutInferenceURL(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.InferenceURL = "" // no direct proxy backend

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	if reg.HasSyncServices() {
		t.Error("service without inference_url should not be indexed for sync routing")
	}
}

// TestRegistry_ModelsForType verifies models are grouped by service type and
// that an unknown type returns an empty (not nil-panicking) slice.
func TestRegistry_ModelsForType(t *testing.T) {
	cfg1 := baseServiceConfig()
	cfg2 := baseServiceConfig()
	cfg2.Model = "whisper-tiny"

	reg := service.NewRegistry([]config.ServiceConfig{cfg1, cfg2})

	models := reg.ModelsForType("transcription")
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %v", models)
	}
	seen := map[string]bool{}
	for _, m := range models {
		seen[m] = true
	}
	if !seen["whisper-large-v3"] || !seen["whisper-tiny"] {
		t.Errorf("expected both models present, got %v", models)
	}

	if got := reg.ModelsForType("unknown"); len(got) != 0 {
		t.Errorf("expected no models for unknown type, got %v", got)
	}
}

// TestRegistry_NotIndexedWithoutModelOrTopic verifies that a service without a
// model is not indexed for sync routing.
func TestRegistry_NotIndexedWithoutModel(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Model = ""
	cfg.Operations = map[string][]string{"transcription": {"/v1/audio/transcriptions"}}

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	if reg.HasSyncServices() {
		t.Error("service without model should not be indexed for sync")
	}
}

// TestRouteSync_UnknownPathReturnsError verifies that an unknown path returns an error.
func TestRouteSync_UnknownPathReturnsError(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{baseServiceConfig()})

	_, err := reg.RouteSync("/v1/unknown", "whisper-large-v3")
	if err == nil {
		t.Error("expected error for unknown path")
	}
}

// TestRouteSync_UnknownModelReturnsError verifies that an unknown model returns an error.
func TestRouteSync_UnknownModelReturnsError(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{baseServiceConfig()})

	_, err := reg.RouteSync("/v1/audio/transcriptions", "unknown-model")
	if err == nil {
		t.Error("expected error for unknown model")
	}
}

// TestRegistry_MultipleServices verifies routing across two service types.
func TestRegistry_MultipleServices(t *testing.T) {
	cfgs := []config.ServiceConfig{
		baseServiceConfig(),
		{
			Type:  "ocr",
			Model: "llava-v1.6-mistral-7b",
			Operations: map[string][]string{
				"chat": {"/v1/chat/completions"},
			},
			InferenceURL: "http://ocr.svc.cluster.local",
		},
	}
	reg := service.NewRegistry(cfgs)

	def, err := reg.RouteSync("/v1/chat/completions", "llava-v1.6-mistral-7b")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}
	if def.Model != "llava-v1.6-mistral-7b" {
		t.Errorf("expected llava-v1.6-mistral-7b, got %q", def.Model)
	}

	def2, err := reg.RouteSync("/v1/audio/transcriptions", "whisper-large-v3")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}
	if def2.Model != "whisper-large-v3" {
		t.Errorf("expected whisper-large-v3, got %q", def2.Model)
	}
}

// TestRouteSync_PatternPath_ModelInURL verifies that a path pattern like
// "/v2/models/{model}/infer" routes correctly by extracting the model from the URL.
func TestRouteSync_PatternPath_ModelInURL(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Operations = map[string][]string{"infer": {"/v2/models/{model}/infer"}}

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	def, err := reg.RouteSync("/v2/models/whisper-large-v3/infer", "")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected model whisper-large-v3, got %q", def.Model)
	}
}

// TestRouteSync_PatternPath_SuffixSeparator verifies patterns like
// "/v1/models/{model}:predict" where the model is embedded with a suffix.
func TestRouteSync_PatternPath_SuffixSeparator(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Operations = map[string][]string{"predict": {"/v1/models/{model}:predict"}}

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	def, err := reg.RouteSync("/v1/models/whisper-large-v3:predict", "")
	if err != nil {
		t.Fatalf("RouteSync failed: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected model whisper-large-v3, got %q", def.Model)
	}
}

// TestRouteSync_PatternPath_UnknownModelReturnsError verifies that a pattern
// path with an unregistered model name returns an error.
func TestRouteSync_PatternPath_UnknownModelReturnsError(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Operations = map[string][]string{"infer": {"/v2/models/{model}/infer"}}

	reg := service.NewRegistry([]config.ServiceConfig{cfg})

	_, err := reg.RouteSync("/v2/models/unknown-model/infer", "")
	if err == nil {
		t.Error("expected error for unregistered model in pattern path")
	}
}

// TestSyncPathPrefixes verifies that SyncPathPrefixes returns unique prefixes
// for all registered paths (exact and pattern).
func TestSyncPathPrefixes(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{
			Type:  "transcription",
			Model: "whisper-large-v3",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
				"infer":         {"/v2/models/{model}/infer"},
			},
			InferenceURL: "http://inference.example.com",
		},
	}
	reg := service.NewRegistry(cfgs)

	prefixes := reg.SyncPathPrefixes()
	prefixSet := make(map[string]struct{}, len(prefixes))
	for _, p := range prefixes {
		prefixSet[p] = struct{}{}
	}

	if _, ok := prefixSet["/v1"]; !ok {
		t.Error("expected /v1 prefix")
	}
	if _, ok := prefixSet["/v2"]; !ok {
		t.Error("expected /v2 prefix")
	}
}

// TestRouteAsync_DefaultModel verifies that the default model is used when no
// model is specified and multiple models are configured for the type.
func TestRouteAsync_DefaultModel(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{
			Type:         "transcription",
			Model:        "whisper-large-v3",
			Default:      true,
			Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			InferenceURL: "http://whisper-large.svc",
		},
		{
			Type:         "transcription",
			Model:        "whisper-turbo",
			Default:      false,
			Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
			InferenceURL: "http://whisper-turbo.svc",
		},
	}
	reg := service.NewRegistry(cfgs)

	// Async: no model specified → default selected.
	def, err := reg.RouteAsync("transcription", "")
	if err != nil {
		t.Fatalf("RouteAsync with default model failed: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected default model whisper-large-v3, got %q", def.Model)
	}

	// Sync: no model in body → default selected.
	def, err = reg.RouteSync("/v1/audio/transcriptions", "")
	if err != nil {
		t.Fatalf("RouteSync with default model failed: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected default model whisper-large-v3 for sync, got %q", def.Model)
	}
}

// TestRouteAsync_NoDefaultMultipleModels verifies that omitting model without a
// default configured returns an error.
func TestRouteAsync_NoDefaultMultipleModels(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{
			Type:       "transcription",
			Model:      "whisper-large-v3",
			Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		},
		{
			Type:       "transcription",
			Model:      "whisper-turbo",
			Operations: map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		},
	}
	reg := service.NewRegistry(cfgs)

	_, err := reg.RouteAsync("transcription", "")
	if err == nil {
		t.Error("expected error when multiple models and no default")
	}
}

// ── wildcard routing ─────────────────────────────────────────────────────────

func wildcardLLMConfig(model string, isDefault bool) config.ServiceConfig {
	return config.ServiceConfig{
		Type:         "llm",
		Model:        model,
		Default:      isDefault,
		Provider:     "passthrough",
		InferenceURL: "http://vllm.svc:8000",
		Operations:   map[string][]string{"proxy": {"/v1/*"}},
	}
}

func TestRouteSync_Wildcard_MatchesAnySubPath(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})

	paths := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/responses",
		"/v1/audio/speech",
	}
	for _, p := range paths {
		def, err := reg.RouteSync(p, "gpt-4o")
		if err != nil {
			t.Errorf("RouteSync(%q) failed: %v", p, err)
			continue
		}
		if def.Model != "gpt-4o" {
			t.Errorf("RouteSync(%q): expected model gpt-4o, got %q", p, def.Model)
		}
	}
}

func TestRouteSync_Wildcard_DoesNotMatchDifferentPrefix(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})

	_, err := reg.RouteSync("/v2/chat/completions", "gpt-4o")
	if err == nil {
		t.Error("wildcard /v1/* should not match /v2/chat/completions")
	}
}

func TestRouteSync_Wildcard_ExactPathTakesPrecedence(t *testing.T) {
	// Exact path for audio + wildcard for LLM on the same prefix.
	cfgs := []config.ServiceConfig{
		{
			Type:         "audio",
			Model:        "whisper-large-v3",
			Provider:     "",
			InferenceURL: "http://whisper.svc",
			Operations:   map[string][]string{"transcription": {"/v1/audio/transcriptions"}},
		},
		wildcardLLMConfig("gpt-4o", false),
	}
	reg := service.NewRegistry(cfgs)

	// Exact path → whisper, not gpt-4o.
	def, err := reg.RouteSync("/v1/audio/transcriptions", "whisper-large-v3")
	if err != nil {
		t.Fatalf("exact path should route to whisper: %v", err)
	}
	if def.Model != "whisper-large-v3" {
		t.Errorf("expected whisper-large-v3, got %q", def.Model)
	}

	// Other /v1/* path → gpt-4o via wildcard.
	def, err = reg.RouteSync("/v1/chat/completions", "gpt-4o")
	if err != nil {
		t.Fatalf("wildcard should catch /v1/chat/completions: %v", err)
	}
	if def.Model != "gpt-4o" {
		t.Errorf("expected gpt-4o, got %q", def.Model)
	}
}

func TestRouteSync_Wildcard_DefaultModel(t *testing.T) {
	cfgs := []config.ServiceConfig{
		wildcardLLMConfig("gpt-4o", true),
		wildcardLLMConfig("gpt-4o-mini", false),
	}
	reg := service.NewRegistry(cfgs)

	// No model specified → default selected.
	def, err := reg.RouteSync("/v1/chat/completions", "")
	if err != nil {
		t.Fatalf("wildcard default model resolution failed: %v", err)
	}
	if def.Model != "gpt-4o" {
		t.Errorf("expected default model gpt-4o, got %q", def.Model)
	}
}

func TestRouteSync_Wildcard_SingleModel_AutoSelected(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})

	// Single model → auto-selected when no model specified.
	def, err := reg.RouteSync("/v1/embeddings", "")
	if err != nil {
		t.Fatalf("single wildcard model should be auto-selected: %v", err)
	}
	if def.Model != "gpt-4o" {
		t.Errorf("expected gpt-4o, got %q", def.Model)
	}
}

func TestRegistry_HasSyncServices_WildcardOnly(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})
	if !reg.HasSyncServices() {
		t.Error("registry with only wildcard routes should report HasSyncServices=true")
	}
}

func TestRegistry_SyncPaths_IncludesWildcard(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{wildcardLLMConfig("gpt-4o", false)})
	paths := reg.SyncPaths()
	found := false
	for _, p := range paths {
		if p == "/v1/*" {
			found = true
		}
	}
	if !found {
		t.Errorf("SyncPaths should include /v1/*; got %v", paths)
	}
}

// ── resolveGuardrails tests ──────────────────────────────────────────────────

func guardrailsCfg(action string, checks []string) config.GuardrailsConfig {
	return config.GuardrailsConfig{Action: action, Checks: checks}
}

func buildRegistryWithGuardrails(cfg config.GuardrailsConfig) *service.Def {
	cfgs := []config.ServiceConfig{{
		Type:         "llm",
		Model:        "gpt-4o",
		Provider:     "passthrough",
		InferenceURL: "http://llm.svc",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		Guardrails:   cfg,
	}}
	reg := service.NewRegistry(cfgs)
	def, _ := reg.RouteAsync("llm", "gpt-4o")
	return def
}

// TestResolveGuardrails_ExplicitChecksAndAction verifies that explicit checks+action are honored
// on the Input stage.
func TestResolveGuardrails_ExplicitChecksAndAction(t *testing.T) {
	def := buildRegistryWithGuardrails(guardrailsCfg("redact", []string{"pii", "secrets"}))
	g := def.Guardrails.Input
	if !g.Enabled {
		t.Error("expected Input.Enabled=true for explicit checks")
	}
	if g.Action != "redact" {
		t.Errorf("expected Input.Action=redact, got %q", g.Action)
	}
	if len(g.Checks) != 2 {
		t.Errorf("expected 2 checks, got %v", g.Checks)
	}
}

// TestResolveGuardrails_DefaultActionBlock verifies that action defaults to "block"
// when omitted with explicit checks.
func TestResolveGuardrails_DefaultActionBlock(t *testing.T) {
	def := buildRegistryWithGuardrails(guardrailsCfg("", []string{"pii_us"}))
	g := def.Guardrails.Input
	if !g.Enabled {
		t.Error("expected Input.Enabled=true when checks set")
	}
	if g.Action != "block" {
		t.Errorf("expected default Input.Action=block, got %q", g.Action)
	}
}

// TestResolveGuardrails_Disabled verifies that guardrails are disabled when checks are empty.
func TestResolveGuardrails_Disabled(t *testing.T) {
	def := buildRegistryWithGuardrails(guardrailsCfg("", nil))
	if def.Guardrails.Input.Enabled {
		t.Error("expected Input.Enabled=false when no checks set")
	}
}

// TestResolveGuardrails_OutputStage verifies that output guardrails are resolved correctly.
func TestResolveGuardrails_OutputStage(t *testing.T) {
	cfg := config.GuardrailsConfig{
		Action: "block",
		Checks: []string{"pii"},
		Output: &config.GuardrailsStageConfig{
			Action: "redact",
			Checks: []string{"pii", "secrets"},
		},
	}
	def := buildRegistryWithGuardrails(cfg)

	in := def.Guardrails.Input
	if !in.Enabled || in.Action != "block" || len(in.Checks) != 1 {
		t.Errorf("unexpected Input stage: %+v", in)
	}

	out := def.Guardrails.Output
	if !out.Enabled {
		t.Error("expected Output.Enabled=true when output checks are set")
	}
	if out.Action != "redact" {
		t.Errorf("expected Output.Action=redact, got %q", out.Action)
	}
	if len(out.Checks) != 2 {
		t.Errorf("expected 2 output checks, got %v", out.Checks)
	}
}

// TestResolveGuardrails_OutputDisabledWhenNil verifies that the output stage is disabled
// when the Output key is absent in config.
func TestResolveGuardrails_OutputDisabledWhenNil(t *testing.T) {
	def := buildRegistryWithGuardrails(guardrailsCfg("block", []string{"pii"}))
	if def.Guardrails.Output.Enabled {
		t.Error("expected Output.Enabled=false when output config is nil")
	}
}

// ── Visibility & backend-model helpers ─────────────────────────────────────────

func TestDef_VisibleTo(t *testing.T) {
	cfg := baseServiceConfig()
	cfg.Visibility = config.VisibilityConfig{
		UserTypes: []string{"beta", "internal"},
		Groups:    []string{"ai-team"},
	}
	reg := service.NewRegistry([]config.ServiceConfig{cfg})
	def := reg.Models()[0]

	if !def.IsRestricted() {
		t.Fatal("expected IsRestricted=true when visibility is set")
	}
	cases := []struct {
		name     string
		userType string
		groups   []string
		want     bool
	}{
		{"matching user type", "beta", nil, true},
		{"other matching user type", "internal", nil, true},
		{"matching group", "", []string{"ai-team"}, true},
		{"user type and group both miss", "user", []string{"other"}, false},
		{"no identity", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := def.VisibleTo(tc.userType, tc.groups); got != tc.want {
				t.Errorf("VisibleTo(%q, %v) = %v, want %v", tc.userType, tc.groups, got, tc.want)
			}
		})
	}
}

func TestDef_PublicModelAlwaysVisible(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{baseServiceConfig()})
	def := reg.Models()[0]
	if def.IsRestricted() {
		t.Fatal("expected public model to be unrestricted")
	}
	if !def.VisibleTo("", nil) {
		t.Error("expected public model visible to anonymous caller")
	}
}

// ── Model-backed guardrail resolution ──────────────────────────────────────────

func TestResolveGuardrailModels(t *testing.T) {
	cfg := config.ServiceConfig{
		Type:         "llm",
		Model:        "chat",
		Provider:     "passthrough",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		InferenceURL: "http://backend",
		Guardrails: config.GuardrailsConfig{
			Models: []config.GuardrailModelConfig{
				{Name: "pg", Endpoint: "http://pg", Mode: "sync", Action: "block", Timeout: "80ms"},
				{Name: "shadow", Endpoint: "http://s"},                                     // defaults: async/flag
				{Name: "red", Endpoint: "http://r", Mode: "sync", Action: "redact"},        // redact → flag (classifier)
				{Name: "asyncblock", Endpoint: "http://a", Mode: "async", Action: "block"}, // async → flag
				{Name: "noendpoint"}, // skipped
			},
		},
	}
	def := service.NewRegistry([]config.ServiceConfig{cfg}).Models()[0]
	stage := def.Guardrails.Input

	if !stage.Enabled {
		t.Error("input stage should be enabled when only models are configured")
	}
	models := stage.Models
	if len(models) != 4 {
		t.Fatalf("expected 4 resolved models (noendpoint skipped), got %d", len(models))
	}
	byName := map[string]struct{ mode, action string }{}
	for _, m := range models {
		byName[m.Detector.Name()] = struct{ mode, action string }{m.Mode, m.Action}
	}
	cases := map[string]struct{ mode, action string }{
		"pg":         {guardrails.ModeSync, guardrails.ActionBlock},
		"shadow":     {guardrails.ModeAsync, guardrails.ActionFlag}, // safe defaults
		"red":        {guardrails.ModeSync, guardrails.ActionFlag},  // redact coerced
		"asyncblock": {guardrails.ModeAsync, guardrails.ActionFlag}, // async coerced
	}
	for name, want := range cases {
		got, ok := byName[name]
		if !ok {
			t.Errorf("model %q missing from resolved set", name)
			continue
		}
		if got.mode != want.mode || got.action != want.action {
			t.Errorf("model %q: got mode=%s action=%s, want mode=%s action=%s", name, got.mode, got.action, want.mode, want.action)
		}
	}
}

func TestResolveGuardrailModels_NER(t *testing.T) {
	cfg := config.ServiceConfig{
		Type: "llm", Model: "chat", Provider: "passthrough",
		Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		InferenceURL: "http://backend",
		Guardrails: config.GuardrailsConfig{
			Models: []config.GuardrailModelConfig{
				{Name: "pii", Endpoint: "http://pii", Kind: "ner", Mode: "sync"},                               // default action → redact
				{Name: "pii-async", Endpoint: "http://p2", Kind: "ner", Mode: "async"},                         // async → flag
				{Name: "clf-redact", Endpoint: "http://c", Kind: "classifier", Mode: "sync", Action: "redact"}, // no spans → flag
			},
		},
	}
	def := service.NewRegistry([]config.ServiceConfig{cfg}).Models()[0]
	byName := map[string]string{}
	for _, m := range def.Guardrails.Input.Models {
		byName[m.Detector.Name()] = m.Action
	}
	want := map[string]string{
		"pii":        guardrails.ActionRedact, // NER + sync default = redact
		"pii-async":  guardrails.ActionFlag,   // async can't act
		"clf-redact": guardrails.ActionFlag,   // classifier can't redact
	}
	for name, action := range want {
		if byName[name] != action {
			t.Errorf("model %q: got action %q, want %q", name, byName[name], action)
		}
	}
}

// ── backend_pools ─────────────────────────────────────────────────────────────

func poolServiceConfig(model, poolName string) config.ServiceConfig {
	return config.ServiceConfig{
		Type:        "llm",
		Model:       model,
		Provider:    "openai",
		BackendPool: poolName,
		Operations:  map[string][]string{"chat": {"/v1/chat/completions"}},
	}
}

func TestRegistry_BackendPool_ResolvesFlattenedBackends(t *testing.T) {
	pools := map[string]config.BackendPoolConfig{
		"vllm-llama3": {
			Members: []config.BackendPoolMember{
				{BackendConfig: config.BackendConfig{URL: "http://vllm-1:8000", Weight: 3}},
				{BackendConfig: config.BackendConfig{URL: "http://vllm-2:8000", Weight: 1}},
			},
		},
	}
	reg := service.NewRegistry([]config.ServiceConfig{poolServiceConfig("gpt-4o", "vllm-llama3")}, service.WithBackendPools(pools))

	def := reg.Models()[0]
	if def.PoolName != "vllm-llama3" {
		t.Errorf("expected PoolName vllm-llama3, got %q", def.PoolName)
	}
	if len(def.Backends) != 2 {
		t.Fatalf("expected 2 flattened backends, got %d", len(def.Backends))
	}
	urls := map[string]int{}
	for _, b := range def.Backends {
		urls[b.URL] = b.Weight
	}
	if urls["http://vllm-1:8000"] != 3 || urls["http://vllm-2:8000"] != 1 {
		t.Errorf("unexpected backend weights: %+v", urls)
	}
}

func TestRegistry_BackendPool_IndexedForSyncRouting(t *testing.T) {
	pools := map[string]config.BackendPoolConfig{
		"pool-a": {Members: []config.BackendPoolMember{{BackendConfig: config.BackendConfig{URL: "http://a:8000", Weight: 1}}}},
	}
	reg := service.NewRegistry([]config.ServiceConfig{poolServiceConfig("gpt-4o", "pool-a")}, service.WithBackendPools(pools))

	if !reg.HasSyncServices() {
		t.Fatal("pool-based service should be indexed for sync routing (regression guard for the hasBackend fix)")
	}
	def, err := reg.RouteSync("/v1/chat/completions", "gpt-4o")
	if err != nil {
		t.Fatalf("unexpected routing error: %v", err)
	}
	if def.Model != "gpt-4o" {
		t.Errorf("routed to wrong def: %+v", def)
	}
}

func TestRegistry_BackendPool_HeaderPrecedence(t *testing.T) {
	pools := map[string]config.BackendPoolConfig{
		"pool-a": {
			Headers: map[string]string{"X-Pool": "pool-value", "X-Shared": "from-pool"},
			Members: []config.BackendPoolMember{
				{BackendConfig: config.BackendConfig{
					URL:     "http://a:8000",
					Weight:  1,
					Headers: map[string]string{"X-Shared": "from-member", "X-Member": "member-value"},
				}},
			},
		},
	}
	reg := service.NewRegistry([]config.ServiceConfig{poolServiceConfig("gpt-4o", "pool-a")}, service.WithBackendPools(pools))
	def := reg.Models()[0]
	if len(def.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(def.Backends))
	}
	h := def.Backends[0].Headers
	if h["X-Pool"] != "pool-value" {
		t.Errorf("expected pool-only header to survive, got %q", h["X-Pool"])
	}
	if h["X-Member"] != "member-value" {
		t.Errorf("expected member-only header to survive, got %q", h["X-Member"])
	}
	if h["X-Shared"] != "from-member" {
		t.Errorf("expected member header to win on collision, got %q", h["X-Shared"])
	}
}

func TestRegistry_BackendPool_UnknownPool_NoBackendsNoPanic(t *testing.T) {
	// Config validation normally rejects an unknown backend_pool reference before
	// the registry is ever built; this guards the registry itself against a nil
	// or incomplete pools map (e.g. a test that omits WithBackendPools) — it must
	// not panic, even though the resulting Def has no usable backends.
	reg := service.NewRegistry([]config.ServiceConfig{poolServiceConfig("gpt-4o", "does-not-exist")})
	def := reg.Models()[0]
	if len(def.Backends) != 0 {
		t.Errorf("expected no backends for unresolvable pool reference, got %+v", def.Backends)
	}
}

func TestRegistry_InlineBackends_PoolNameEmpty(t *testing.T) {
	cfg := baseServiceConfig()
	reg := service.NewRegistry([]config.ServiceConfig{cfg})
	def := reg.Models()[0]
	if def.PoolName != "" {
		t.Errorf("expected empty PoolName for inline-backends config, got %q", def.PoolName)
	}
}

func TestRegistry_Realtime_ResolveIndexAndDefaults(t *testing.T) {
	reg := service.NewRegistry([]config.ServiceConfig{
		{
			Type:         "transcription",
			Model:        "rt",
			InferenceURL: "http://asr.svc.cluster.local",
			Realtime:     &config.RealtimeConfig{Path: "/v1/audio/stream"},
		},
		{ // a normal sync service must not appear as realtime
			Type:         "llm",
			Model:        "chat",
			InferenceURL: "http://vllm:8000",
			Provider:     "openai",
			Operations:   map[string][]string{"chat": {"/v1/chat/completions"}},
		},
	})

	if !reg.HasRealtimeServices() {
		t.Fatal("expected HasRealtimeServices() true")
	}
	if got := reg.RealtimePaths(); len(got) != 1 || got[0] != "/v1/audio/stream" {
		t.Fatalf("RealtimePaths() = %v, want [/v1/audio/stream]", got)
	}
	def, err := reg.RouteRealtime("/v1/audio/stream")
	if err != nil {
		t.Fatalf("RouteRealtime error: %v", err)
	}
	if def.Realtime == nil {
		t.Fatal("resolved def has nil Realtime")
	}
	// Audio-format defaults (16 kHz s16le mono) and BackendPath defaulting to Path.
	rt := def.Realtime
	if rt.SampleRate != 16000 || rt.BytesPerSample != 2 || rt.Channels != 1 {
		t.Errorf("audio defaults wrong: %+v", rt)
	}
	if rt.BackendPath != "/v1/audio/stream" {
		t.Errorf("BackendPath default = %q, want /v1/audio/stream", rt.BackendPath)
	}
	if bps := rt.BytesPerSecond(); bps != 32000 {
		t.Errorf("BytesPerSecond() = %d, want 32000", bps)
	}
	// The sync service is not a realtime route.
	if _, err := reg.RouteRealtime("/v1/chat/completions"); err == nil {
		t.Error("sync path must not resolve as realtime")
	}
}
