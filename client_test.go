package proxator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domainstate "github.com/cpouldev/go-proxator/internal/domain"
)

// respond returns a RequestFunc that never touches the borrowed session, so the
// whole Do path can be exercised without a single packet leaving the machine.
func respond(status int, calls *atomic.Int32) RequestFunc {
	return func(Session) (*Response, error) {
		if calls != nil {
			calls.Add(1)
		}
		return &Response{StatusCode: status}, nil
	}
}

func fail(err error, calls *atomic.Int32) RequestFunc {
	return func(Session) (*Response, error) {
		if calls != nil {
			calls.Add(1)
		}
		return nil, err
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	client, err := New(
		Config{
			Pools: []PoolConfig{
				{Name: "alpha", Endpoints: []string{testProxyURL, testProxyURL}, SessionPoolSize: 1},
				{Name: "beta", Endpoints: []string{testProxyURL}, SessionPoolSize: 1},
			},
			Logger: testLogger(),
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer client.Close()

	if got, want := client.PoolNames(), []string{"alpha", "beta"}; !equalStrings(got, want) {
		t.Fatalf("expected sorted pool names %q, got %q", want, got)
	}
	if !client.HasPool("alpha") || !client.HasPool("beta") {
		t.Fatal("expected both pools to be present")
	}
	if client.HasPool("nope") {
		t.Fatal("expected HasPool to reject an unknown name")
	}
	if client.Pool("alpha") == nil {
		t.Fatal("expected Pool to return the alpha pool")
	}
	if client.Pool("nope") != nil {
		t.Fatal("expected Pool to return nil for an unknown name")
	}

	// Unset optional fields fall back to package defaults.
	if client.detector == nil {
		t.Fatal("expected a default block detector")
	}
	if client.retry != DefaultRetryConfig() {
		t.Fatalf("expected the default retry config, got %+v", client.retry)
	}
	if client.logger == nil {
		t.Fatal("expected a logger")
	}
}

func TestNew_PoolNamesAreCopied(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "a", "b")

	names := client.PoolNames()
	names[0] = "mutated"

	if got := client.PoolNames(); got[0] == "mutated" {
		t.Fatal("expected PoolNames to hand back a copy")
	}
}

func TestNew_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
		wantMsg string
	}{
		{
			name:    "no pools",
			cfg:     Config{},
			wantErr: ErrNoPools,
		},
		{
			name: "duplicate pool names",
			cfg: Config{
				Pools: []PoolConfig{
					{Name: "main", Endpoints: []string{testProxyURL}, SessionPoolSize: 1},
					{Name: "main", Endpoints: []string{testProxyURL}, SessionPoolSize: 1},
				},
			},
			wantMsg: `duplicate pool name "main"`,
		},
		{
			name: "invalid pool config",
			cfg: Config{
				Pools: []PoolConfig{{Name: "main"}},
			},
			wantMsg: "has no endpoints",
		},
		{
			name: "unusable endpoint",
			cfg: Config{
				Pools: []PoolConfig{
					{Name: "main", Endpoints: []string{"ftp://127.0.0.1:21"}, SessionPoolSize: 1},
				},
			},
			wantMsg: "creating endpoint 1",
		},
		{
			name: "one bad pool among several",
			cfg: Config{
				Pools: []PoolConfig{
					{Name: "good", Endpoints: []string{testProxyURL}, SessionPoolSize: 1},
					{Name: "bad", Endpoints: []string{"ftp://127.0.0.1:21"}, SessionPoolSize: 1},
				},
			},
			wantMsg: `pool "bad"`,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()

				cfg := tt.cfg
				cfg.Logger = testLogger()

				client, err := New(cfg)
				if err == nil {
					client.Close()
					t.Fatal("expected an error")
				}
				if client != nil {
					t.Fatal("expected a nil client alongside the error")
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
					t.Fatalf("expected an error containing %q, got: %v", tt.wantMsg, err)
				}
			},
		)
	}
}

func TestClient_ResolvePool(t *testing.T) {
	t.Parallel()

	t.Run(
		"empty name with a single pool", func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, 1, "only")

			pool, err := client.resolvePool("")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pool.Name() != "only" {
				t.Fatalf("expected the sole pool, got %q", pool.Name())
			}
		},
	)

	t.Run(
		"empty name with several pools", func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, 1, "residential", "datacenter")

			if _, err := client.resolvePool(""); !errors.Is(err, ErrAmbiguousPool) {
				t.Fatalf("expected ErrAmbiguousPool, got %v", err)
			}
		},
	)

	t.Run(
		"unknown name", func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, 1, "only")

			_, err := client.resolvePool("nope")
			if !errors.Is(err, ErrPoolNotFound) {
				t.Fatalf("expected ErrPoolNotFound, got %v", err)
			}
			if !strings.Contains(err.Error(), `"nope"`) {
				t.Fatalf("expected the error to name the pool, got: %v", err)
			}
		},
	)

	t.Run(
		"named lookup", func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, 1, "residential", "datacenter")

			pool, err := client.resolvePool("datacenter")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pool.Name() != "datacenter" {
				t.Fatalf("expected the datacenter pool, got %q", pool.Name())
			}
		},
	)
}

