package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"gatewai/gateway/internal/service"
)

// capitalizeFirstRune upper-cases the first rune of s (rune-aware, so a leading
// multi-byte character — e.g. a non-ASCII operation name — is never split).
func capitalizeFirstRune(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// GenerateSpec builds an OpenAPI 3.0.3 spec dynamically from the service registry.
// Call once at startup after the registry is initialised; serve the result statically.
// Admin-only endpoints (the `/-/` namespace) are NOT included here — see GenerateAdminSpec.
func GenerateSpec(reg *service.Registry, appVersion string, usageEnabled bool) []byte {
	paths := map[string]any{}

	// ── Fixed endpoints ────────────────────────────────────────────────────────
	paths["/health"] = healthPathItem()

	serviceTypes := reg.Types()
	sort.Strings(serviceTypes)

	paths["/jobs/{service_type}"] = submitJobPathItem(serviceTypes, reg)
	paths["/jobs/{service_type}/{id}"] = jobByIDPathItem(serviceTypes)

	if usageEnabled {
		paths["/usage"] = usagePathItem()
	}

	if reg.HasSyncServices() {
		paths["/v1/models"] = listModelsPathItem()
	}

	spec := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "Kevent Inference Gateway",
			"version": appVersion,
			"description": "Gateway for asynchronous and synchronous inference jobs.\n\n" +
				"**Async mode** — submit a file, get a `job_id`, poll for result or receive a webhook.\n\n" +
				"**Sync mode** — OpenAI-compatible endpoints, response held open until inference is done.\n\n" +
				"Operator-only endpoints (the `/-/` namespace) are documented separately at `/-/openapi.yaml` / `/-/docs`.",
		},
		"servers": []any{map[string]any{"url": "/"}},
		"tags":    specTags(usageEnabled),
		// Global security: all endpoints require a bearer token unless overridden.
		"security": []any{
			map[string]any{"BearerAuth": []any{}},
		},
		"paths":      paths,
		"components": specComponents(),
	}

	out, _ := yaml.Marshal(spec)
	return out
}

// GenerateAdminSpec builds a separate OpenAPI 3.0.3 spec for the `/-/` admin
// namespace (config reload, job purge, usage listing). Kept apart from
// GenerateSpec so operator-only endpoints aren't mixed into the consumer-facing
// API docs served at /docs.
func GenerateAdminSpec(appVersion string, usageEnabled bool, quotaResetEnabled bool) []byte {
	paths := map[string]any{
		"/-/reload":     reloadPathItem(),
		"/-/jobs/purge": purgeJobsPathItem(),
	}
	if usageEnabled {
		paths["/-/usage"] = adminUsagePathItem()
		paths["/-/usage/report"] = usageReportPathItem()
	}
	if quotaResetEnabled {
		paths["/-/quota/reset"] = quotaResetPathItem()
	}

	spec := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Kevent Inference Gateway — Admin",
			"version":     appVersion,
			"description": "Operator-only endpoints under the `/-/` namespace. Not intended for end consumers — protect with upstream auth/network policy.",
		},
		"servers": []any{map[string]any{"url": "/"}},
		"tags": []any{
			map[string]any{"name": "Admin", "description": "Operator-only endpoints"},
		},
		"security": []any{
			map[string]any{"BearerAuth": []any{}},
		},
		"paths":      paths,
		"components": specComponents(),
	}

	out, _ := yaml.Marshal(spec)
	return out
}

// specTags builds the OpenAPI tags array for the public gateway spec.
func specTags(usageEnabled bool) []any {
	tags := []any{
		map[string]any{"name": "Jobs", "description": "Async job submission and status"},
	}
	if usageEnabled {
		tags = append(tags, map[string]any{"name": "Usage", "description": "Per-consumer usage reporting"})
	}
	return tags
}

// NewDocsSpec returns a handler that serves the pre-generated OpenAPI spec.
func NewDocsSpec(spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(spec)
	}
}

