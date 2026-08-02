package proxator

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPool_GetNextEndpoint_SingleAlive(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1)

	ep, err := pool.getNextEndpoint()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.index != 1 {
		t.Fatalf("expected endpoint 1, got %d", ep.index)
	}
}

func TestPool_GetNextEndpoint_SingleUnavailable(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1)
	pool.endpoints[0].setState(StateCooldown)

	if _, err := pool.getNextEndpoint(); !errors.Is(err, ErrAllProxiesBlocked) {
		t.Fatalf("expected ErrAllProxiesBlocked, got %v", err)
	}
}

func TestPool_GetNextEndpoint_AllDead(t *testing.T) {
	t.Parallel()

	pool := newTestPool(3)
	for _, ep := range pool.endpoints {
		ep.setState(StateDead)
	}

	if _, err := pool.getNextEndpoint(); !errors.Is(err, ErrAllProxiesBlocked) {
		t.Fatalf("expected ErrAllProxiesBlocked, got %v", err)
	}
}

func TestPool_GetNextEndpoint_SkipsUnavailable(t *testing.T) {
	t.Parallel()

	pool := newTestPool(3)
	pool.endpoints[0].setState(StateDead)
	pool.endpoints[1].setState(StateCooldown)

	for i := 0; i < 20; i++ {
		ep, err := pool.getNextEndpoint()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.index != 3 {
			t.Fatalf("expected endpoint 3, got %d", ep.index)
		}
	}
}

func TestPool_GetNextEndpoint_WeightedSelection(t *testing.T) {
	t.Parallel()

	pool := newTestPool(3)

	// Endpoint 1: fast and reliable.
	pool.endpoints[0].avgLatency.Store(int64(100 * time.Millisecond))
	pool.endpoints[0].totalReqs.Store(100)
	pool.endpoints[0].successReqs.Store(99)

	// Endpoint 2: slow and unreliable.
	pool.endpoints[1].avgLatency.Store(int64(2 * time.Second))
	pool.endpoints[1].totalReqs.Store(100)
	pool.endpoints[1].successReqs.Store(50)

	// Endpoint 3: somewhere in between.
	pool.endpoints[2].avgLatency.Store(int64(500 * time.Millisecond))
	pool.endpoints[2].totalReqs.Store(100)
	pool.endpoints[2].successReqs.Store(90)

	// Weight is success rate over average latency, so the expected traffic split
	// is those three ratios normalised.
	weights := map[int]float64{
		1: 0.99 / 0.1,
		2: 0.50 / 2.0,
		3: 0.90 / 0.5,
	}
	var totalWeight float64
	for _, w := range weights {
		totalWeight += w
	}

	const (
		iterations = 10000
		tolerance  = 0.25 // relative, comfortably outside sampling noise
	)

	counts := map[int]int{}
	for i := 0; i < iterations; i++ {
		ep, err := pool.getNextEndpoint()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[ep.index]++
	}

	t.Logf("selection distribution: %v", counts)

	for index, weight := range weights {
		want := weight / totalWeight * iterations
		got := float64(counts[index])
		if got < want*(1-tolerance) || got > want*(1+tolerance) {
			t.Fatalf(
				"endpoint %d got %.0f selections, expected ~%.0f (+/- %.0f%%): %v",
				index, got, want, tolerance*100, counts,
			)
		}
	}

	// And the ordering that the weighting exists to produce.
	if counts[1] <= counts[3] || counts[3] <= counts[2] {
		t.Fatalf("expected traffic to rank fast > moderate > slow, got: %v", counts)
	}
}

func TestPool_GetNextEndpoint_EqualWeightsDistribution(t *testing.T) {
	t.Parallel()

	pool := newTestPool(3)

	// No latency data anywhere, so every endpoint carries the neutral weight.
	counts := map[int]int{}
	for i := 0; i < 3000; i++ {
		ep, err := pool.getNextEndpoint()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[ep.index]++
	}

	for idx, count := range counts {
		if count < 600 || count > 1400 {
			t.Fatalf("endpoint %d got %d selections, expected ~1000: %v", idx, count, counts)
		}
	}
}

func TestPool_GetNextEndpointForDomain_PenalisesBlocked(t *testing.T) {
	t.Parallel()

	pool := newTestPool(3)

	const blockedDomain = "example.com"
	const otherDomain = "example.org"

	pool.recordDomainBlock(1, blockedDomain)
	pool.recordDomainBlock(1, blockedDomain)

	blocked := map[int]int{}
	for i := 0; i < 1000; i++ {
		ep, err := pool.getNextEndpointForDomain(blockedDomain)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		blocked[ep.index]++
	}

	if blocked[1] >= blocked[2] || blocked[1] >= blocked[3] {
		t.Fatalf("expected endpoint 1 to be deprioritised for %s, got: %v", blockedDomain, blocked)
	}
	t.Logf("distribution for the blocked domain: %v", blocked)

	// The same endpoint keeps full weight everywhere else.
	other := map[int]int{}
	for i := 0; i < 1000; i++ {
		ep, err := pool.getNextEndpointForDomain(otherDomain)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		other[ep.index]++
	}

	if other[1] < 200 {
		t.Fatalf("expected endpoint 1 to keep full weight for %s, got: %v", otherDomain, other)
	}
	t.Logf("distribution for the unaffected domain: %v", other)
}

