package proxator

import (
	"context"
	"fmt"
	"time"
)

// healthChecker periodically probes alive endpoints so that a dead proxy is
// discovered by background traffic rather than by a user-facing request.
// Failures feed into the same fail-count and cooldown path as real requests.
type healthChecker struct {
	pool     *Pool
	interval time.Duration
	url      string
	timeout  time.Duration
	cancel   context.CancelFunc
}

// startHealthCheck launches the background loop, or returns nil when health
// checking is disabled.
func startHealthCheck(pool *Pool, cfg PoolConfig) *healthChecker {
	if cfg.HealthCheckInterval <= 0 || cfg.HealthCheckURL == "" {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	hc := &healthChecker{
		pool:     pool,
		interval: cfg.HealthCheckInterval,
		url:      cfg.HealthCheckURL,
		timeout:  cfg.healthCheckTimeout(),
		cancel:   cancel,
	}

	go hc.run(ctx)
	pool.logger.Info(
		"proxy health check started",
		"interval", hc.interval,
		"url", hc.url,
	)

	return hc
}

func (hc *healthChecker) run(ctx context.Context) {
	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.checkAll(ctx)
		}
	}
}

// checkAll probes every alive endpoint. A successful probe clears accumulated
// failures so that intermittent trouble does not add up to a cooldown.
func (hc *healthChecker) checkAll(ctx context.Context) {
	for _, ep := range hc.pool.endpoints {
		if ep.getState() != StateAlive {
			continue
		}

		if err := hc.pingEndpoint(ctx, ep); err != nil {
			hc.pool.logger.Warn(
				"proxy health check failed",
				"endpoint", ep.index,
				"error", err,
			)
			ep.incrementFailCount()
			if ep.getFailCount() >= hc.pool.retryConfig.failThreshold() {
				ep.markFailed(
					hc.pool.retryConfig.CooldownPeriod,
					hc.pool.retryConfig.MaxCooldown,
				)
			}
			continue
		}

		ep.resetFailCount()
	}
}

func (hc *healthChecker) pingEndpoint(ctx context.Context, ep *endpoint) error {
	pingCtx, cancel := context.WithTimeout(ctx, hc.timeout)
	defer cancel()

	session, err := ep.acquireSession(pingCtx)
	if err != nil {
		return err
	}
	defer ep.releaseSession(session)

	resp, err := session.Do(pingCtx, Request{Method: "GET", URL: hc.url})
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return &healthCheckError{statusCode: resp.StatusCode}
	}

	return nil
}

func (hc *healthChecker) stop() {
	if hc != nil && hc.cancel != nil {
		hc.cancel()
	}
}

type healthCheckError struct {
	statusCode int
}

func (e *healthCheckError) Error() string {
	return fmt.Sprintf("health check failed with status %d", e.statusCode)
}
