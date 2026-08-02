package proxator

import (
	"errors"
	"strings"
)

// DefaultUnambiguousPatterns are error substrings that can only originate from
// proxy infrastructure. They are classified as transient on sight.
var DefaultUnambiguousPatterns = []string{
	"proxy tunnel failed",
	"all proxies are blocked",
	"blocking response",
}

// DefaultAmbiguousPatterns are generic network failures. Any TCP connection in
// the process can produce these — your database, your cache, your queue — so
// they are only transient when the error also carries proxy context.
var DefaultAmbiguousPatterns = []string{
	"failed to dial",
	"connection refused",
	"connection reset",
	"i/o timeout",
	"tls handshake timeout",
	"no such host",
	"502 bad gateway",
	"503 service unavailable",
	"429 too many requests",
}

// DefaultBlockingPatterns indicate target-site blocking. Like the ambiguous
// set, they require proxy context: a bare "status 403" is just as likely to be
// an expired API key as a bot wall.
var DefaultBlockingPatterns = []string{
	"just a moment",
	"cloudflare",
	"captcha",
	"status 403",
	"status 407",
	"status 429",
	"status 502",
	"status 503",
}

// DefaultContextPatterns confirm that an error came from proxied traffic.
//
// Keep these tight. Every string here widens what the ambiguous and blocking
// tiers will swallow, and the whole point of the tiering is to avoid
// misclassifying a database outage as a proxy hiccup. Add the names of your
// own scraping call sites here if their errors do not already mention a proxy.
var DefaultContextPatterns = []string{
	"proxy",
	"proxator",
}

// TransientConfig customises a TransientClassifier. Empty fields fall back to
// the corresponding package defaults.
type TransientConfig struct {
	UnambiguousPatterns []string
	AmbiguousPatterns   []string
	BlockingPatterns    []string
	ContextPatterns     []string
}

// TransientClassifier decides whether an error is a transient external failure
// that will clear on its own, and therefore should be retried and kept out of
// your error tracker rather than paged on.
//
// It works in three tiers, because a flat substring list produces false
// positives that are worse than the noise it removes:
//
//  1. Unambiguous proxy failures       — always transient.
//  2. Generic network failures         — transient only alongside proxy context.
//  3. Blocking and rate-limit signals  — transient only alongside proxy context.
//
// Without tier 2's context requirement, a "connection refused" from your
// primary database would be silently classified as a proxy blip.
type TransientClassifier struct {
	unambiguous []string
	ambiguous   []string
	blocking    []string
	contextual  []string
}

// NewTransientClassifier returns a classifier using the package defaults.
func NewTransientClassifier() *TransientClassifier {
	return NewTransientClassifierWith(TransientConfig{})
}

// NewTransientClassifierWith returns a classifier built from cfg, falling back
// to the package defaults for any field left empty.
func NewTransientClassifierWith(cfg TransientConfig) *TransientClassifier {
	// A nil field selects the defaults; an empty non-nil field disables that
	// tier, which callers cannot express if the fallback triggers on length.
	pick := func(given, fallback []string) []string {
		src := given
		if src == nil {
			src = fallback
		}
		out := make([]string, len(src))
		for i, s := range src {
			out[i] = strings.ToLower(s)
		}
		return out
	}

	return &TransientClassifier{
		unambiguous: pick(cfg.UnambiguousPatterns, DefaultUnambiguousPatterns),
		ambiguous:   pick(cfg.AmbiguousPatterns, DefaultAmbiguousPatterns),
		blocking:    pick(cfg.BlockingPatterns, DefaultBlockingPatterns),
		contextual:  pick(cfg.ContextPatterns, DefaultContextPatterns),
	}
}

// IsTransient reports whether err is a transient external failure.
func (c *TransientClassifier) IsTransient(err error) bool {
	if c == nil || err == nil {
		return false
	}

	if errors.Is(err, ErrAllProxiesBlocked) {
		return true
	}

	msg := strings.ToLower(err.Error())

	// Tier 1: unambiguous proxy infrastructure failures.
	if containsAny(msg, c.unambiguous) {
		return true
	}

	// Tiers 2 and 3 both need corroborating proxy context.
	if !containsAny(msg, c.contextual) {
		return false
	}

	return containsAny(msg, c.ambiguous) || containsAny(msg, c.blocking)
}

// IsTransient reports whether err is a transient external failure, using the
// Default pattern sets as they are configured at call time. Build a
// TransientClassifier once with NewTransientClassifier when classifying errors
// in bulk, or NewTransientClassifierWith to tune the pattern sets.
func IsTransient(err error) bool {
	return NewTransientClassifier().IsTransient(err)
}

func containsAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
