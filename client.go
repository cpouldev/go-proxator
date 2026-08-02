package proxator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/cpouldev/go-proxator/internal/domain"
)

// Client routes requests across one or more named pools of rotating proxies.
//
// A Client is safe for concurrent use. Call Close when finished to release
// every pooled transport session and stop background health checks.
type Client struct {
	pools     map[string]*Pool
	poolNames []string
	detector  *BlockDetector
	retry     RetryConfig
	logger    *slog.Logger
}

// New creates a Client from cfg. It eagerly constructs every configured pool
// and closes any sessions created successfully if construction fails.
func New(cfg Config) (*Client, error) {
	if len(cfg.Pools) == 0 {
		return nil, ErrNoPools
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	detector := cfg.Detector
	if detector == nil {
		// Built here rather than via NewBlockDetector so that unparseable
		// Default patterns surface as an error instead of a panic.
		d, err := NewBlockDetectorWith(DetectorConfig{})
		if err != nil {
			return nil, err
		}
		detector = d
	}
	factory := cfg.SessionFactory
	if factory == nil {
		factory = defaultSessionFactory()
	}

	c := &Client{
		pools:     make(map[string]*Pool, len(cfg.Pools)),
		poolNames: make([]string, 0, len(cfg.Pools)),
		detector:  detector,
		retry:     cfg.Retry.withDefaults(),
		logger:    logger,
	}

	for _, pc := range cfg.Pools {
		if _, exists := c.pools[pc.Name]; exists {
			c.Close()
			return nil, fmt.Errorf("proxator: duplicate pool name %q", pc.Name)
		}
		pool, err := newPoolWithFactory(pc, c.retry, logger, factory)
		if err != nil {
			c.Close()
			return nil, err
		}
		c.pools[pc.Name] = pool
		c.poolNames = append(c.poolNames, pc.Name)
	}

	sort.Strings(c.poolNames)
	return c, nil
}

// Pool returns the named pool, or nil when it does not exist.
func (c *Client) Pool(name string) *Pool {
	return c.pools[name]
}

// PoolNames returns the configured pool names in sorted order.
func (c *Client) PoolNames() []string {
	out := make([]string, len(c.poolNames))
	copy(out, c.poolNames)
	return out
}

// HasPool reports whether the named pool exists and holds any endpoints.
func (c *Client) HasPool(name string) bool {
	p, ok := c.pools[name]
	return ok && len(p.endpoints) > 0
}

// resolvePool looks up a pool by name. An empty name selects the only
// configured pool, which keeps single-pool call sites terse.
func (c *Client) resolvePool(name string) (*Pool, error) {
	if name == "" {
		if len(c.poolNames) != 1 {
			return nil, ErrAmbiguousPool
		}
		return c.pools[c.poolNames[0]], nil
	}
	pool, ok := c.pools[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPoolNotFound, name)
	}
	return pool, nil
}

// RequestFunc issues a request on a borrowed session. It must not retain the
// session beyond the call and must return a non-nil Response with a nil error.
// To observe cancellation, capture the request context and pass it to
// Session.Do.
type RequestFunc func(session Session) (*Response, error)

// Do runs fn against a rotating endpoint of the named pool. It retries when fn
// returns an error or the response is classified as blocking. It selects an
// endpoint again before each attempt, but weighted selection may choose the
// same endpoint. MaxAttempts limits attempt count, not elapsed time; ctx bounds
// the complete operation.
//
// Pass an empty pool name when exactly one pool is configured. Use DoForDomain
// when fn targets a known host and you want domain-aware endpoint selection.
func (c *Client) Do(ctx context.Context, pool string, fn RequestFunc) (*Response, error) {
	return c.DoForDomain(ctx, pool, "", fn)
}