func TestClient_Do_Success(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 2, "main")

	var calls atomic.Int32
	resp, err := client.Do(context.Background(), "main", respond(200, &calls))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one attempt, got %d", calls.Load())
	}

	stats := client.Stats()["main"]
	var total, success int64
	for _, ep := range stats.Endpoints {
		total += ep.TotalRequests
		success += ep.SuccessRequests
	}
	if total != 1 || success != 1 {
		t.Fatalf("expected 1 request and 1 success, got %d and %d", total, success)
	}
}

func TestClient_Do_EmptyPoolNameWithOnePool(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "only")

	if _, err := client.Do(context.Background(), "", respond(200, nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_Do_UnknownPool(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "main")

	var calls atomic.Int32
	_, err := client.Do(context.Background(), "nope", respond(200, &calls))
	if !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("expected ErrPoolNotFound, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("expected the request never to be attempted")
	}
}

func TestClient_Do_AllEndpointsUnavailable(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 2, "main")
	for i := 1; i <= 2; i++ {
		if err := client.MarkEndpointDead("main", i); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	var calls atomic.Int32
	_, err := client.Do(context.Background(), "main", respond(200, &calls))
	if !errors.Is(err, ErrAllProxiesBlocked) {
		t.Fatalf("expected ErrAllProxiesBlocked, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("expected the request never to be attempted")
	}
}

func TestClient_Do_RetriesBlockingResponses(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "main")

	var calls atomic.Int32
	resp, err := client.Do(context.Background(), "main", respond(403, &calls))
	if err == nil {
		t.Fatal("expected an error after every attempt was blocked")
	}
	// The blocked response is still handed back for inspection.
	if resp == nil || resp.StatusCode != 403 {
		t.Fatalf("expected the blocked response to be returned, got %v", resp)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}

	stats := client.Stats()["main"]
	if stats.Cooldown != 1 {
		t.Fatalf("expected the endpoint to be in cooldown, got %+v", stats)
	}
	if stats.Endpoints[0].TotalRequests != 3 {
		t.Fatalf("expected 3 recorded requests, got %d", stats.Endpoints[0].TotalRequests)
	}
	if stats.Endpoints[0].SuccessRequests != 0 {
		t.Fatalf("expected no successes, got %d", stats.Endpoints[0].SuccessRequests)
	}
}

func TestClient_Do_RecoversOnALaterAttempt(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 2, "main")

	var calls atomic.Int32
	resp, err := client.Do(
		context.Background(), "main",
		func(Session) (*Response, error) {
			if calls.Add(1) == 1 {
				return &Response{StatusCode: 429}, nil
			}
			return &Response{StatusCode: 200}, nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
}

func TestClient_Do_TransportError(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "main")

	sentinel := errors.New("fetching listing page: unexpected EOF")

	var calls atomic.Int32
	resp, err := client.Do(context.Background(), "main", fail(sentinel, &calls))
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the transport error to surface, got %v", err)
	}
	if resp != nil {
		t.Fatalf("expected no response, got %v", resp)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	// A non-blocking error must not count against the endpoint.
	if fails := client.Stats()["main"].Endpoints[0].FailCount; fails != 0 {
		t.Fatalf("expected no recorded failures, got %d", fails)
	}
}

func TestClient_Do_BlockingError(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "main")

	var calls atomic.Int32
	_, err := client.Do(
		context.Background(), "main",
		fail(errors.New("request blocked by the edge"), &calls),
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	if state := client.Stats()["main"].Endpoints[0].State; state != StateCooldown {
		t.Fatalf("expected the endpoint to be penalised into cooldown, got %v", state)
	}
}

func TestClient_Do_ContextCancelled(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "main")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	_, err := client.Do(ctx, "main", respond(200, &calls))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("expected the request never to be attempted")
	}
}

func TestClient_DoForDomain_RecordsDomainBlocks(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "main")
	const domain = "shop.example.net"

	if _, err := client.DoForDomain(context.Background(), "main", domain, respond(403, nil)); err == nil {
		t.Fatal("expected an error after every attempt was blocked")
	}

	pool := client.Pool("main")
	if got := pool.domains.PenaltyFor(1, domain); got != domainstate.DefaultBlockPenalty {
		t.Fatalf("expected the endpoint to be penalised for %s, got %f", domain, got)
	}
	if got := pool.domains.PenaltyFor(1, "example.org"); got != 1.0 {
		t.Fatalf("expected other domains to be unaffected, got %f", got)
	}
}