// DocsUI serves the Swagger UI at GET /docs.
// Service specs are served under /docs/spec/{type}/{model}, which stays within
// the /docs* path prefix exposed by the API gateway.
func DocsUI(specs []SwaggerSpec) http.HandlerFunc {
	type urlEntry struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	entries := []urlEntry{
		{Name: "Gateway (jobs async + sync)", URL: "/openapi.yaml"},
	}
	for _, s := range specs {
		entries = append(entries, urlEntry{
			Name: s.Type + " / " + s.Model,
			URL:  "/docs/spec/" + s.Type + "/" + s.Model,
		})
	}
	urlsJSON, _ := json.Marshal(entries)

	html := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Kevent API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist/swagger-ui.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist/swagger-ui-bundle.js"></script>
  <script src="https://unpkg.com/swagger-ui-dist/swagger-ui-standalone-preset.js"></script>
  <script>
    SwaggerUIBundle({
      urls: ` + string(urlsJSON) + `,
      "urls.primaryName": "Gateway (jobs async + sync)",
      dom_id: "#swagger-ui",
      presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
      layout: "StandaloneLayout",
      deepLinking: true,
    });
  </script>
</body>
</html>`

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}
}

// AdminDocsUI serves the Swagger UI for the admin (`/-/`) spec at GET /-/docs.
func AdminDocsUI() http.HandlerFunc {
	html := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Kevent API — Admin</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist/swagger-ui.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist/swagger-ui-bundle.js"></script>
  <script src="https://unpkg.com/swagger-ui-dist/swagger-ui-standalone-preset.js"></script>
  <script>
    SwaggerUIBundle({
      url: "/-/openapi.yaml",
      dom_id: "#swagger-ui",
      presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
      layout: "StandaloneLayout",
      deepLinking: true,
    });
  </script>
</body>
</html>`

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}
}

// ── Path item builders ─────────────────────────────────────────────────────────

func healthPathItem() map[string]any {
	statusSchema := map[string]any{
		"type": "string",
		"enum": []any{"ok", "up", "partial", "down", "unknown"},
	}
	backendsSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "string", "enum": []any{"up", "down", "unknown"}},
		"description":          "Per-model status (only present when verbose=true)",
	}
	verboseResponseSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":     statusSchema,
			"checked_at": map[string]any{"type": "string", "format": "date-time"},
			"backends":   backendsSchema,
		},
	}
	simpleResponseSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "example": "ok"},
			"time":   map[string]any{"type": "string", "format": "date-time"},
		},
	}
	errorResponseSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": statusSchema,
			"error":  map[string]any{"type": "string"},
		},
	}

	return map[string]any{
		"get": map[string]any{
			"summary":     "Health check",
			"operationId": "healthCheck",
			"security":    []any{}, // public — no API key required
			"description": "Without query params returns a lightweight `{\"status\":\"ok\"}` with no backend probing.\n\n" +
				"Add `?verbose=true` or `?model=<name>` to retrieve the cached per-backend probe results " +
				"(populated by the background health-check loop).\n\n" +
				"Possible aggregate statuses: **up** (all backends up), **partial** (some up / some down), " +
				"**down** (all down), **unknown** (not yet probed or no backends configured).",
			"parameters": []any{
				map[string]any{
					"name":        "verbose",
					"in":          "query",
					"required":    false,
					"description": "Include per-backend statuses and last probe timestamp in the response.",
					"schema":      map[string]any{"type": "boolean", "default": false},
				},
				map[string]any{
					"name":        "mode",
					"in":          "query",
					"required":    false,
					"description": "Set to `strict` to return HTTP 500 when the aggregate status is not `up` (partial / down / unknown). The response body is unchanged.",
					"schema":      map[string]any{"type": "string", "enum": []any{"strict"}},
				},
				map[string]any{
					"name":        "model",
					"in":          "query",
					"required":    false,
					"description": "Filter to a single model name. Implies backend lookup (same as verbose=true).",
					"schema":      map[string]any{"type": "string"},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Health status (aggregate up, partial, or unknown in non-strict mode)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"oneOf": []any{simpleResponseSchema, verboseResponseSchema},
							},
						},
					},
				},
				"500": map[string]any{
					"description": "One or more backends are not up (strict mode only)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": verboseResponseSchema,
						},
					},
				},
				"503": map[string]any{
					"description": "Health cache unavailable (Redis error)",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": errorResponseSchema,
						},
					},
				},
			},
		},
	}
}

