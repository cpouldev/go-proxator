package proxator

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// The health checker only ever probes through an endpoint's pooled sessions.
// Test pools hold none, so every probe fails at acquireSession and no request
// ever leaves the machine.
func newTestHealthChecker(pool *Pool) *healthChecker {
	return &healthChecker{
		pool:     pool,
		interval: time.Hour,
		url:      "https://example.com/health",
		timeout:  time.Millisecond,
	}
}

func TestStartHealthCheck_Disabled(t *testing.T) {
	t.Parallel()

	pool := newTestPool(2)

	tests := []struct {
		name string
		cfg  PoolConfig
	}{
		{"zero interval", PoolConfig{HealthCheckURL: "https://example.com/health"}},
		{
			"negative interval",
			PoolConfig{HealthCheckInterval: -time.Second, HealthCheckURL: "https://example.com/health"},
		},
		{"no URL", PoolConfig{HealthCheckInterval: time.Minute}},
		{"neither", PoolConfig{}},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				if hc := startHealthCheck(pool, tt.cfg); hc != nil {
					hc.stop()
					t.Fatal("expected health checking to stay disabled")
				}
			},
		)
	}
}

func TestStartHealthCheck_Enabled(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1)
	cfg := PoolConfig{
		HealthCheckInterval: time.Hour,
		HealthCheckURL:      "https://example.com/health",
		HealthCheckTimeout:  2 * time.Second,
	}

	hc := startHealthCheck(pool, cfg)
	if hc == nil {
		t.Fatal("expected a health checker")
	}
	defer hc.stop()

	if hc.url != cfg.HealthCheckURL {
		t.Fatalf("expected URL %q, got %q", cfg.HealthCheckURL, hc.url)
	}
	if hc.interval != cfg.HealthCheckInterval {
		t.Fatalf("expected interval %v, got %v", cfg.HealthCheckInterval, hc.interval)
	}
	if hc.timeout != cfg.HealthCheckTimeout {
		t.Fatalf("expected timeout %v, got %v", cfg.HealthCheckTimeout, hc.timeout)
	}
}

func TestStartHealthCheck_DefaultTimeout(t *testing.T) {
	t.Parallel()

	hc := startHealthCheck(
		newTestPool(1),
		PoolConfig{HealthCheckInterval: time.Hour, HealthCheckURL: "https://example.com/health"},
	)
	if hc == nil {
		t.Fatal("expected a health checker")
	}
	defer hc.stop()

	if hc.timeout != defaultHealthCheckTimeout {
		t.Fatalf("expected the default timeout %v, got %v", defaultHealthCheckTimeout, hc.timeout)
	}
}

func TestHealthChecker_StopIsIdempotent(t *testing.T) {
	t.Parallel()

	hc := startHealthCheck(
		newTestPool(1),
		PoolConfig{HealthCheckInterval: time.Hour, HealthCheckURL: "https://example.com/health"},
	)
	if hc == nil {
		t.Fatal("expected a health checker")
	}

	hc.stop()
	hc.stop()

	var nilChecker *healthChecker
	nilChecker.stop()
}

func TestHealthChecker_CheckAll_SkipsUnavailableEndpoints(t *testing.T) {
	t.Parallel()

	pool := newTestPool(3)
	pool.endpoints[0].setState(StateDead)
	pool.endpoints[1].setState(StateCooldown)

	hc := newTestHealthChecker(pool)
	hc.checkAll(context.Background())

	if got := pool.endpoints[0].getFailCount(); got != 0 {
		t.Fatalf("expected the dead endpoint to be skipped, fail count %d", got)
	}
	if got := pool.endpoints[1].getFailCount(); got != 0 {
		t.Fatalf("expected the cooling-down endpoint to be skipped, fail count %d", got)
	}
	if got := pool.endpoints[2].getFailCount(); got != 1 {
		t.Fatalf("expected the alive endpoint to record a failure, fail count %d", got)
	}
}