func TestClient_Do_Concurrent(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 3, "main")

	var wg sync.WaitGroup
	var failures atomic.Int32

	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if _, err := client.DoForDomain(ctx, "main", "example.com", respond(200, nil)); err != nil {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("expected every concurrent request to succeed, %d failed", failures.Load())
	}

	stats := client.Stats()["main"]
	var total int64
	for _, ep := range stats.Endpoints {
		total += ep.TotalRequests
	}
	if total != 30 {
		t.Fatalf("expected 30 recorded requests, got %d", total)
	}
}

func TestClient_Stats(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 2, "residential", "datacenter")

	stats := client.Stats()
	if len(stats) != 2 {
		t.Fatalf("expected stats for 2 pools, got %d", len(stats))
	}
	for name, poolStats := range stats {
		if poolStats.Name != name {
			t.Fatalf("expected stats keyed by pool name, got key %q for name %q", name, poolStats.Name)
		}
		if poolStats.Total != 2 {
			t.Fatalf("expected 2 endpoints in %q, got %d", name, poolStats.Total)
		}
		if poolStats.Alive != 2 {
			t.Fatalf("expected 2 alive endpoints in %q, got %d", name, poolStats.Alive)
		}
	}
}

func TestClient_MarkEndpointDeadAndReset(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 2, "main")

	if err := client.MarkEndpointDead("main", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stats := client.Stats()["main"]
	if stats.Dead != 1 || stats.Alive != 1 {
		t.Fatalf("expected one dead and one alive endpoint, got %+v", stats)
	}

	// Dead endpoints never recover on their own.
	endpoint, err := client.Pool("main").findEndpoint(1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	endpoint.failCount.Store(4)
	endpoint.cooldownTier.Store(3)

	if err := client.ResetEndpoint("main", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state := endpoint.getState(); state != StateAlive {
		t.Fatalf("expected StateAlive after a reset, got %v", state)
	}
	if got := endpoint.getFailCount(); got != 0 {
		t.Fatalf("expected the fail count to be cleared, got %d", got)
	}
	if got := endpoint.cooldownTier.Load(); got != 0 {
		t.Fatalf("expected the cooldown escalation to be cleared, got %d", got)
	}
}

func TestClient_EndpointControls_Errors(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "residential", "datacenter")

	tests := []struct {
		name string
		call func() error
	}{
		{"mark dead unknown pool", func() error { return client.MarkEndpointDead("nope", 1) }},
		{"mark dead unknown endpoint", func() error { return client.MarkEndpointDead("residential", 99) }},
		{"mark dead ambiguous pool", func() error { return client.MarkEndpointDead("", 1) }},
		{"reset unknown pool", func() error { return client.ResetEndpoint("nope", 1) }},
		{"reset unknown endpoint", func() error { return client.ResetEndpoint("residential", 99) }},
		{"reset ambiguous pool", func() error { return client.ResetEndpoint("", 1) }},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				if err := tt.call(); err == nil {
					t.Fatal("expected an error")
				}
			},
		)
	}
}

func TestClient_Ping_Validation(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, 1, "residential", "datacenter")
	ctx := context.Background()

	// A probe URL is mandatory: the library will not poll a third party on the
	// caller's behalf. These all fail before any request is attempted.
	if err := client.Ping(ctx, "residential", ""); err == nil {
		t.Fatal("expected an error without a probe URL")
	}
	if err := client.Ping(ctx, "nope", "https://example.com/health"); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("expected ErrPoolNotFound, got %v", err)
	}
	if err := client.Ping(ctx, "", "https://example.com/health"); !errors.Is(err, ErrAmbiguousPool) {
		t.Fatalf("expected ErrAmbiguousPool, got %v", err)
	}

	if err := client.MarkEndpointDead("residential", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := client.Ping(ctx, "residential", "https://example.com/health"); !errors.Is(
		err, ErrAllProxiesBlocked,
	) {
		t.Fatalf("expected ErrAllProxiesBlocked, got %v", err)
	}
}

func TestClient_Close_IsIdempotent(t *testing.T) {
	t.Parallel()

	client, err := New(
		Config{
			Pools:  []PoolConfig{{Name: "main", Endpoints: []string{testProxyURL}, SessionPoolSize: 1}},
			Logger: testLogger(),
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Callers routinely pair `defer client.Close()` with an explicit shutdown.
	client.Close()
	client.Close()
}

func TestClient_CustomDetector(t *testing.T) {
	t.Parallel()

	detector, err := NewBlockDetectorWith(DetectorConfig{StatusCodes: []int{418}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	client, err := New(
		Config{
			Pools:    []PoolConfig{{Name: "main", Endpoints: []string{testProxyURL}, SessionPoolSize: 1}},
			Retry:    fastRetry(),
			Detector: detector,
			Logger:   testLogger(),
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer client.Close()

	// 403 is no longer a block for this client.
	resp, err := client.Do(context.Background(), "main", respond(403, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("expected status 403, got %d", resp.StatusCode)
	}

	// The custom code is.
	if _, err := client.Do(context.Background(), "main", respond(418, nil)); err == nil {
		t.Fatal("expected the custom status code to be treated as a block")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