func submitJobPathItem(serviceTypes []string, reg *service.Registry) map[string]any {
	// Collect models and operations per service type.
	modelsByType := map[string][]string{}
	deprecatedModels := map[string]bool{}
	allOps := []string{}
	for _, def := range reg.All() {
		if def.Model != "" {
			modelsByType[def.Type] = uniqueSorted(append(modelsByType[def.Type], def.Model))
			if def.Deprecated {
				deprecatedModels[def.Model] = true
			}
		}
		for opName := range def.Operations {
			allOps = appendUniq(allOps, opName)
		}
	}
	sort.Strings(allOps)

	// Build a readable description of models per type. This path is shared
	// across every model of a type (routed via the "model" body field), so an
	// individual model's deprecation can only be called out inline here —
	// see syncPathItems / the per-model swagger overlay for cases where the
	// whole OpenAPI `deprecated: true` operation flag applies cleanly.
	typeParts := make([]string, 0, len(serviceTypes))
	for _, t := range serviceTypes {
		models := modelsByType[t]
		if len(models) == 0 {
			continue
		}
		labeled := make([]string, len(models))
		for i, m := range models {
			if deprecatedModels[m] {
				labeled[i] = m + " (deprecated)"
			} else {
				labeled[i] = m
			}
		}
		typeParts = append(typeParts, fmt.Sprintf("**%s**: %s", t, strings.Join(labeled, ", ")))
	}

	schemaProps := map[string]any{
		"file": map[string]any{
			"type":        "string",
			"format":      "binary",
			"description": "File to process",
		},
		"model": map[string]any{
			"type":        "string",
			"description": "Inference model. Required when multiple models are configured for the service type.\n\n" + strings.Join(typeParts, "\n\n"),
		},
		"callback_url": map[string]any{
			"type":        "string",
			"format":      "uri",
			"description": "Webhook URL called when the job completes",
			"example":     "https://your-app.example.com/webhook",
		},
	}
	if len(allOps) > 0 {
		opDesc := "Operation to perform. Required when the model has multiple operations."
		schemaProps["operation"] = map[string]any{
			"type":        "string",
			"enum":        allOps,
			"description": opDesc,
		}
	}

	return map[string]any{
		"post": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Submit an async inference job",
			"operationId": "submitJob",
			"parameters": []any{
				map[string]any{
					"name": "service_type", "in": "path", "required": true,
					"schema":  map[string]any{"type": "string", "enum": serviceTypes},
					"example": serviceTypes[0],
				},
				map[string]any{
					"name": "Idempotency-Key", "in": "header", "required": false,
					"schema":      map[string]any{"type": "string"},
					"description": "Optional. A repeat submission with the same key (scoped per consumer) returns the original job (200 + `X-Idempotent-Replay: true`) instead of starting a duplicate inference. If the key was reused but its job is gone, returns 409.",
				},
			},
			"requestBody": map[string]any{
				"required": true,
				"content": map[string]any{
					"multipart/form-data": map[string]any{
						"schema": map[string]any{
							"type":       "object",
							"required":   []string{"file"},
							"properties": schemaProps,
						},
					},
				},
			},
			"responses": map[string]any{
				"202": map[string]any{
					"description": "Job accepted",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/JobSubmitResponse"},
						},
					},
				},
				"200": map[string]any{
					"description": "Idempotent replay — an existing job with the same Idempotency-Key is returned (header `X-Idempotent-Replay: true`); no new inference is started",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/JobSubmitResponse"},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
				"409": map[string]any{"description": "Idempotency-Key was reused but its job is no longer available; retry with a new key"},
				"500": map[string]any{"$ref": "#/components/responses/InternalError"},
			},
		},
	}
}

// jobByIDPathItem returns the combined GET + DELETE path item for /jobs/{service_type}/{id}.
func jobByIDPathItem(serviceTypes []string) map[string]any {
	pathParams := []any{
		map[string]any{
			"name": "service_type", "in": "path", "required": true,
			"schema": map[string]any{"type": "string", "enum": serviceTypes},
		},
		map[string]any{
			"name": "id", "in": "path", "required": true,
			"schema":  map[string]any{"type": "string", "format": "uuid"},
			"example": "550e8400-e29b-41d4-a716-446655440000",
		},
	}
	return map[string]any{
		"get": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Get job status and result",
			"operationId": "getJob",
			"description": "Returns the current status of a job. When `completed`, the result is inlined.\n\n**The result file is deleted after this call** — subsequent calls return 404.",
			"parameters":  pathParams,
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Job found",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/JobStatusResponse"},
						},
					},
				},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
				"410": map[string]any{"description": "The job existed but its retention TTL has passed — body `{\"status\":\"expired\"}`. Distinguishes a timed-out/cleaned-up job from one that never existed (404)."},
			},
		},
		"delete": map[string]any{
			"tags":        []string{"Jobs"},
			"summary":     "Cancel a job",
			"operationId": "cancelJob",
			"description": "Cancels a pending job and deletes its input file from S3. Returns 409 if the job is not in `pending` state: once the relay has started inference (`processing`, `completed`, or `failed`), cancellation is not allowed.\n\nApplies the same consumer ownership check as GET: if `consumer_header` is configured and present, only the owning consumer can cancel.",
			"parameters":  pathParams,
			"responses": map[string]any{
				"204": map[string]any{"description": "Job cancelled"},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
				"409": map[string]any{
					"description": "Job is not cancellable in its current state",
					"content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
					},
				},
			},
		},
	}
}

