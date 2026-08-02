package proxator

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/cpouldev/go-proxator/internal/domain"
)

// testProxyURL is syntactically valid but points at a port nothing listens on.
// Nothing in the test suite dials it: azuretls' SetProxy only parses and stores
// the URL, so endpoints can be constructed for real without any network I/O.
const testProxyURL = "http://user:pass@127.0.0.1:9999"

// testLogger discards everything so cooldown and health-check chatter does not
// end up in the test output.
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// fastRetry keeps retry-driven tests in the millisecond range.
func fastRetry() RetryConfig {
	return RetryConfig{
		MaxAttempts:    3,
		InitialDelay:   time.Millisecond,
		MaxDelay:       2 * time.Millisecond,
		CooldownPeriod: 5 * time.Second,
		MaxCooldown:    10 * time.Second,
		FailThreshold:  3,
	}
}

// newTestEndpoint builds an endpoint without opening any proxy sessions. Its
// session channel starts empty; tests that need a borrowable session push one
// in themselves.
//
// Endpoints built this way must not be Closed while a nil session sits in the
// channel — Close calls Session.Close on everything it drains.
func newTestEndpoint(index int) *endpoint {
	closeCtx, cancel := context.WithCancel(context.Background())
	ep := &endpoint{
		url:      testProxyURL,
		index:    index,
		logger:   testLogger(),
		sessions: make(chan Session, 5),
		poolSize: 5,
		closed:   make(chan struct{}),
		closeCtx: closeCtx,
		cancel:   cancel,
	}
	ep.setState(StateAlive)
	return ep
}

// newTestPool builds a pool of count test endpoints without opening sessions.
func newTestPool(count int) *Pool {
	pool := &Pool{
		name:            "test",
		endpoints:       make([]*endpoint, 0, count),
		retryConfig:     DefaultRetryConfig(),
		sessionPoolSize: 5,
		domains:         domain.NewTracker(0, 0),
		logger:          testLogger(),
	}
	for i := 0; i < count; i++ {
		pool.endpoints = append(pool.endpoints, newTestEndpoint(i+1))
	}
	return pool
}

// newTestClient builds a Client whose endpoints hold real (but never dialled)
// azuretls sessions, so the full Do path can be exercised offline.
func newTestClient(t *testing.T, endpointsPerPool int, names ...string) *Client {
	t.Helper()

	endpoints := make([]string, 0, endpointsPerPool)
	for i := 0; i < endpointsPerPool; i++ {
		endpoints = append(endpoints, testProxyURL)
	}

	pools := make([]PoolConfig, 0, len(names))
	for _, name := range names {
		pools = append(
			pools, PoolConfig{
				Name:            name,
				Endpoints:       endpoints,
				SessionPoolSize: 1,
			},
		)
	}

	client, err := New(
		Config{
			Pools:  pools,
			Retry:  fastRetry(),
			Logger: testLogger(),
		},
	)
	if err != nil {
		t.Fatalf("building test client: %v", err)
	}
	t.Cleanup(client.Close)

	return client
}
