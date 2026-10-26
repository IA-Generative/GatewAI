// Package health provides asynchronous backend health probing for the gateway.
//
// The Checker runs a background loop that probes each configured backend on a
// fixed interval and stores a JSON snapshot in Redis. GET /health reads the
// snapshot; no live probing happens on the request path.
//
// # Multi-instance safety
//
// A Redis distributed lock (SET NX EX <interval>) ensures only one gateway
// instance probes per cycle. Others skip their tick and serve the cached result.
// If the lock holder crashes the lock expires naturally (TTL = interval) and
// any surviving instance takes over on the next tick.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/service"
)

const (
	snapshotKey = "gatewai:health:snapshot"
	lockKey     = "gatewai:health:lock"
)

// Snapshot holds the result of one probe cycle.
type Snapshot struct {
	CheckedAt time.Time         `json:"checked_at"`
	Backends  map[string]string `json:"backends"` // model/type → "up" | "down"
}

// Checker runs periodic backend health probes and caches results in Redis.
type Checker struct {
	reg            atomic.Pointer[service.Registry]
	redis          *redis.Client
	defaultTimeout time.Duration
	interval       time.Duration
	instanceID     string
	httpClient     *http.Client
}

// New creates a Checker.
// instanceID must be unique per gateway replica (use the pod hostname).
func New(reg *service.Registry, redisClient *redis.Client, cfg config.HealthConfig, instanceID string) *Checker {
	timeout := cfg.TimeoutDuration()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	interval := cfg.IntervalDuration()
	if interval <= 0 {
		interval = 30 * time.Second
	}
	c := &Checker{
		redis:          redisClient,
		defaultTimeout: timeout,
		interval:       interval,
		instanceID:     instanceID,
		httpClient:     &http.Client{},
	}
	c.reg.Store(reg)
	return c
}

// UpdateRegistry swaps the registry used for future probe cycles (hot-reload).
func (c *Checker) UpdateRegistry(reg *service.Registry) {
	c.reg.Store(reg)
}

// Start runs the background probe loop until ctx is cancelled.
// Call as: go checker.Start(ctx).
func (c *Checker) Start(ctx context.Context) {
	c.runOnce(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

// Snapshot retrieves the latest probe result from Redis.
// Returns (nil, nil) when no snapshot has been stored yet.
func (c *Checker) Snapshot(ctx context.Context) (*Snapshot, error) {
	data, err := c.redis.Get(ctx, snapshotKey).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("health: reading snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("health: parsing snapshot: %w", err)
	}
	return &snap, nil
}

func (c *Checker) runOnce(ctx context.Context) {
	// Acquire the distributed lock.
	// TTL = interval: the lock expires on its own so a surviving instance can
	// take over if this one dies before the next tick.
	ok, err := c.redis.SetNX(ctx, lockKey, c.instanceID, c.interval).Result()
	if err != nil || !ok {
		return // another instance is (or was just) probing
	}

	reg := c.reg.Load()
	defs := reg.All()

	type result struct {
		key    string
		status string
	}
	ch := make(chan result, len(defs))
	var wg sync.WaitGroup
	for _, d := range defs {
		if d.InferenceURL == "" || d.HealthCheck.Disabled {
			continue
		}
		d := d
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := d.Model
			if key == "" {
				key = d.Type
			}
			ch <- result{key: key, status: c.probe(ctx, d)}
		}()
	}
	wg.Wait()
	close(ch)

	backends := make(map[string]string, len(defs))
	for r := range ch {
		backends[r.key] = r.status
	}

	snap := Snapshot{CheckedAt: time.Now().UTC(), Backends: backends}
	data, err := json.Marshal(snap)
	if err != nil {
		slog.Error("health checker: marshal snapshot", "error", err)
		return
	}
	// Store for 3× the interval so a few missed cycles don't erase the result.
	if err := c.redis.Set(ctx, snapshotKey, data, 3*c.interval).Err(); err != nil {
		slog.Error("health checker: store snapshot", "error", err)
	}
}

// probe makes an HTTP request to the backend's health endpoint.
// Returns "up" for 2xx–4xx, "down" for 5xx or connection errors,
// or "dormant" when scale_to_zero is set (no request sent).
func (c *Checker) probe(ctx context.Context, d *service.Def) string {
	if d.HealthCheck.ScaleToZero {
		return "dormant"
	}
	timeout := c.defaultTimeout
	if t := d.HealthCheck.TimeoutDuration(); t > 0 {
		timeout = t
	}
	healthPath := d.HealthCheck.Path
	if healthPath == "" {
		healthPath = "/health"
	}
	probeURL := strings.TrimSuffix(d.InferenceURL, "/") + "/" + strings.TrimPrefix(healthPath, "/")

	method := d.HealthCheck.Method
	if method == "" {
		method = http.MethodGet
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, method, probeURL, nil)
	if err != nil {
		return "down"
	}

	// Apply headers in increasing priority order:
	// 1. service-level inference headers
	// 2. per-backend headers (for the backend matching InferenceURL)
	// 3. health-check-specific headers (highest priority)
	for k, v := range d.InferenceHeaders {
		req.Header.Set(k, v)
	}
	for _, b := range d.Backends {
		if b.URL == d.InferenceURL {
			for k, v := range b.Headers {
				req.Header.Set(k, v)
			}
			break
		}
	}
	for k, v := range d.HealthCheck.Headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "down"
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		return "up"
	}
	return "down"
}
