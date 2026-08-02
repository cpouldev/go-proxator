package proxator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestEndpoint_StateTransitions(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)

	if ep.getState() != StateAlive {
		t.Fatalf("expected StateAlive, got %v", ep.getState())
	}
	if !ep.isAvailable() {
		t.Fatal("expected the endpoint to be available")
	}

	ep.setState(StateCooldown)
	if ep.isAvailable() {
		t.Fatal("expected the endpoint to be unavailable in cooldown")
	}

	ep.setState(StateDead)
	if ep.isAvailable() {
		t.Fatal("expected a dead endpoint to be unavailable")
	}

	ep.setState(StateAlive)
	if !ep.isAvailable() {
		t.Fatal("expected the endpoint to be available again after a reset")
	}
}

func TestEndpoint_MarkDead(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	ep.markDead()

	if ep.getState() != StateDead {
		t.Fatalf("expected StateDead, got %v", ep.getState())
	}
	if ep.isAvailable() {
		t.Fatal("expected a dead endpoint to be unavailable")
	}
}

func TestEndpoint_FailCount(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)

	if ep.getFailCount() != 0 {
		t.Fatalf("expected fail count 0, got %d", ep.getFailCount())
	}

	ep.incrementFailCount()
	ep.incrementFailCount()
	if ep.getFailCount() != 2 {
		t.Fatalf("expected fail count 2, got %d", ep.getFailCount())
	}

	ep.resetFailCount()
	if ep.getFailCount() != 0 {
		t.Fatalf("expected fail count 0 after a reset, got %d", ep.getFailCount())
	}
}

func TestEndpoint_RecordLatency(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)

	// The first sample seeds the average directly.
	ep.recordLatency(100 * time.Millisecond)
	if avg := ep.getAvgLatency(); avg != 100*time.Millisecond {
		t.Fatalf("expected a 100ms seed average, got %v", avg)
	}

	// Then it becomes an exponential moving average: 100ms*0.7 + 200ms*0.3.
	ep.recordLatency(200 * time.Millisecond)
	want := time.Duration(float64(100*time.Millisecond)*0.7 + float64(200*time.Millisecond)*0.3)
	if avg := ep.getAvgLatency(); avg != want {
		t.Fatalf("expected %v, got %v", want, avg)
	}

	if ep.totalReqs.Load() != 2 {
		t.Fatalf("expected 2 recorded requests, got %d", ep.totalReqs.Load())
	}
}

func TestEndpoint_RecordLatency_Concurrent(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep.recordLatency(50 * time.Millisecond)
		}()
	}
	wg.Wait()

	if ep.totalReqs.Load() != 100 {
		t.Fatalf("expected 100 total requests, got %d", ep.totalReqs.Load())
	}
	if ep.getAvgLatency() <= 0 {
		t.Fatal("expected a positive average latency")
	}
}

func TestEndpoint_SuccessRate(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)

	// A fresh endpoint is assumed good until proven otherwise.
	if sr := ep.successRate(); sr != 1.0 {
		t.Fatalf("expected a 1.0 success rate with no data, got %f", sr)
	}

	ep.totalReqs.Store(3)
	ep.successReqs.Store(2)

	want := 2.0 / 3.0
	if sr := ep.successRate(); sr < want-0.001 || sr > want+0.001 {
		t.Fatalf("expected ~%f, got %f", want, sr)
	}
}