// DoForDomain is Do with an explicit domain hint. Endpoints recently blocked
// for that domain are deprioritised while keeping full weight elsewhere.
func (c *Client) DoForDomain(
	ctx context.Context,
	poolName string,
	domain string,
	fn RequestFunc,
) (*Response, error) {
	pool, err := c.resolvePool(poolName)
	if err != nil {
		return nil, err
	}

	failThreshold := c.retry.failThreshold()

	var (
		lastResp *Response
		lastErr  error
	)

	err = retry.Do(
		func() error {
			ep, err := pool.getNextEndpointForDomain(domain)
			if err != nil {
				return retry.Unrecoverable(ErrAllProxiesBlocked)
			}

			session, err := ep.acquireSession(ctx)
			if err != nil {
				return fmt.Errorf("acquiring session: %w", err)
			}
			defer ep.releaseSession(session)

			start := time.Now()
			resp, err := fn(session)
			elapsed := time.Since(start)
			ep.recordLatency(elapsed)
			lastResp = resp
			lastErr = err

			if err != nil {
				if c.detector.IsBlockedError(err) {
					c.penalise(pool, ep, domain, failThreshold)
					c.logger.Debug(
						"proxy blocked by error",
						"pool", pool.name,
						"endpoint", ep.index,
						"domain", domain,
						"error", err,
						"latency", elapsed,
					)
					return fmt.Errorf("proxy %d blocked: %w", ep.index, err)
				}
				return err
			}

			if c.detector.IsBlocked(resp) {
				c.penalise(pool, ep, domain, failThreshold)
				c.logger.Debug(
					"proxy blocked by response",
					"pool", pool.name,
					"endpoint", ep.index,
					"domain", domain,
					"status", resp.StatusCode,
					"latency", elapsed,
				)
				return fmt.Errorf(
					"proxy %d received blocking response (status %d)",
					ep.index, resp.StatusCode,
				)
			}

			ep.recordSuccess()
			ep.resetFailCount()
			return nil
		},
		retry.Attempts(uint(c.retry.MaxAttempts)),
		retry.Delay(c.retry.InitialDelay),
		retry.MaxDelay(c.retry.MaxDelay),
		retry.DelayType(retry.CombineDelay(retry.BackOffDelay, retry.RandomDelay)),
		retry.Context(ctx),
		retry.LastErrorOnly(true),
		retry.RetryIf(func(err error) bool {
			return !errors.Is(err, ErrAllProxiesBlocked) && !errors.Is(err, context.Canceled)
		}),
	)

	if err != nil {
		// A blocked response is still worth handing back for inspection.
		if lastResp != nil {
			return lastResp, err
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, err
	}

	if lastResp == nil {
		return nil, errors.New("proxator: request returned a nil response")
	}

	return lastResp, nil
}

// penalise records a block against an endpoint and moves it into cooldown once
// it crosses the consecutive-failure threshold.
func (c *Client) penalise(pool *Pool, ep *endpoint, domain string, failThreshold int32) {
	ep.incrementFailCount()
	pool.recordDomainBlock(ep.index, domain)
	if ep.getFailCount() >= failThreshold {
		ep.markFailed(c.retry.CooldownPeriod, c.retry.MaxCooldown)
	}
}

// Get performs a GET request, deriving the domain hint from targetURL.
func (c *Client) Get(ctx context.Context, pool, targetURL string) (*Response, error) {
	return c.DoForDomain(
		ctx, pool, domain.FromURL(targetURL),
		func(s Session) (*Response, error) {
			return s.Do(ctx, Request{Method: "GET", URL: targetURL})
		},
	)
}

// GetWithHeaders performs a GET request with headers represented in a stable
// order. Whether that order is preserved on the wire is transport-specific.
func (c *Client) GetWithHeaders(
	ctx context.Context,
	pool, targetURL string,
	headers OrderedHeaders,
) (*Response, error) {
	return c.DoForDomain(
		ctx, pool, domain.FromURL(targetURL),
		func(s Session) (*Response, error) {
			return s.Do(ctx, Request{Method: "GET", URL: targetURL, Headers: headers})
		},
	)
}

// Post performs a POST request, deriving the domain hint from targetURL. When
// body is a consumable io.Reader, retries reuse it after the first attempt; use
// Do with a RequestFunc that creates a fresh reader when necessary.
func (c *Client) Post(ctx context.Context, pool, targetURL string, body any) (*Response, error) {
	return c.DoForDomain(
		ctx, pool, domain.FromURL(targetURL),
		func(s Session) (*Response, error) {
			return s.Do(ctx, Request{Method: "POST", URL: targetURL, Body: body})
		},
	)
}

// PostWithHeaders performs a POST request with headers represented in a stable
// order. Whether that order is preserved on the wire is transport-specific.
// When body is a consumable io.Reader, use Do to recreate it for each attempt.
func (c *Client) PostWithHeaders(
	ctx context.Context,
	pool, targetURL string,
	body any,
	headers OrderedHeaders,
) (*Response, error) {
	return c.DoForDomain(
		ctx, pool, domain.FromURL(targetURL),
		func(s Session) (*Response, error) {
			return s.Do(ctx, Request{Method: "POST", URL: targetURL, Body: body, Headers: headers})
		},
	)
}

// Ping fetches probeURL through one endpoint of the named pool and reports
// whether it answered with a non-error status.
//
// probeURL is required: the library will not pick a third-party endpoint to
// poll on your behalf.
func (c *Client) Ping(ctx context.Context, pool, probeURL string) error {
	if probeURL == "" {
		return errors.New("proxator: Ping requires a probe URL")
	}

	p, err := c.resolvePool(pool)
	if err != nil {
		return err
	}

	ep, err := p.getNextEndpoint()
	if err != nil {
		return err
	}

	session, err := ep.acquireSession(ctx)
	if err != nil {
		return fmt.Errorf("acquiring session: %w", err)
	}
	defer ep.releaseSession(session)

	resp, err := session.Do(ctx, Request{Method: "GET", URL: probeURL})
	if err != nil {
		return fmt.Errorf("proxy ping failed: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("proxator: ping returned status %d", resp.StatusCode)
	}

	return nil
}

// Close releases every pool. It is safe to call more than once.
func (c *Client) Close() {
	for _, p := range c.pools {
		p.Close()
	}
}

// Stats returns a snapshot of every pool, keyed by pool name.
func (c *Client) Stats() map[string]PoolStats {
	stats := make(map[string]PoolStats, len(c.pools))
	for name, p := range c.pools {
		stats[name] = p.Stats()
	}
	return stats
}

// MarkEndpointDead permanently retires one endpoint. Dead endpoints never
// recover on their own; call ResetEndpoint to bring one back.
func (c *Client) MarkEndpointDead(pool string, index int) error {
	p, err := c.resolvePool(pool)
	if err != nil {
		return err
	}
	ep, err := p.findEndpoint(index)
	if err != nil {
		return err
	}
	ep.markDead()
	return nil
}

// ResetEndpoint returns one endpoint to service and clears its failure history.
func (c *Client) ResetEndpoint(pool string, index int) error {
	p, err := c.resolvePool(pool)
	if err != nil {
		return err
	}
	ep, err := p.findEndpoint(index)
	if err != nil {
		return err
	}
	ep.setState(StateAlive)
	ep.resetFailCount()
	ep.cooldownTier.Store(0)
	return nil
}