func TestPool_GetNextEndpointForDomain_NoDomain(t *testing.T) {
	t.Parallel()

	pool := newTestPool(2)

	ep, err := pool.getNextEndpointForDomain("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep == nil {
		t.Fatal("expected a non-nil endpoint")
	}
}

func TestPool_GetNextEndpointForDomain_LastSurvivorIsStillSelectable(t *testing.T) {
	t.Parallel()

	pool := newTestPool(2)
	pool.endpoints[0].setState(StateDead)

	// Endpoint 2 is penalised for this domain but it is all that is left, so the
	// penalty must deprioritise rather than starve it.
	pool.recordDomainBlock(2, "example.com")

	ep, err := pool.getNextEndpointForDomain("example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.index != 2 {
		t.Fatalf("expected endpoint 2, got %d", ep.index)
	}
}

func TestPool_FindEndpoint(t *testing.T) {
	t.Parallel()

	pool := newTestPool(3)

	ep, err := pool.findEndpoint(2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.index != 2 {
		t.Fatalf("expected endpoint 2, got %d", ep.index)
	}

	if _, err := pool.findEndpoint(99); err == nil {
		t.Fatal("expected an error for an unknown endpoint index")
	}

	var nilPool *Pool
	if _, err := nilPool.findEndpoint(1); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("expected ErrPoolNotFound from a nil pool, got %v", err)
	}
}

func TestPool_Stats(t *testing.T) {
	t.Parallel()

	pool := newTestPool(3)
	pool.endpoints[0].setState(StateDead)
	pool.endpoints[1].setState(StateCooldown)
	pool.endpoints[2].avgLatency.Store(int64(200 * time.Millisecond))
	pool.endpoints[2].totalReqs.Store(50)
	pool.endpoints[2].successReqs.Store(45)

	stats := pool.Stats()

	if stats.Name != "test" {
		t.Fatalf("expected pool name %q, got %q", "test", stats.Name)
	}
	if stats.Total != 3 {
		t.Fatalf("expected total 3, got %d", stats.Total)
	}
	if stats.Alive != 1 {
		t.Fatalf("expected 1 alive, got %d", stats.Alive)
	}
	if stats.Dead != 1 {
		t.Fatalf("expected 1 dead, got %d", stats.Dead)
	}
	if stats.Cooldown != 1 {
		t.Fatalf("expected 1 in cooldown, got %d", stats.Cooldown)
	}
	if stats.SessionPoolSize != 5 {
		t.Fatalf("expected session pool size 5, got %d", stats.SessionPoolSize)
	}

	third := stats.Endpoints[2]
	if third.State != StateAlive {
		t.Fatalf("expected endpoint 3 to be alive, got %v", third.State)
	}
	if third.AvgLatency != 200*time.Millisecond {
		t.Fatalf("expected 200ms average latency, got %v", third.AvgLatency)
	}
	if third.TotalRequests != 50 {
		t.Fatalf("expected 50 total requests, got %d", third.TotalRequests)
	}
	if third.SuccessRate < 0.89 || third.SuccessRate > 0.91 {
		t.Fatalf("expected a ~0.9 success rate, got %f", third.SuccessRate)
	}
}

func TestPool_Name(t *testing.T) {
	t.Parallel()

	if got := newTestPool(1).Name(); got != "test" {
		t.Fatalf("expected %q, got %q", "test", got)
	}

	var nilPool *Pool
	if got := nilPool.Name(); got != "" {
		t.Fatalf("expected an empty name from a nil pool, got %q", got)
	}
}

func TestPool_NilPool(t *testing.T) {
	t.Parallel()

	var pool *Pool

	if _, err := pool.getNextEndpoint(); err == nil {
		t.Fatal("expected an error from a nil pool")
	}
	if stats := pool.Stats(); stats.Total != 0 {
		t.Fatalf("expected empty stats from a nil pool, got %v", stats)
	}

	// Neither of these should panic.
	pool.recordDomainBlock(1, "example.com")
	pool.Close()
}

func TestPool_EmptyEndpoints(t *testing.T) {
	t.Parallel()

	pool := newTestPool(0)

	if _, err := pool.getNextEndpoint(); !errors.Is(err, ErrAllProxiesBlocked) {
		t.Fatalf("expected ErrAllProxiesBlocked, got %v", err)
	}
}

func TestPool_ConcurrentSelection(t *testing.T) {
	t.Parallel()

	pool := newTestPool(5)

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	counts := map[int]int{}

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				ep, err := pool.getNextEndpointForDomain("example.com")
				if err != nil {
					return
				}
				pool.recordDomainBlock(ep.index, "example.org")
				mu.Lock()
				counts[ep.index]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	total := 0
	for _, c := range counts {
		total += c
	}
	if total != 1000 {
		t.Fatalf("expected 1000 selections, got %d", total)
	}
}

