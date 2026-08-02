package proxator

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/cpouldev/go-proxator/internal/domain"
)

// Pool manages one named set of interchangeable proxy endpoints.
type Pool struct {
	name            string
	endpoints       []*endpoint
	currentIndex    atomic.Uint64 // uint64 so the counter can never wrap negative
	retryConfig     RetryConfig
	sessionPoolSize int
	domains         *domain.Tracker
	health          *healthChecker
	logger          *slog.Logger
}

// newPool builds a pool and starts its health checker, if configured.
func newPool(cfg PoolConfig, retryCfg RetryConfig, logger *slog.Logger) (*Pool, error) {
	return newPoolWithFactory(cfg, retryCfg, logger, defaultSessionFactory())
}

func newPoolWithFactory(
	cfg PoolConfig,
	retryCfg RetryConfig,
	logger *slog.Logger,
	factory SessionFactory,
) (*Pool, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	sessionPoolSize := cfg.sessionPoolSize()
	poolLogger := logger.With("pool", cfg.Name)

	pool := &Pool{
		name:            cfg.Name,
		endpoints:       make([]*endpoint, 0, len(cfg.Endpoints)),
		retryConfig:     retryCfg,
		sessionPoolSize: sessionPoolSize,
		domains:         domain.NewTracker(cfg.DomainBlockTTL, cfg.DomainBlockPenalty),
		logger:          poolLogger,
	}

	for i, proxyURL := range cfg.Endpoints {
		e, err := newEndpointWithFactory(
			proxyURL, i+1, sessionPoolSize, cfg.RequestsPerSecond, poolLogger, factory,
		)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("pool %q: creating endpoint %d: %w", cfg.Name, i+1, err)
		}
		pool.endpoints = append(pool.endpoints, e)
	}

	pool.health = startHealthCheck(pool, cfg)

	return pool, nil
}

// Name returns the pool's configured name.
func (p *Pool) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// Close releases every session and stops the health checker.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	if p.health != nil {
		p.health.stop()
	}
	for _, ep := range p.endpoints {
		ep.Close()
	}
}

// getNextEndpoint selects an endpoint with no domain hint.
func (p *Pool) getNextEndpoint() (*endpoint, error) {
	return p.getNextEndpointForDomain("")
}

// getNextEndpointForDomain selects an endpoint by weighted random choice,
// applying a penalty to endpoints that were recently blocked for this specific
// domain. An endpoint blocked by one host stays at full weight for every other
// host.
func (p *Pool) getNextEndpointForDomain(domain string) (*endpoint, error) {
	if p == nil || len(p.endpoints) == 0 {
		return nil, ErrAllProxiesBlocked
	}

	n := len(p.endpoints)

	if n == 1 {
		if p.endpoints[0].isAvailable() {
			return p.endpoints[0], nil
		}
		return nil, ErrAllProxiesBlocked
	}

	// Rotate the scan origin so that equal-weight endpoints still round-robin.
	startIdx := int(p.currentIndex.Add(1) % uint64(n))

	type candidate struct {
		ep     *endpoint
		weight float64
	}
	candidates := make([]candidate, 0, n)
	totalWeight := 0.0

	for i := 0; i < n; i++ {
		ep := p.endpoints[(startIdx+i)%n]
		if !ep.isAvailable() {
			continue
		}
		w := ep.weight()
		if w <= 0 {
			w = 0.001 // keep it selectable rather than starving it entirely
		}
		if domain != "" {
			w *= p.domains.PenaltyFor(ep.index, domain)
		}
		candidates = append(candidates, candidate{ep: ep, weight: w})
		totalWeight += w
	}

	switch len(candidates) {
	case 0:
		return nil, ErrAllProxiesBlocked
	case 1:
		return candidates[0].ep, nil
	}

	r := rand.Float64() * totalWeight
	cumulative := 0.0
	for _, c := range candidates {
		cumulative += c.weight
		if r <= cumulative {
			return c.ep, nil
		}
	}

	// Floating-point edge case: r landed just past the accumulated total.
	return candidates[len(candidates)-1].ep, nil
}

// recordDomainBlock notes that an endpoint was blocked for a specific domain.
func (p *Pool) recordDomainBlock(endpointIndex int, domain string) {
	if p != nil && p.domains != nil {
		p.domains.RecordBlock(endpointIndex, domain)
	}
}

// findEndpoint returns the endpoint with the given 1-based index.
func (p *Pool) findEndpoint(index int) (*endpoint, error) {
	if p == nil {
		return nil, ErrPoolNotFound
	}
	for _, ep := range p.endpoints {
		if ep.index == index {
			return ep, nil
		}
	}
	return nil, fmt.Errorf("proxator: endpoint %d not found in pool %q", index, p.name)
}

// Stats returns a point-in-time snapshot of the pool.
func (p *Pool) Stats() PoolStats {
	if p == nil {
		return PoolStats{}
	}
	stats := PoolStats{
		Name:            p.name,
		Total:           len(p.endpoints),
		SessionPoolSize: p.sessionPoolSize,
		Endpoints:       make([]EndpointStats, len(p.endpoints)),
	}

	for i, ep := range p.endpoints {
		state := ep.getState()
		stats.Endpoints[i] = EndpointStats{
			Index:             ep.index,
			State:             state,
			FailCount:         int(ep.getFailCount()),
			AvailableSessions: len(ep.sessions),
			TotalSessions:     ep.poolSize,
			AvgLatency:        ep.getAvgLatency(),
			TotalRequests:     ep.totalReqs.Load(),
			SuccessRequests:   ep.successReqs.Load(),
			SuccessRate:       ep.successRate(),
			CooldownTier:      int(ep.cooldownTier.Load()),
		}
		switch state {
		case StateAlive:
			stats.Alive++
		case StateDead:
			stats.Dead++
		case StateCooldown:
			stats.Cooldown++
		}
	}

	return stats
}

// PoolStats is a snapshot of one pool's health.
type PoolStats struct {
	Name            string
	Total           int
	Alive           int
	Dead            int
	Cooldown        int
	SessionPoolSize int
	Endpoints       []EndpointStats
}

// EndpointStats is a snapshot of one endpoint's health.
type EndpointStats struct {
	Index             int
	State             State
	FailCount         int
	AvailableSessions int
	TotalSessions     int
	AvgLatency        time.Duration
	TotalRequests     int64
	SuccessRequests   int64
	SuccessRate       float64
	CooldownTier      int
}