// reloadPathItem documents POST /-/reload (admin spec only).
func reloadPathItem() map[string]any {
	return map[string]any{
		"post": map[string]any{
			"tags":        []string{"Admin"},
			"summary":     "Admin: hot-reload configuration",
			"operationId": "reloadConfig",
			"description": "Re-reads config.yaml and atomically swaps the active router, service registry, rate limits, and access-control policies. Auth configuration changes still require a process restart. Restricted to the `/-/` admin namespace — protect with upstream auth.",
			"responses": map[string]any{
				"200": map[string]any{"description": "Reload succeeded"},
				"500": map[string]any{"$ref": "#/components/responses/InternalError"},
			},
		},
	}
}

// usagePathItem documents GET /usage (public spec).
func usagePathItem() map[string]any {
	return map[string]any{
		"get": map[string]any{
			"tags":        []string{"Usage"},
			"summary":     "Get my usage",
			"operationId": "getMyUsage",
			"description": "Returns cumulative and current-window usage for the calling consumer (identified by the configured consumer header), across all configured service types.\n\n" +
				"Each service's `window` reports both the usage counted so far in the current window AND the configured quota it is measured against (`request_limit`/`token_limit`/`processing_time_limit_seconds` plus their periods, sourced from `rate_limits` in config.yaml) — so a caller can see how much of their allocated quota remains, not just what they've consumed.",
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Consumer usage",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/ConsumerUsage"},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"501": map[string]any{
					"description": "Usage tracking is not configured (no consumer_header set)",
					"content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
					},
				},
			},
		},
	}
}

// adminUsagePathItem documents GET /-/usage (admin spec only).
func adminUsagePathItem() map[string]any {
	return map[string]any{
		"get": map[string]any{
			"tags":        []string{"Admin"},
			"summary":     "Admin: list usage across consumers",
			"operationId": "adminListUsage",
			"description": "Paginated usage listing across all consumers, each including the configured quota alongside counted usage (see GET /usage). Restricted to the `/-/` admin namespace — protect with upstream auth.\n\n" +
				"Filter with `consumer` (exact match) or `type` (service type); otherwise returns consumers ordered by most-recently-active.",
			"parameters": []any{
				map[string]any{"name": "consumer", "in": "query", "required": false, "description": "Filter to a single consumer", "schema": map[string]any{"type": "string"}},
				map[string]any{"name": "type", "in": "query", "required": false, "description": "Filter to consumers active on this service type", "schema": map[string]any{"type": "string"}},
				map[string]any{"name": "limit", "in": "query", "required": false, "description": "Page size (default 20, max 100)", "schema": map[string]any{"type": "integer", "default": 20, "maximum": 100}},
				map[string]any{"name": "offset", "in": "query", "required": false, "description": "Page offset", "schema": map[string]any{"type": "integer", "default": 0, "minimum": 0}},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Paginated usage listing",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/AdminUsageResponse"},
						},
					},
				},
			},
		},
	}
}

// usageReportPathItem documents GET /-/usage/report (admin spec only).
func usageReportPathItem() map[string]any {
	return map[string]any{
		"get": map[string]any{
			"tags":        []string{"Admin"},
			"summary":     "Admin: cross-consumer calendar usage report",
			"operationId": "adminUsageReport",
			"description": "Cross-consumer, calendar-aligned usage totals for one service type — for finance/BI reporting (e.g. \"total tokens for service `llm` in March 2026\"), not a per-consumer breakdown. Restricted to the `/-/` admin namespace — protect with upstream auth.\n\n" +
				"Buckets are UTC calendar-aligned: `daily` (`YYYYMMDD`), `weekly` (ISO `YYYY-Www`), or `monthly` (`YYYYMM`). The `[from, to]` range is capped at 400 buckets per request.\n\n" +
				"**Example:** `GET /-/usage/report?type=llm&period=monthly&from=2026-01-01&to=2026-03-31`",
			"parameters": []any{
				map[string]any{"name": "type", "in": "query", "required": true, "description": "Service type, e.g. \"audio\", \"llm\"", "schema": map[string]any{"type": "string"}},
				map[string]any{"name": "period", "in": "query", "required": true, "description": "Bucket granularity", "schema": map[string]any{"type": "string", "enum": []string{"daily", "weekly", "monthly"}}},
				map[string]any{"name": "from", "in": "query", "required": true, "description": "Range start (inclusive), UTC", "schema": map[string]any{"type": "string", "format": "date", "example": "2026-01-01"}},
				map[string]any{"name": "to", "in": "query", "required": true, "description": "Range end (inclusive), UTC", "schema": map[string]any{"type": "string", "format": "date", "example": "2026-03-31"}},
				map[string]any{"name": "total", "in": "query", "required": false, "description": "When \"true\", also sum every bucket into the response's top-level `total` field", "schema": map[string]any{"type": "string", "enum": []string{"true", "false"}, "default": "false"}},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Usage report",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/UsageReport"},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"500": map[string]any{"$ref": "#/components/responses/InternalError"},
			},
		},
	}
}