func TestNewPool(t *testing.T) {
	t.Parallel()

	pool, err := newPool(
		PoolConfig{
			Name:              "residential",
			Endpoints:         []string{testProxyURL, testProxyURL},
			SessionPoolSize:   2,
			RequestsPerSecond: 5,
		},
		DefaultRetryConfig(),
		testLogger(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer pool.Close()

	if pool.Name() != "residential" {
		t.Fatalf("expected pool name %q, got %q", "residential", pool.Name())
	}
	if len(pool.endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(pool.endpoints))
	}
	if pool.sessionPoolSize != 2 {
		t.Fatalf("expected session pool size 2, got %d", pool.sessionPoolSize)
	}
	if pool.health != nil {
		t.Fatal("expected health checking to stay off when it is not configured")
	}

	for i, ep := range pool.endpoints {
		if ep.index != i+1 {
			t.Fatalf("expected endpoint index %d, got %d", i+1, ep.index)
		}
		if ep.limiter == nil {
			t.Fatalf("expected endpoint %d to carry a rate limiter", ep.index)
		}
		if len(ep.sessions) != 2 {
			t.Fatalf("expected endpoint %d to hold 2 sessions, got %d", ep.index, len(ep.sessions))
		}
	}
}

func TestNewPool_DefaultsSessionPoolSize(t *testing.T) {
	t.Parallel()

	pool, err := newPool(
		PoolConfig{Name: "p", Endpoints: []string{testProxyURL}},
		DefaultRetryConfig(),
		testLogger(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer pool.Close()

	if pool.sessionPoolSize != defaultSessionPoolSize {
		t.Fatalf("expected session pool size %d, got %d", defaultSessionPoolSize, pool.sessionPoolSize)
	}
}

func TestNewPool_DomainBlockSettings(t *testing.T) {
	t.Parallel()

	pool, err := newPoolWithFactory(
		PoolConfig{
			Name:               "custom-domain-blocks",
			Endpoints:          []string{testProxyURL},
			SessionPoolSize:    1,
			DomainBlockTTL:     time.Hour,
			DomainBlockPenalty: 0.25,
		},
		DefaultRetryConfig(),
		testLogger(),
		&recordingFactory{response: &Response{StatusCode: 200}},
	)
	if err != nil {
		t.Fatalf("newPoolWithFactory returned an error: %v", err)
	}
	t.Cleanup(pool.Close)

	pool.recordDomainBlock(1, "example.com")
	if got := pool.domains.PenaltyFor(1, "example.com"); got != 0.25 {
		t.Fatalf("penalty = %f, want 0.25", got)
	}
}

func TestNewPool_InvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  PoolConfig
	}{
		{"missing name", PoolConfig{Endpoints: []string{testProxyURL}}},
		{"no endpoints", PoolConfig{Name: "p"}},
		{"unsupported proxy scheme", PoolConfig{Name: "p", Endpoints: []string{"ftp://127.0.0.1:21"}}},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				pool, err := newPool(tt.cfg, DefaultRetryConfig(), testLogger())
				if err == nil {
					pool.Close()
					t.Fatal("expected an error")
				}
				if pool != nil {
					t.Fatal("expected a nil pool alongside the error")
				}
			},
		)
	}
}

func TestNewPool_StartsHealthChecker(t *testing.T) {
	t.Parallel()

	pool, err := newPool(
		PoolConfig{
			Name:                "p",
			Endpoints:           []string{testProxyURL},
			SessionPoolSize:     1,
			HealthCheckInterval: time.Hour,
			HealthCheckURL:      "https://example.com/health",
		},
		DefaultRetryConfig(),
		testLogger(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer pool.Close()

	if pool.health == nil {
		t.Fatal("expected a health checker when an interval and URL are configured")
	}
	if pool.health.url != "https://example.com/health" {
		t.Fatalf("unexpected health check URL: %q", pool.health.url)
	}
}

func TestNewPool_ErrorMentionsPoolAndEndpoint(t *testing.T) {
	t.Parallel()

	_, err := newPool(
		PoolConfig{
			Name:            "residential",
			Endpoints:       []string{testProxyURL, "ftp://127.0.0.1:21"},
			SessionPoolSize: 1,
		},
		DefaultRetryConfig(),
		testLogger(),
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `pool "residential"`) || !strings.Contains(err.Error(), "endpoint 2") {
		t.Fatalf("expected the error to name the pool and endpoint, got: %v", err)
	}
}

func BenchmarkPool_GetNextEndpointForDomain(b *testing.B) {
	pool := newTestPool(100)
	for _, ep := range pool.endpoints {
		ep.recordLatency(100 * time.Millisecond)
		ep.recordSuccess()
	}
	for i := 1; i <= 20; i++ {
		pool.recordDomainBlock(i, "shop.example.com")
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := pool.getNextEndpointForDomain("shop.example.com"); err != nil {
			b.Fatal(err)
		}
	}
}