func TestEndpoint_Weight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(ep *endpoint)
		want     float64
		tolerate float64
	}{
		{
			name:     "no data is neutral",
			setup:    func(*endpoint) {},
			want:     1.0,
			tolerate: 0,
		},
		{
			name: "warmup blends toward neutral",
			setup: func(ep *endpoint) {
				ep.avgLatency.Store(int64(500 * time.Millisecond))
				ep.totalReqs.Store(warmupRequests / 2)
				ep.successReqs.Store(warmupRequests / 2)
			},
			// A perfect success rate makes the neutral baseline and the computed
			// weight identical, so the blend lands on 1/0.5.
			want:     1.0 / 0.5,
			tolerate: 0.1,
		},
		{
			name: "past warmup trusts the measurement",
			setup: func(ep *endpoint) {
				ep.avgLatency.Store(int64(500 * time.Millisecond))
				ep.totalReqs.Store(20)
				ep.successReqs.Store(16)
			},
			want:     0.8 / 0.5,
			tolerate: 0.01,
		},
		{
			name: "a poor success rate is discounted mid-warmup",
			setup: func(ep *endpoint) {
				ep.avgLatency.Store(int64(500 * time.Millisecond))
				ep.totalReqs.Store(2)
				ep.successReqs.Store(0)
			},
			// confidence 0.2: 2.0*0.8 + 0.0*0.2.
			want:     1.6,
			tolerate: 0.01,
		},
		{
			name: "dead endpoints weigh nothing",
			setup: func(ep *endpoint) {
				ep.avgLatency.Store(int64(100 * time.Millisecond))
				ep.totalReqs.Store(20)
				ep.successReqs.Store(20)
				ep.setState(StateDead)
			},
			want:     0,
			tolerate: 0,
		},
		{
			name: "cooldown endpoints weigh nothing",
			setup: func(ep *endpoint) {
				ep.setState(StateCooldown)
			},
			want:     0,
			tolerate: 0,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				ep := newTestEndpoint(1)
				tt.setup(ep)

				got := ep.weight()
				if got < tt.want-tt.tolerate || got > tt.want+tt.tolerate {
					t.Fatalf("weight = %f, want %f (+/- %f)", got, tt.want, tt.tolerate)
				}
			},
		)
	}
}

func TestEndpoint_Weight_NoColdStartBias(t *testing.T) {
	t.Parallel()

	// A brand new endpoint must not out-weigh one with a proven track record.
	fresh := newTestEndpoint(1)

	proven := newTestEndpoint(2)
	proven.avgLatency.Store(int64(200 * time.Millisecond))
	proven.totalReqs.Store(50)
	proven.successReqs.Store(45)

	if fresh.weight() > proven.weight() {
		t.Fatalf(
			"fresh endpoint weight (%f) should not exceed the proven one (%f)",
			fresh.weight(), proven.weight(),
		)
	}
}

func TestEndpoint_MarkFailed_Escalating(t *testing.T) {
	ep := newTestEndpoint(1)

	base := 50 * time.Millisecond
	maxCooldown := 500 * time.Millisecond

	// Tier 1: 50ms * 2^0.
	ep.markFailed(base, maxCooldown)
	if ep.getState() != StateCooldown {
		t.Fatal("expected StateCooldown after markFailed")
	}
	if tier := ep.cooldownTier.Load(); tier != 1 {
		t.Fatalf("expected cooldown tier 1, got %d", tier)
	}

	time.Sleep(80 * time.Millisecond)
	if ep.getState() != StateAlive {
		t.Fatal("expected recovery once the first cooldown elapsed")
	}
	if ep.getFailCount() != 0 {
		t.Fatalf("expected the fail count to reset on recovery, got %d", ep.getFailCount())
	}

	// Tier 2: 50ms * 2^1.
	ep.markFailed(base, maxCooldown)
	if tier := ep.cooldownTier.Load(); tier != 2 {
		t.Fatalf("expected cooldown tier 2, got %d", tier)
	}

	time.Sleep(80 * time.Millisecond)
	if ep.getState() == StateAlive {
		t.Fatal("expected the endpoint to still be cooling down 80ms into a 100ms cooldown")
	}

	time.Sleep(70 * time.Millisecond)
	if ep.getState() != StateAlive {
		t.Fatal("expected recovery once the escalated cooldown elapsed")
	}
}

func TestEndpoint_MarkFailed_MaxCooldown(t *testing.T) {
	ep := newTestEndpoint(1)

	// A high tier would otherwise mean 50ms * 16 = 800ms.
	ep.cooldownTier.Store(10)
	ep.markFailed(50*time.Millisecond, 100*time.Millisecond)

	time.Sleep(200 * time.Millisecond)
	if ep.getState() != StateAlive {
		t.Fatal("expected the cooldown to be capped at MaxCooldown")
	}
}