// quotaResetPathItem documents POST /-/quota/reset (admin spec only).
func quotaResetPathItem() map[string]any {
	return map[string]any{
		"post": map[string]any{
			"tags":        []string{"Admin"},
			"summary":     "Admin: reset a consumer's quota for a service type",
			"operationId": "resetQuota",
			"description": "Deletes the rate-limit and token-budget Redis keys for one consumer/service_type pair, across all user types, so the next request starts a fresh window. With the optional `model` param, also deletes that consumer's per-model token budget, which is keyed by model rather than service_type and is otherwise untouched by a reset. Restricted to the `/-/` admin namespace — protect with upstream auth.\n\n" +
				"**Example:** `POST /-/quota/reset?consumer=acme&type=audio&model=whisper-large-v3`",
			"parameters": []any{
				map[string]any{"name": "consumer", "in": "query", "required": true, "description": "Consumer identifier (as set by consumer_header)", "schema": map[string]any{"type": "string"}},
				map[string]any{"name": "type", "in": "query", "required": true, "description": "Service type, e.g. \"audio\", \"llm\"", "schema": map[string]any{"type": "string"}},
				map[string]any{"name": "model", "in": "query", "required": false, "description": "Also reset this consumer's per-model token budget", "schema": map[string]any{"type": "string"}},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Quota reset",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"consumer":     map[string]any{"type": "string"},
									"type":         map[string]any{"type": "string"},
									"model":        map[string]any{"type": "string"},
									"deleted_keys": map[string]any{"type": "integer"},
								},
							},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"500": map[string]any{"$ref": "#/components/responses/InternalError"},
			},
		},
	}
}

// purgeJobsPathItem documents POST /-/jobs/purge (admin spec only).
func purgeJobsPathItem() map[string]any {
	return map[string]any{
		"post": map[string]any{
			"tags":        []string{"Admin"},
			"summary":     "Admin: purge stale pending jobs",
			"operationId": "purgeJobs",
			"description": "Deletes pending jobs older than `older_than`. Also cleans up their S3 input files. Restricted to the `/-/` admin namespace — protect with upstream auth.\n\nIf `truncated=true` in the response, there are more matching jobs — call again until `truncated=false` to fully drain the queue.\n\n**Example:** `POST /-/jobs/purge?older_than=2h&limit=200`",
			"parameters": []any{
				map[string]any{
					"name":        "older_than",
					"in":          "query",
					"required":    true,
					"description": "Minimum age of jobs to purge, as a Go duration string (e.g. `2h`, `30m`)",
					"schema":      map[string]any{"type": "string", "example": "2h"},
				},
				map[string]any{
					"name":        "limit",
					"in":          "query",
					"required":    false,
					"description": "Maximum number of jobs to delete per call (default 500). Call repeatedly until `truncated=false` to fully drain.",
					"schema":      map[string]any{"type": "integer", "default": 500, "minimum": 1},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Purge result",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"older_than": map[string]any{"type": "string", "example": "2h"},
									"found":      map[string]any{"type": "integer", "description": "Jobs matched (capped at limit)"},
									"purged":     map[string]any{"type": "integer", "description": "Jobs successfully deleted"},
									"truncated":  map[string]any{"type": "boolean", "description": "True when more matching jobs exist beyond the limit"},
								},
							},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
			},
		},
	}
}

func listModelsPathItem() map[string]any {
	return map[string]any{
		"get": map[string]any{
			"tags":    []string{"Inference"},
			"summary": "List available models",
			"description": "Without query params, returns all configured models in OpenAI-compatible format.\n\n" +
				"With `?model=<name>`, proxies to the underlying model backend to retrieve its native information " +
				"(context size, capabilities, etc.).\n\n" +
				"Models gated via `services[].visibility` are filtered by caller audience: a model restricted to " +
				"specific user types or groups is omitted from the list — and returns `404` on `?model=` — for callers " +
				"outside its audience, so a hidden model is indistinguishable from a non-existent one.",
			"operationId": "listModels",
			"parameters": []any{
				map[string]any{
					"name":        "model",
					"in":          "query",
					"required":    false,
					"description": "When provided, proxies the request to the underlying model backend and returns its native model information.",
					"schema":      map[string]any{"type": "string"},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Model list or native model information",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/ModelList"},
						},
					},
				},
				"404": map[string]any{"$ref": "#/components/responses/NotFound"},
				"502": map[string]any{"$ref": "#/components/responses/BadGateway"},
			},
		},
	}
}

