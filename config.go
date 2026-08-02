package proxator

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"
)

// State is the lifecycle state of a single proxy endpoint.
type State int32

const (
	// StateAlive means the endpoint is eligible for selection.
	StateAlive State = iota
	// StateDead means the endpoint was permanently retired and will not
	// recover without an explicit ResetEndpoint call.
	StateDead
	// StateCooldown means the endpoint failed recently and is temporarily
	// excluded. It returns to StateAlive when its cooldown elapses.
	StateCooldown
)

func (s State) String() string {
	switch s {
	case StateAlive:
		return "alive"
	case StateDead:
		return "dead"
	case StateCooldown:
		return "cooldown"
	default:
		return "unknown"
	}
}

const (
	defaultSessionPoolSize          = 15
	defaultFailThreshold      int32 = 3
	defaultHealthCheckTimeout       = 15 * time.Second
)

// Sentinel errors returned by the package.
var (
	// ErrAllProxiesBlocked is returned when every endpoint in the selected
	// pool is dead or in cooldown. It is not retried.
	ErrAllProxiesBlocked = errors.New("proxator: all proxies are blocked or unavailable")

	// ErrPoolNotFound is returned when the named pool does not exist.
	ErrPoolNotFound = errors.New("proxator: pool not found")

	// ErrNoPools is returned by New when Config contains no pools.
	ErrNoPools = errors.New("proxator: no pools configured")

	// ErrAmbiguousPool is returned when a pool name is omitted but more than
	// one pool is configured.
	ErrAmbiguousPool = errors.New("proxator: pool name required when multiple pools are configured")

	// ErrClientClosed is returned when work is attempted after the endpoint
	// sessions have been closed.
	ErrClientClosed = errors.New("proxator: client is closed")
)

// Config configures a Client.
type Config struct {
	// Pools are the named proxy pools. At least one pool is required and
	// every pool must carry a unique, non-empty Name.
	Pools []PoolConfig

	// Retry governs attempts, backoff, and endpoint cooldown across all pools.
	// Each non-positive field is replaced by its value from DefaultRetryConfig.
	Retry RetryConfig

	// Detector classifies responses and transport errors as blocked.
	// Defaults to NewBlockDetector.
	Detector *BlockDetector

	// SessionFactory creates the sessions held by every endpoint. It defaults
	// to the built-in Azure TLS adapter. Client construction calls the factory
	// SessionPoolSize times per endpoint and owns every session returned
	// successfully. Supply another factory to use a custom HTTP transport or an
	// in-memory test implementation.
	SessionFactory SessionFactory

	// Logger receives operational events: cooldown entry and recovery, dead
	// endpoints, and health-check failures. Defaults to slog.Default.
	Logger *slog.Logger
}

// PoolConfig describes one named pool of interchangeable proxy endpoints.
type PoolConfig struct {
	// Name identifies the pool in Client calls and in Stats. Required.
	Name string

	// Endpoints are fully-formed proxy URLs, for example
	// "http://user:pass@gate.example.net:8000". At least one is required.
	// Supported URL schemes depend on Config.SessionFactory. Use
	// StickyEndpoints to generate an indexed sticky-session set.
	Endpoints []string

	// SessionPoolSize is the number of concurrent transport sessions held per
	// endpoint. Defaults to 15. This is also the rate limiter's burst size.
	SessionPoolSize int

	// RequestsPerSecond caps the sustained request rate per endpoint.
	// Zero means unlimited.
	RequestsPerSecond float64

	// HealthCheckInterval enables background health checks when greater than
	// zero. HealthCheckURL is then required.
	HealthCheckInterval time.Duration

	// HealthCheckURL is fetched through every alive endpoint on each
	// health-check tick.
	//
	// There is deliberately no default. Pick a URL you control or are content
	// to poll indefinitely — a library that silently polls a third party on
	// every user's behalf is a bad neighbour.
	HealthCheckURL string

	// HealthCheckTimeout bounds a single health-check request. Defaults to 15s.
	HealthCheckTimeout time.Duration

	// DomainBlockTTL controls how long a block lowers an endpoint's weight for
	// one target domain. Defaults to 5 minutes.
	DomainBlockTTL time.Duration

	// DomainBlockPenalty is the weight multiplier for an endpoint recently
	// blocked by one domain. It must be greater than 0 and at most 1. The zero
	// value selects the default, 0.1.
	DomainBlockPenalty float64
}

func (p PoolConfig) validate() error {
	if p.Name == "" {
		return errors.New("proxator: pool name is required")
	}
	if len(p.Endpoints) == 0 {
		return fmt.Errorf("proxator: pool %q has no endpoints", p.Name)
	}
	for i, ep := range p.Endpoints {
		if ep == "" {
			return fmt.Errorf("proxator: pool %q endpoint %d is empty", p.Name, i)
		}
		if _, err := url.Parse(ep); err != nil {
			// url.Error stringifies the whole URL, which may embed credentials.
			var uerr *url.Error
			if errors.As(err, &uerr) {
				err = uerr.Err
			}
			return fmt.Errorf("proxator: pool %q endpoint %d is not a valid URL: %w", p.Name, i, err)
		}
	}
	if p.HealthCheckInterval > 0 && p.HealthCheckURL == "" {
		return fmt.Errorf("proxator: pool %q enables health checks but sets no HealthCheckURL", p.Name)
	}
	if p.DomainBlockTTL < 0 {
		return fmt.Errorf("proxator: pool %q DomainBlockTTL must not be negative", p.Name)
	}
	if p.DomainBlockPenalty < 0 || p.DomainBlockPenalty > 1 {
		return fmt.Errorf(
			"proxator: pool %q DomainBlockPenalty must be between 0 and 1",
			p.Name,
		)
	}
	return nil
}