func TestHealthChecker_CheckAll_AccumulatesFailures(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1)
	pool.endpoints[0].incrementFailCount()
	pool.endpoints[0].incrementFailCount()

	hc := newTestHealthChecker(pool)
	hc.checkAll(context.Background())

	if got := pool.endpoints[0].getFailCount(); got <= 2 {
		t.Fatalf("expected the fail count to grow past 2, got %d", got)
	}
}

func TestHealthChecker_CheckAll_CoolsDownAtThreshold(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1)
	pool.retryConfig = RetryConfig{
		FailThreshold:  2,
		CooldownPeriod: time.Hour,
		MaxCooldown:    time.Hour,
	}

	hc := newTestHealthChecker(pool)

	hc.checkAll(context.Background())
	if state := pool.endpoints[0].getState(); state != StateAlive {
		t.Fatalf("expected the endpoint to survive a single failure, got %v", state)
	}

	hc.checkAll(context.Background())
	if state := pool.endpoints[0].getState(); state != StateCooldown {
		t.Fatalf("expected StateCooldown once the threshold was crossed, got %v", state)
	}

	// Cancel the hour-long recovery goroutine.
	pool.Close()
}

func TestHealthChecker_CheckAll_HonoursCancelledContext(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1)
	hc := newTestHealthChecker(pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	hc.checkAll(ctx)

	if got := pool.endpoints[0].getFailCount(); got != 1 {
		t.Fatalf("expected a cancelled probe to count as a failure, got %d", got)
	}
}

func TestHealthChecker_PingEndpoint_Success(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1)
	session := &recordingSession{response: &Response{StatusCode: http.StatusNoContent}}
	pool.endpoints[0].sessions <- session
	hc := &healthChecker{
		pool:    pool,
		url:     "https://example.com/health",
		timeout: time.Second,
	}

	if err := hc.pingEndpoint(context.Background(), pool.endpoints[0]); err != nil {
		t.Fatalf("pingEndpoint returned an error: %v", err)
	}
	if len(session.requests) != 1 || session.requests[0].URL != hc.url {
		t.Fatalf("requests = %+v, want one request for %q", session.requests, hc.url)
	}
}

func TestHealthChecker_PingEndpoint_StatusError(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1)
	pool.endpoints[0].sessions <- &recordingSession{
		response: &Response{StatusCode: http.StatusInternalServerError},
	}
	hc := &healthChecker{pool: pool, url: "https://example.com/health", timeout: time.Second}

	err := hc.pingEndpoint(context.Background(), pool.endpoints[0])
	var statusErr *healthCheckError
	if !errors.As(err, &statusErr) || statusErr.statusCode != http.StatusInternalServerError {
		t.Fatalf("pingEndpoint error = %v, want healthCheckError for 500", err)
	}
}

func TestHealthChecker_PingEndpoint_TransportError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("transport failed")
	pool := newTestPool(1)
	pool.endpoints[0].sessions <- &recordingSession{err: wantErr}
	hc := &healthChecker{pool: pool, url: "https://example.com/health", timeout: time.Second}

	if err := hc.pingEndpoint(context.Background(), pool.endpoints[0]); !errors.Is(err, wantErr) {
		t.Fatalf("pingEndpoint error = %v, want %v", err, wantErr)
	}
}

func TestHealthCheckError_Message(t *testing.T) {
	t.Parallel()

	tests := []struct {
		statusCode int
		want       string
	}{
		{403, "health check failed with status 403"},
		{503, "health check failed with status 503"},
	}

	for _, tt := range tests {
		t.Run(
			tt.want, func(t *testing.T) {
				t.Parallel()
				err := &healthCheckError{statusCode: tt.statusCode}
				if got := err.Error(); got != tt.want {
					t.Fatalf("expected %q, got %q", tt.want, got)
				}
			},
		)
	}
}