// syncPathItems generates one path item per unique URL path found across all
// service definitions. Pattern paths (containing {model}) get a path parameter.
func syncPathItems(reg *service.Registry) map[string]any {
	type entry struct {
		serviceType      string
		models           []string
		deprecatedModels map[string]bool
		opNames          []string
		exts             []string
		isPattern        bool
	}

	byPath := map[string]*entry{}

	for _, def := range reg.All() {
		if def.InferenceURL == "" {
			continue
		}
		for opName, paths := range def.Operations {
			for _, p := range paths {
				if p == "" {
					continue
				}
				e := byPath[p]
				if e == nil {
					e = &entry{
						serviceType:      def.Type,
						isPattern:        strings.Contains(p, "{model}"),
						deprecatedModels: map[string]bool{},
					}
					byPath[p] = e
				}
				e.models = appendUniq(e.models, def.Model)
				e.opNames = appendUniq(e.opNames, opName)
				if def.Deprecated {
					e.deprecatedModels[def.Model] = true
				}
				for ext := range def.AcceptedExts {
					e.exts = appendUniq(e.exts, ext)
				}
			}
		}
	}

	items := map[string]any{}
	for p, e := range byPath {
		sort.Strings(e.models)
		sort.Strings(e.opNames)
		sort.Strings(e.exts)

		// Request body schema
		schemaProps := map[string]any{}
		required := []string{}

		allDeprecated := len(e.models) > 0
		for _, m := range e.models {
			if !e.deprecatedModels[m] {
				allDeprecated = false
				break
			}
		}

		if !e.isPattern {
			modelField := map[string]any{"type": "string"}
			if len(e.models) > 0 {
				modelField["enum"] = e.models
				modelField["example"] = e.models[0]
				if !allDeprecated {
					if deprecated := deprecatedModelNames(e.models, e.deprecatedModels); deprecated != "" {
						modelField["description"] = "Deprecated: " + deprecated
					}
				}
			}
			schemaProps["model"] = modelField
			if len(e.models) > 1 {
				required = append(required, "model")
			}
		}

		if len(e.opNames) > 1 {
			schemaProps["operation"] = map[string]any{
				"type":        "string",
				"enum":        e.opNames,
				"description": "Operation to perform. Required when the model supports multiple operations.",
			}
		}

		fileField := map[string]any{"type": "string", "format": "binary"}
		if len(e.exts) > 0 {
			fileField["description"] = "Accepted formats: " + strings.Join(e.exts, ", ")
		}
		schemaProps["file"] = fileField
		required = append(required, "file")

		// Summary from operation names
		summary := strings.Join(e.opNames, " / ")
		if summary != "" {
			summary = capitalizeFirstRune(summary) + " (sync)"
		} else {
			summary = "Inference (sync)"
		}

		desc := "Direct proxy to the inference backend (`inference_url`)."

		op := map[string]any{
			"tags":        []string{e.serviceType},
			"summary":     summary,
			"description": desc,
			"requestBody": map[string]any{
				"required": true,
				"content": map[string]any{
					"multipart/form-data": map[string]any{
						"schema": map[string]any{
							"type":       "object",
							"required":   required,
							"properties": schemaProps,
						},
					},
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Inference result",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"type": "object"},
						},
					},
				},
				"400": map[string]any{"$ref": "#/components/responses/BadRequest"},
				"422": map[string]any{"$ref": "#/components/responses/UnprocessableEntity"},
				"504": map[string]any{"$ref": "#/components/responses/GatewayTimeout"},
				"502": map[string]any{"$ref": "#/components/responses/BadGateway"},
			},
		}

		// Only mark the whole operation deprecated when every model sharing
		// this path is deprecated — otherwise callers of the still-active
		// models would see a misleading deprecation warning in Swagger UI.
		if allDeprecated {
			op["deprecated"] = true
		}

		if e.isPattern {
			op["parameters"] = []any{
				map[string]any{
					"name": "model", "in": "path", "required": true,
					"schema": map[string]any{"type": "string", "enum": e.models},
				},
			}
		}

		items[p] = map[string]any{"post": op}
	}

	return items
}

// ── Shared components ──────────────────────────────────────────────────────────