func (p PoolConfig) sessionPoolSize() int {
	if p.SessionPoolSize <= 0 {
		return defaultSessionPoolSize
	}
	return p.SessionPoolSize
}

func (p PoolConfig) healthCheckTimeout() time.Duration {
	if p.HealthCheckTimeout <= 0 {
		return defaultHealthCheckTimeout
	}
	return p.HealthCheckTimeout
}

// RetryConfig configures retry, backoff and endpoint cooldown behaviour.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts per request, including the
	// initial attempt. An endpoint is selected again before every attempt. A
	// non-positive value selects the default, 3.
	MaxAttempts int

	// InitialDelay is the base delay between attempts. Combined with
	// exponential backoff and jitter.
	InitialDelay time.Duration

	// MaxDelay caps the delay between attempts.
	MaxDelay time.Duration

	// CooldownPeriod is the base cooldown applied when an endpoint crosses
	// FailThreshold. Each consecutive cooldown doubles it.
	CooldownPeriod time.Duration

	// MaxCooldown caps the escalated cooldown. The effective duration never
	// exceeds the lower of MaxCooldown and 16 times CooldownPeriod. A
	// non-positive value selects the default, 5 minutes.
	MaxCooldown time.Duration

	// FailThreshold is the number of consecutive failures before an endpoint
	// enters cooldown. Defaults to 3.
	FailThreshold int32
}

// DefaultRetryConfig returns sensible defaults for retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:    3,
		InitialDelay:   500 * time.Millisecond,
		MaxDelay:       5 * time.Second,
		CooldownPeriod: 30 * time.Second,
		MaxCooldown:    5 * time.Minute,
		FailThreshold:  defaultFailThreshold,
	}
}

// withDefaults fills unset fields from DefaultRetryConfig so that a zero
// RetryConfig behaves identically to the default one.
func (r RetryConfig) withDefaults() RetryConfig {
	d := DefaultRetryConfig()
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = d.MaxAttempts
	}
	if r.InitialDelay <= 0 {
		r.InitialDelay = d.InitialDelay
	}
	if r.MaxDelay <= 0 {
		r.MaxDelay = d.MaxDelay
	}
	if r.CooldownPeriod <= 0 {
		r.CooldownPeriod = d.CooldownPeriod
	}
	if r.MaxCooldown <= 0 {
		r.MaxCooldown = d.MaxCooldown
	}
	if r.FailThreshold <= 0 {
		r.FailThreshold = d.FailThreshold
	}
	return r
}

func (r RetryConfig) failThreshold() int32 {
	if r.FailThreshold <= 0 {
		return defaultFailThreshold
	}
	return r.FailThreshold
}

// StickyOptions describes an indexed sticky-session endpoint set.
type StickyOptions struct {
	// Scheme defaults to "http". Support for other schemes depends on the
	// configured SessionFactory; the default adapter supports SOCKS5.
	Scheme string

	// Username is the base username, before index substitution.
	Username string

	// Password is used verbatim for every generated endpoint.
	Password string

	// Host is the proxy gateway hostname. Required.
	Host string

	// Port is the proxy gateway port. Required.
	Port string

	// Count is how many endpoints to generate. Required, must be positive.
	Count int

	// UsernameFormat renders each endpoint's username from the base username
	// and its 1-based index. Defaults to "%s-%d", producing user-1, user-2,
	// ... — the sticky-session convention used by most residential providers.
	//
	// Set it to "%s" to reuse the same username for every endpoint, or to
	// something like "%s_session_%d" to match a different provider.
	UsernameFormat string
}

// StickyEndpoints builds Count proxy URLs that differ only by an indexed
// username, which is how most residential proxy providers expose distinct
// sticky sessions behind a single gateway host.
//
//	eps, err := proxator.StickyEndpoints(proxator.StickyOptions{
//	    Username: "user", Password: "pass",
//	    Host: "gate.example.net", Port: "8000", Count: 10,
//	})
//	// http://user-1:pass@gate.example.net:8000 ... user-10
//
// If your provider hands you a plain list of proxy URLs instead, skip this
// entirely and assign them to PoolConfig.Endpoints directly.
func StickyEndpoints(opts StickyOptions) ([]string, error) {
	if opts.Host == "" || opts.Port == "" {
		return nil, errors.New("proxator: sticky endpoints require Host and Port")
	}
	if opts.Count <= 0 {
		return nil, errors.New("proxator: sticky endpoints require a positive Count")
	}

	scheme := opts.Scheme
	if scheme == "" {
		scheme = "http"
	}
	format := opts.UsernameFormat
	if format == "" {
		format = "%s-%d"
	}

	endpoints := make([]string, 0, opts.Count)
	for i := 1; i <= opts.Count; i++ {
		u := &url.URL{
			Scheme: scheme,
			Host:   opts.Host + ":" + opts.Port,
		}
		if opts.Username != "" {
			name := renderUsername(format, opts.Username, i)
			if opts.Password != "" {
				u.User = url.UserPassword(name, opts.Password)
			} else {
				u.User = url.User(name)
			}
		}
		endpoints = append(endpoints, u.String())
	}
	return endpoints, nil
}

// renderUsername applies format to username and index, tolerating formats that
// reference only the username (for example "%s").
func renderUsername(format, username string, index int) string {
	rendered := fmt.Sprintf(format, username, index)
	// A format with a single verb leaves "%!(EXTRA int=N)" behind.
	if idx := indexOfExtra(rendered); idx >= 0 {
		return rendered[:idx]
	}
	return rendered
}

func indexOfExtra(s string) int {
	const marker = "%!(EXTRA"
	for i := 0; i+len(marker) <= len(s); i++ {
		if s[i:i+len(marker)] == marker {
			return i
		}
	}
	return -1
}
