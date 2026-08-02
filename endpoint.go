package proxator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// warmupRequests is the number of requests before weight fully trusts the
// measured latency. During warmup the weight blends between a neutral baseline
// and the computed weight, so a new endpoint neither hogs all traffic nor gets
// written off because of one slow first sample.
const warmupRequests = 10

// endpoint is a single proxy endpoint with reusable transport sessions.
type endpoint struct {
	url    string
	index  int
	logger *slog.Logger

	state       atomic.Int32
	failCount   atomic.Int32
	lastFailure time.Time
	mu          sync.RWMutex

	// sessions is a buffered channel used as a semaphore-backed pool: each
	// session is held by at most one goroutine at a time.
	sessions  chan Session
	poolSize  int
	closed    chan struct{}
	sessionMu sync.Mutex
	closeCtx  context.Context
	cancel    context.CancelFunc

	// limiter caps sustained throughput for this endpoint. nil means unlimited.
	limiter *rate.Limiter

	avgLatency   atomic.Int64 // nanoseconds, exponential moving average
	totalReqs    atomic.Int64
	successReqs  atomic.Int64
	cooldownTier atomic.Int32 // escalating cooldown multiplier (0 = first = 1x)

	cancelCooldown context.CancelFunc
	closeOnce      sync.Once
}

// newEndpoint creates an endpoint and eagerly opens its session pool.
func newEndpoint(proxyURL string, index, poolSize int, rps float64, logger *slog.Logger) (*endpoint, error) {
	return newEndpointWithFactory(proxyURL, index, poolSize, rps, logger, defaultSessionFactory())
}

func newEndpointWithFactory(
	proxyURL string,
	index, poolSize int,
	rps float64,
	logger *slog.Logger,
	factory SessionFactory,
) (*endpoint, error) {
	if poolSize <= 0 {
		poolSize = defaultSessionPoolSize
	}

	closeCtx, cancel := context.WithCancel(context.Background())
	e := &endpoint{
		url:      proxyURL,
		index:    index,
		logger:   logger,
		sessions: make(chan Session, poolSize),
		poolSize: poolSize,
		closed:   make(chan struct{}),
		closeCtx: closeCtx,
		cancel:   cancel,
	}
	e.setState(StateAlive)

	if rps > 0 {
		e.limiter = rate.NewLimiter(rate.Limit(rps), poolSize)
	}

	for i := 0; i < poolSize; i++ {
		session, err := factory.New(proxyURL)
		if err != nil {
			e.Close()
			return nil, fmt.Errorf("creating session %d: %w", i+1, err)
		}

		e.sessions <- session
	}

	return e, nil
}

// acquireSession borrows a session from the pool, first waiting on the rate
// limiter if one is configured. It blocks while every session is in use and
// returns the context error if the caller gives up first.
func (e *endpoint) acquireSession(ctx context.Context) (Session, error) {
	select {
	case <-e.closed:
		return nil, ErrClientClosed
	default:
	}

	if e.limiter != nil {
		waitCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(e.closeCtx, cancel)
		err := e.limiter.Wait(waitCtx)
		stop()
		cancel()
		if err != nil {
			select {
			case <-e.closed:
				return nil, ErrClientClosed
			default:
			}
			return nil, fmt.Errorf("rate limit wait: %w", err)
		}
	}

	select {
	case <-e.closed:
		return nil, ErrClientClosed
	case session := <-e.sessions:
		select {
		case <-e.closed:
			session.Close()
			return nil, ErrClientClosed
		default:
		}
		return session, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// releaseSession returns a session to the pool.
func (e *endpoint) releaseSession(session Session) {
	if session == nil {
		return
	}
	e.sessionMu.Lock()
	defer e.sessionMu.Unlock()
	select {
	case <-e.closed:
		session.Close()
		return
	default:
	}
	select {
	case e.sessions <- session:
	default:
		// Pool full — should not happen, but never leak the session.
		session.Close()
	}
}

// Close shuts every session down and cancels any pending cooldown recovery.
// It is safe to call concurrently and more than once.
func (e *endpoint) Close() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		if e.cancelCooldown != nil {
			e.cancelCooldown()
			e.cancelCooldown = nil
		}
		e.mu.Unlock()

		e.sessionMu.Lock()
		defer e.sessionMu.Unlock()
		close(e.closed)
		e.cancel()
		for {
			select {
			case session := <-e.sessions:
				session.Close()
			default:
				return
			}
		}
	})
}

