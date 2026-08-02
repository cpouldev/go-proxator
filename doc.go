// Package proxator routes HTTP requests through pools of rotating proxies,
// scoring each endpoint on live performance and taking failing ones out of
// rotation automatically.
//
// By default it uses azuretls-client with a Chrome TLS fingerprint and TLS
// certificate verification enabled. The public transport contracts also allow
// callers to select the standard-library HTTPFactory or provide their own
// SessionFactory.
//
// # Selection
//
// Endpoints are chosen by weighted random selection rather than round-robin.
// An endpoint's weight is its success rate divided by its average latency, so
// faster and more reliable endpoints receive proportionally more traffic. For
// the first few requests the weight blends toward a neutral baseline, which
// stops a newly added endpoint from either monopolising traffic or being
// written off because its first sample happened to be slow.
//
// # Domain-aware blocking
//
// Blocks are recorded per endpoint *and* per target domain. If one host starts
// serving challenge pages to endpoint 3, endpoint 3 is deprioritised for that
// host while keeping full weight everywhere else — a per-endpoint circuit
// breaker would have removed it from rotation completely. PoolConfig controls
// the penalty multiplier and expiry through DomainBlockPenalty and
// DomainBlockTTL.
//
// # Cooldown
//
// After a configurable number of consecutive failures an endpoint enters
// cooldown. Each consecutive cooldown doubles the previous duration, capped at
// 16x the base period and at RetryConfig.MaxCooldown. A single success resets
// the escalation.
//
// # Transports
//
// Config.SessionFactory defaults to the Azure TLS adapter, which supports HTTP
// and HTTPS proxies and is tested with SOCKS5 proxies. HTTPFactory uses Go's
// standard net/http transport and supports HTTP and HTTPS proxy URLs; it uses
// normal certificate verification and does not guarantee header field order on
// the wire. Proxy scheme support is otherwise factory-specific.
//
// Client construction eagerly creates SessionPoolSize sessions for each
// endpoint. The Client owns and closes every session returned successfully by
// the factory. Custom Session implementations must honor the context passed to
// Do, fully buffer response bodies, and return a non-nil Response with a nil
// error.
//
// # Retries and cancellation
//
// RetryConfig.MaxAttempts includes the initial request. Every callback error is
// generally eligible for retry, but only errors and responses classified as
// blocked affect domain penalties and endpoint cooldown. Each attempt selects
// an endpoint again, though weighted selection can choose the same endpoint.
//
// Retries have no separate elapsed-time budget. Use context.WithTimeout to
// bound the operation, and pass that context to Session.Do from a RequestFunc.
// If attempts are exhausted after a blocking response, Client.Do can return
// both the Response and a non-nil error.
//
// # Usage
//
//	endpoints, err := proxator.StickyEndpoints(proxator.StickyOptions{
//	    Username: "user",
//	    Password: "pass",
//	    Host:     "gate.example.net",
//	    Port:     "8000",
//	    Count:    10,
//	})
//	if err != nil {
//	    return err
//	}
//
//	client, err := proxator.New(proxator.Config{
//	    Pools: []proxator.PoolConfig{{
//	        Name:              "main",
//	        Endpoints:         endpoints,
//	        RequestsPerSecond: 5,
//	    }},
//	})
//	if err != nil {
//	    return err
//	}
//	defer client.Close()
//
//	resp, err := client.Get(ctx, "main", "https://example.com")
//
// With exactly one pool configured the name may be omitted:
//
//	resp, err := client.Get(ctx, "", "https://example.com")
package proxator