func specComponents() map[string]any {
	return map[string]any{
		"securitySchemes": map[string]any{
			"BearerAuth": map[string]any{
				"type":         "http",
				"scheme":       "bearer",
				"bearerFormat": "JWT",
			},
		},
		"schemas": map[string]any{
			"JobSubmitResponse": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":       map[string]any{"type": "string", "format": "uuid", "example": "550e8400-e29b-41d4-a716-446655440000"},
					"service_type": map[string]any{"type": "string", "example": "audio"},
					"model":        map[string]any{"type": "string", "example": "whisper-large-v3"},
					"status":       map[string]any{"type": "string", "example": "pending"},
				},
			},
			"JobStatusResponse": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id":       map[string]any{"type": "string", "format": "uuid"},
					"service_type": map[string]any{"type": "string"},
					"model":        map[string]any{"type": "string"},
					"status":       map[string]any{"$ref": "#/components/schemas/JobStatus"},
					"queue_position": map[string]any{
						"type":        "integer",
						"description": "1-indexed position in the model queue. Present only when status is `pending`.",
					},
					"result":     map[string]any{"type": "object", "description": "Inline result payload — present only when status is `completed`"},
					"error":      map[string]any{"type": "string", "description": "Error message — present only when status is `failed`"},
					"created_at": map[string]any{"type": "string", "format": "date-time"},
					"updated_at": map[string]any{"type": "string", "format": "date-time"},
				},
			},
			"JobStatus": map[string]any{
				"type":    "string",
				"enum":    []string{"pending", "processing", "completed", "failed"},
				"example": "completed",
			},
			"ModelList": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object": map[string]any{"type": "string", "example": "list"},
					"data": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/components/schemas/Model"},
					},
				},
			},
			"Model": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":           map[string]any{"type": "string", "example": "gpt-4o", "description": "Model alias clients request in the `model` field"},
					"object":       map[string]any{"type": "string", "example": "model"},
					"owned_by":     map[string]any{"type": "string", "example": "gatewai"},
					"service_type": map[string]any{"type": "string", "example": "llm"},
					"provider":     map[string]any{"type": "string", "example": "openai", "description": "LLM proxy provider, when set"},
					"backend_model": map[string]any{
						"type":        "string",
						"example":     "meta-llama/Meta-Llama-3-8B-Instruct",
						"description": "Real model this alias forwards to. Present only when the service's `backend_model` is configured; omitted when the alias is passed through unchanged.",
					},
					"capabilities": map[string]any{"$ref": "#/components/schemas/ModelCapabilities"},
				},
			},
			"ModelCapabilities": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"supports_async":     map[string]any{"type": "boolean"},
					"supports_sync":      map[string]any{"type": "boolean"},
					"supports_streaming": map[string]any{"type": "boolean"},
					"accepted_formats":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"max_file_size_mb":   map[string]any{"type": "integer", "format": "int64"},
					"operations":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"deprecated":         map[string]any{"type": "boolean"},
				},
			},
			"TokenUsage": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt":     map[string]any{"type": "integer", "format": "int64"},
					"completion": map[string]any{"type": "integer", "format": "int64"},
				},
			},
			"TotalUsage": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"requests":                map[string]any{"type": "integer", "format": "int64"},
					"jobs":                    map[string]any{"type": "integer", "format": "int64"},
					"processing_time_seconds": map[string]any{"type": "number"},
					"tokens":                  map[string]any{"$ref": "#/components/schemas/TokenUsage"},
				},
			},
			"WindowUsage": map[string]any{
				"type":        "object",
				"description": "Current rate-limit window: usage counted so far, plus the configured quota it is measured against (from `rate_limits` in config.yaml). Limit/period fields are omitted when no quota is configured for that dimension.",
				"properties": map[string]any{
					"requests":                      map[string]any{"type": "integer", "format": "int64", "description": "Requests counted in the current window"},
					"request_limit":                 map[string]any{"type": "integer", "format": "int64", "description": "Configured request quota (rate_limits.<type>.<user_type>.rate)"},
					"request_period":                map[string]any{"type": "string", "example": "1m", "description": "Configured window period (rate_limits.<type>.<user_type>.period)"},
					"tokens":                        map[string]any{"type": "integer", "format": "int64", "description": "Tokens counted in the current window"},
					"token_limit":                   map[string]any{"type": "integer", "format": "int64", "description": "Configured token quota (rate_limits.<type>.<user_type>.token_rate)"},
					"token_period":                  map[string]any{"type": "string", "example": "1h", "description": "Configured token window period (token_period)"},
					"processing_time_seconds":       map[string]any{"type": "number", "description": "Processing seconds counted in the current window"},
					"processing_time_limit_seconds": map[string]any{"type": "integer", "format": "int64", "description": "Configured processing-time quota (processing_time)"},
					"processing_time_period":        map[string]any{"type": "string", "example": "1h", "description": "Configured processing-time window period (processing_period)"},
					"reset_at":                      map[string]any{"type": "string", "format": "date-time", "description": "When the current window resets"},
				},
			},
			"ModelUsage": map[string]any{
				"type":        "object",
				"description": "Per-model token usage and quota, for models with a token budget configured via `services[].token_limits`. Limit/period fields are omitted when no quota is configured for that model.",
				"properties": map[string]any{
					"model":        map[string]any{"type": "string", "example": "gpt-oss"},
					"tokens":       map[string]any{"type": "integer", "format": "int64", "description": "Tokens counted in the current window for this model"},
					"token_limit":  map[string]any{"type": "integer", "format": "int64", "description": "Configured token quota (services[].token_limits.<user_type>.token_rate)"},
					"token_period": map[string]any{"type": "string", "example": "1h", "description": "Configured token window period (token_period)"},
					"reset_at":     map[string]any{"type": "string", "format": "date-time", "description": "When the current window resets"},
				},
			},
			"ServiceUsage": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"service_type": map[string]any{"type": "string", "example": "audio"},
					"total":        map[string]any{"$ref": "#/components/schemas/TotalUsage"},
					"window":       map[string]any{"$ref": "#/components/schemas/WindowUsage"},
					"models": map[string]any{
						"type":        "array",
						"description": "Per-model token quota breakdown (only present for services with model-level token_limits configured)",
						"items":       map[string]any{"$ref": "#/components/schemas/ModelUsage"},
					},
					"user_type": map[string]any{"type": "string", "description": "Tier this consumer was actually rate-limited under for this service type, recorded from real requests to it — not resolved from this /usage request's own identity, since a consumer can hold a different tier per service.", "example": "limited"},
				},
			},
			"ConsumerUsage": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"consumer":    map[string]any{"type": "string"},
					"retention":   map[string]any{"type": "string", "example": "all-time"},
					"last_active": map[string]any{"type": "string", "format": "date-time"},
					"usage": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/components/schemas/ServiceUsage"},
					},
				},
			},
			"AdminUsageResponse": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"total":  map[string]any{"type": "integer", "format": "int64"},
					"limit":  map[string]any{"type": "integer", "format": "int64"},
					"offset": map[string]any{"type": "integer", "format": "int64"},
					"consumers": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/components/schemas/ConsumerUsage"},
					},
				},
			},
			"PeriodUsage": map[string]any{
				"type":        "object",
				"description": "Cross-consumer aggregate totals for one calendar bucket.",
				"properties": map[string]any{
					"bucket":                  map[string]any{"type": "string", "description": "Bucket ID: YYYYMMDD (daily), ISO YYYY-Www (weekly), or YYYYMM (monthly)", "example": "202603"},
					"requests":                map[string]any{"type": "integer", "format": "int64"},
					"jobs":                    map[string]any{"type": "integer", "format": "int64"},
					"processing_time_seconds": map[string]any{"type": "number"},
					"tokens":                  map[string]any{"$ref": "#/components/schemas/TokenUsage"},
				},
			},
			"UsageReport": map[string]any{
				"type":        "object",
				"description": "Cross-consumer, calendar-aligned usage totals for one service type (GET /-/usage/report).",
				"properties": map[string]any{
					"service_type": map[string]any{"type": "string", "example": "llm"},
					"period":       map[string]any{"type": "string", "enum": []string{"daily", "weekly", "monthly"}},
					"from":         map[string]any{"type": "string", "format": "date"},
					"to":           map[string]any{"type": "string", "format": "date"},
					"buckets": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/components/schemas/PeriodUsage"},
					},
					"total": map[string]any{
						"description": "Sum of every bucket in `buckets`. Only present when the request sets `total=true`.",
						"allOf":       []any{map[string]any{"$ref": "#/components/schemas/TotalUsage"}},
					},
				},
			},
			"Error": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"error": map[string]any{"type": "string", "example": "field 'file' is required"},
				},
			},
		},
		"responses": map[string]any{
			"BadRequest": map[string]any{
				"description": "Invalid request",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"NotFound": map[string]any{
				"description": "Resource not found",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"UnprocessableEntity": map[string]any{
				"description": "Inference failed",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"InternalError": map[string]any{
				"description": "Internal server error",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"GatewayTimeout": map[string]any{
				"description": "Timed out waiting for inference result",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
			"BadGateway": map[string]any{
				"description": "Upstream inference backend error",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
				},
			},
		},
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────────

// deprecatedModelNames returns a sorted, comma-joined list of the models in
// models that are marked deprecated, or "" if none are.
func deprecatedModelNames(models []string, deprecated map[string]bool) string {
	var out []string
	for _, m := range models {
		if deprecated[m] {
			out = append(out, m)
		}
	}
	return strings.Join(out, ", ")
}

func appendUniq(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

func uniqueSorted(s []string) []string {
	seen := map[string]struct{}{}
	out := s[:0]
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