func TestEndpoint_MarkFailed_SupersedesPreviousCooldown(t *testing.T) {
	ep := newTestEndpoint(1)

	// A long cooldown followed immediately by a short one: the stale recovery
	// goroutine must not resurrect the endpoint later.
	ep.markFailed(time.Hour, 2*time.Hour)
	ep.markFailed(30*time.Millisecond, 30*time.Millisecond)

	time.Sleep(100 * time.Millisecond)
	if ep.getState() != StateAlive {
		t.Fatal("expected the newest cooldown to drive recovery")
	}
	if tier := ep.cooldownTier.Load(); tier != 2 {
		t.Fatalf("expected cooldown tier 2, got %d", tier)
	}
}

func TestEndpoint_MarkFailed_CancelOnClose(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)

	ep.markFailed(5*time.Second, 10*time.Second)
	if ep.getState() != StateCooldown {
		t.Fatal("expected StateCooldown")
	}

	// Close must cancel the pending recovery without panicking or deadlocking.
	ep.Close()
}

func TestEndpoint_Close_IsIdempotent(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)

	ep.Close()
	ep.Close()
}

func TestEndpoint_Close_UnblocksAcquire(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	result := make(chan error, 1)
	go func() {
		_, err := ep.acquireSession(context.Background())
		result <- err
	}()

	ep.Close()

	select {
	case err := <-result:
		if !errors.Is(err, ErrClientClosed) {
			t.Fatalf("acquire error = %v, want ErrClientClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("acquire remained blocked after Close")
	}
}

func TestEndpoint_Close_UnblocksRateLimitWait(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	ep.limiter = rate.NewLimiter(rate.Every(time.Hour), 1)
	if !ep.limiter.Allow() {
		t.Fatal("expected to consume the limiter's initial token")
	}

	result := make(chan error, 1)
	go func() {
		_, err := ep.acquireSession(context.Background())
		result <- err
	}()

	time.Sleep(10 * time.Millisecond)
	ep.Close()

	select {
	case err := <-result:
		if !errors.Is(err, ErrClientClosed) {
			t.Fatalf("acquire error = %v, want ErrClientClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("rate-limit wait remained blocked after Close")
	}
}

func TestEndpoint_RecordSuccess_ResetsCooldownTier(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	ep.cooldownTier.Store(5)

	ep.recordSuccess()

	if tier := ep.cooldownTier.Load(); tier != 0 {
		t.Fatalf("expected cooldown tier 0 after a success, got %d", tier)
	}
	if ep.successReqs.Load() != 1 {
		t.Fatalf("expected 1 successful request, got %d", ep.successReqs.Load())
	}
}

func TestEndpoint_AcquireRelease(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	// A session object is enough: nothing in this test dials through it.
	ep.sessions <- &recordingSession{}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	session, err := ep.acquireSession(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ep.sessions) != 0 {
		t.Fatal("expected the borrowed session to leave the pool")
	}

	ep.releaseSession(session)
	if len(ep.sessions) != 1 {
		t.Fatal("expected the session to return to the pool")
	}

	// Releasing nil is a no-op rather than a panic.
	ep.releaseSession(nil)
	if len(ep.sessions) != 1 {
		t.Fatal("expected releasing nil to leave the pool untouched")
	}
}

func TestEndpoint_ReleaseSession_ClosesOverflow(t *testing.T) {
	t.Parallel()

	closeCtx, cancel := context.WithCancel(context.Background())
	ep := &endpoint{
		url:      testProxyURL,
		index:    1,
		logger:   testLogger(),
		sessions: make(chan Session, 1),
		poolSize: 1,
		closed:   make(chan struct{}),
		closeCtx: closeCtx,
		cancel:   cancel,
	}
	ep.setState(StateAlive)

	ep.sessions <- &recordingSession{}

	// The pool is full, so the extra session is closed rather than leaked.
	ep.releaseSession(&recordingSession{})
	if len(ep.sessions) != 1 {
		t.Fatalf("expected the pool to stay at capacity, got %d", len(ep.sessions))
	}
}

func TestEndpoint_ReleaseSession_AfterCloseClosesSession(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	session := &recordingSession{}
	ep.Close()

	ep.releaseSession(session)

	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.closed {
		t.Fatal("expected a returned session to close after endpoint shutdown")
	}
}

func TestEndpoint_AcquireSession_ContextCancelled(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1) // no sessions available

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ep.acquireSession(ctx); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
}

func TestEndpoint_AcquireSession_BlocksUntilReleased(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	acquired := make(chan struct{})
	go func() {
		defer close(acquired)
		session, err := ep.acquireSession(ctx)
		if err == nil {
			ep.releaseSession(session)
		}
	}()

	select {
	case <-acquired:
		t.Fatal("expected acquireSession to block while the pool is empty")
	case <-time.After(20 * time.Millisecond):
	}

	ep.sessions <- nil

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("expected acquireSession to return once a session was released")
	}
}

func TestEndpoint_RateLimiter_BurstIsImmediate(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	ep.limiter = rate.NewLimiter(rate.Limit(10), 5)
	ep.sessions <- nil

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	session, err := ep.acquireSession(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ep.releaseSession(session)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected the first request to come straight out of the burst, took %v", elapsed)
	}
}

func TestEndpoint_RateLimiter_ContextTimeout(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	ep.limiter = rate.NewLimiter(rate.Limit(0.01), 0) // no burst at all
	ep.sessions <- nil

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := ep.acquireSession(ctx)
	if err == nil {
		t.Fatal("expected the rate limiter wait to fail under a short deadline")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("expected a rate limit error, got: %v", err)
	}
}

func TestEndpoint_RateLimiter_NilIsUnlimited(t *testing.T) {
	t.Parallel()

	ep := newTestEndpoint(1)
	ep.limiter = nil
	ep.sessions <- nil

	session, err := ep.acquireSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error with a nil limiter: %v", err)
	}
	ep.releaseSession(session)
}

func TestNewEndpoint(t *testing.T) {
	t.Parallel()

	ep, err := newEndpoint(testProxyURL, 3, 2, 4, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ep.Close()

	if ep.index != 3 {
		t.Fatalf("expected index 3, got %d", ep.index)
	}
	if ep.url != testProxyURL {
		t.Fatalf("expected url %q, got %q", testProxyURL, ep.url)
	}
	if ep.poolSize != 2 {
		t.Fatalf("expected pool size 2, got %d", ep.poolSize)
	}
	if len(ep.sessions) != 2 {
		t.Fatalf("expected 2 pooled sessions, got %d", len(ep.sessions))
	}
	if ep.getState() != StateAlive {
		t.Fatalf("expected a new endpoint to start alive, got %v", ep.getState())
	}
	if ep.limiter == nil {
		t.Fatal("expected a rate limiter when RequestsPerSecond is positive")
	}
	if burst := ep.limiter.Burst(); burst != 2 {
		t.Fatalf("expected the burst to match the session pool size, got %d", burst)
	}
}

func TestNewEndpoint_Defaults(t *testing.T) {
	t.Parallel()

	ep, err := newEndpoint(testProxyURL, 1, 0, 0, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer ep.Close()

	if ep.poolSize != defaultSessionPoolSize {
		t.Fatalf("expected pool size %d, got %d", defaultSessionPoolSize, ep.poolSize)
	}
	if ep.limiter != nil {
		t.Fatal("expected no rate limiter when RequestsPerSecond is zero")
	}
}

func TestNewEndpoint_InvalidProxyURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		proxyURL string
	}{
		{"empty", ""},
		{"unsupported scheme", "ftp://127.0.0.1:21"},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				ep, err := newEndpoint(tt.proxyURL, 1, 1, 0, testLogger())
				if err == nil {
					ep.Close()
					t.Fatal("expected an error")
				}
				if ep != nil {
					t.Fatal("expected a nil endpoint alongside the error")
				}
			},
		)
	}
}