func (e *endpoint) getState() State     { return State(e.state.Load()) }
func (e *endpoint) setState(s State)    { e.state.Store(int32(s)) }
func (e *endpoint) incrementFailCount() { e.failCount.Add(1) }
func (e *endpoint) resetFailCount()     { e.failCount.Store(0) }
func (e *endpoint) getFailCount() int32 { return e.failCount.Load() }
func (e *endpoint) isAvailable() bool   { return e.getState() == StateAlive }

// recordLatency folds a sample into the exponential moving average.
// Alpha of 0.3 keeps ~70% weight on history and 30% on the new sample.
func (e *endpoint) recordLatency(d time.Duration) {
	e.totalReqs.Add(1)
	ns := d.Nanoseconds()
	for {
		old := e.avgLatency.Load()
		if old == 0 {
			if e.avgLatency.CompareAndSwap(0, ns) {
				return
			}
			continue
		}
		updated := int64(float64(old)*0.7 + float64(ns)*0.3)
		if e.avgLatency.CompareAndSwap(old, updated) {
			return
		}
	}
}

// recordSuccess counts a success and resets cooldown escalation.
func (e *endpoint) recordSuccess() {
	e.successReqs.Add(1)
	e.cooldownTier.Store(0)
}

func (e *endpoint) getAvgLatency() time.Duration {
	return time.Duration(e.avgLatency.Load())
}

// successRate returns the success ratio, assuming a fresh endpoint is good
// until proven otherwise.
func (e *endpoint) successRate() float64 {
	total := e.totalReqs.Load()
	if total == 0 {
		return 1.0
	}
	return float64(e.successReqs.Load()) / float64(total)
}

// markFailed moves the endpoint into cooldown with an escalating duration:
// each consecutive cooldown doubles the base period, capped at 16x and at
// maxCooldown. A prior recovery goroutine, if any, is cancelled first.
func (e *endpoint) markFailed(baseCooldown, maxCooldown time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastFailure = time.Now()
	e.setState(StateCooldown)

	if e.cancelCooldown != nil {
		e.cancelCooldown()
	}

	tier := e.cooldownTier.Add(1)
	multiplier := int64(1) << min(tier-1, 4) // cap at 16x to prevent overflow
	cooldown := time.Duration(int64(baseCooldown) * multiplier)
	if maxCooldown > 0 && cooldown > maxCooldown {
		cooldown = maxCooldown
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.cancelCooldown = cancel

	e.logger.Info(
		"proxy endpoint cooldown",
		"endpoint", e.index,
		"cooldown", cooldown,
		"tier", tier,
		"fail_count", e.getFailCount(),
	)

	go func() {
		select {
		case <-time.After(cooldown):
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.getState() == StateCooldown {
				e.setState(StateAlive)
				e.resetFailCount()
				e.logger.Info("proxy endpoint recovered", "endpoint", e.index)
			}
		case <-ctx.Done():
			// Shutting down, or superseded by a newer cooldown — do not recover.
		}
	}()
}

func (e *endpoint) markDead() {
	e.setState(StateDead)
	e.logger.Warn("proxy endpoint marked dead", "endpoint", e.index)
}

// weight scores the endpoint for weighted random selection: lower latency and
// higher success rate mean a higher weight. During the first warmupRequests
// requests the score blends toward a neutral baseline so that limited data
// cannot cause wild routing swings.
func (e *endpoint) weight() float64 {
	if !e.isAvailable() {
		return 0
	}
	sr := e.successRate()
	latency := e.getAvgLatency()
	total := e.totalReqs.Load()

	if latency == 0 || total == 0 {
		return 1.0
	}

	computed := sr / latency.Seconds()
	if total >= warmupRequests {
		return computed
	}

	// confidence ramps 0.0 -> 1.0 across the warmup period.
	confidence := float64(total) / float64(warmupRequests)
	neutral := 1.0 / latency.Seconds() // baseline assumes a 100% success rate
	return neutral*(1-confidence) + computed*confidence
}
