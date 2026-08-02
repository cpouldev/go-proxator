package proxator

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsTransient_Nil(t *testing.T) {
	t.Parallel()

	if IsTransient(nil) {
		t.Fatal("expected nil error to not be transient")
	}

	var nilClassifier *TransientClassifier
	if nilClassifier.IsTransient(errors.New("proxy tunnel failed")) {
		t.Fatal("expected nil classifier to report false")
	}
	if nilClassifier.IsTransient(nil) {
		t.Fatal("expected nil classifier with nil error to report false")
	}
}

func TestIsTransient_Sentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"all proxies blocked", ErrAllProxiesBlocked, true},
		{
			"wrapped all proxies blocked",
			fmt.Errorf("fetching listing page: %w", ErrAllProxiesBlocked),
			true,
		},
		{
			"deeply wrapped all proxies blocked",
			fmt.Errorf("scrape job: %w", fmt.Errorf("fetching listing page: %w", ErrAllProxiesBlocked)),
			true,
		},
		// Configuration errors never resolve on their own.
		{"no pools", ErrNoPools, false},
		{"pool not found", ErrPoolNotFound, false},
		{"ambiguous pool", ErrAmbiguousPool, false},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				if got := IsTransient(tt.err); got != tt.want {
					t.Fatalf("IsTransient(%v) = %t, want %t", tt.err, got, tt.want)
				}
			},
		)
	}
}

// Tier 1: strings that can only come from proxy infrastructure. They are
// transient on sight, with no corroborating context required.
func TestIsTransient_UnambiguousProxyFailures(t *testing.T) {
	t.Parallel()

	messages := []string{
		"failed to dial: proxy tunnel failed: 502 Bad Gateway",
		"All attempts fail: #1: failed to dial: proxy tunnel failed: 502 Bad Gateway " +
			"#2: failed to dial: proxy tunnel failed: 502 Bad Gateway",
		"fetching listing page: All attempts fail: #1: failed to dial: proxy tunnel failed: 502 Bad Gateway",
		"scrape job: fetching listing page: proxy tunnel failed: 503 Service Unavailable",
		"proxator: all proxies are blocked or unavailable",
		"proxy 2 received blocking response (status 403)",
		"proxy 5 received blocking response (status 429)",
		"proxy 1 received blocking response (status 503)",
	}

	for _, msg := range messages {
		if !IsTransient(errors.New(msg)) {
			t.Errorf("expected unambiguous proxy failure to be transient: %s", msg)
		}
	}
}

// Tier 2: generic network failures. Transient only when the error also carries
// proxy context.
func TestIsTransient_NetworkFailures_WithProxyContext(t *testing.T) {
	t.Parallel()

	messages := []string{
		"proxy 3 blocked: connection refused",
		"proxy 1: fetching listing page: connection reset by peer",
		"proxator: acquiring session: i/o timeout",
		"proxy dial: TLS handshake timeout",
		"proxy resolve: no such host",
		"fetching listing page through proxy: 502 bad gateway",
		"proxy 4: 429 too many requests",
	}

	for _, msg := range messages {
		if !IsTransient(errors.New(msg)) {
			t.Errorf("expected network error with proxy context to be transient: %s", msg)
		}
	}
}

// The whole point of the tiering: a bare network failure is just as likely to be
// the database or the cache as it is to be a proxy, so it must not be swallowed.
func TestIsTransient_NetworkFailures_WithoutProxyContext(t *testing.T) {
	t.Parallel()

	messages := []string{
		"connection refused",
		"connection reset by peer",
		"i/o timeout",
		"TLS handshake timeout",
		"no such host",
		"pq: connection refused",
		"dial tcp 10.0.0.5:5432: connect: connection refused",
		"redis: i/o timeout",
	}

	for _, msg := range messages {
		if IsTransient(errors.New(msg)) {
			t.Errorf("expected bare network error to NOT be transient: %s", msg)
		}
	}
}

// Tier 3: blocking and rate-limit signals, again gated on proxy context.
func TestIsTransient_BlockingSignals_WithProxyContext(t *testing.T) {
	t.Parallel()

	messages := []string{
		"proxy 2 blocked: status 403",
		"proxy 5: status 429 rate limited",
		"proxy 7: status 407 proxy authentication required",
		"proxator: cloudflare challenge detected",
		"proxy tunnel: captcha required",
		`proxy 4: fetching listing page failed with status 403: <!DOCTYPE html><html lang="en-US">` +
			`<head><title>Just a moment...</title></head><body>cloudflare challenge</body></html>`,
	}

	for _, msg := range messages {
		if !IsTransient(errors.New(msg)) {
			t.Errorf("expected blocking error with proxy context to be transient: %s", msg)
		}
	}
}

func TestIsTransient_BlockingSignals_WithoutProxyContext(t *testing.T) {
	t.Parallel()

	messages := []string{
		"upstream API returned status 403: forbidden",
		"metadata service: cloudflare block",
		"status 429 from the internal rate limiter",
		"captcha solver returned no answer",
	}

	for _, msg := range messages {
		if IsTransient(errors.New(msg)) {
			t.Errorf("expected blocking error without proxy context to NOT be transient: %s", msg)
		}
	}
}

// Application and storage errors must reach the error tracker untouched.
func TestIsTransient_ApplicationErrors(t *testing.T) {
	t.Parallel()

	messages := []string{
		"failed to unmarshal response: unexpected character '<'",
		"json: cannot unmarshal string into Go struct field",
		"fetching listing page: unexpected status 200 with empty body",
		"listing lookup returned no data for ABC123",
		"permanent failure: unsupported payload",
		"persisting record: pq: duplicate key value violates unique constraint",
		"inserting row: pq: connection refused",
		"updating record: context deadline exceeded",
		"loading record: sql: no rows in result set",
	}

	for _, msg := range messages {
		if IsTransient(errors.New(msg)) {
			t.Errorf("expected application error to NOT be transient: %s", msg)
		}
	}
}

func TestIsTransient_WrappedErrors(t *testing.T) {
	t.Parallel()

	inner := errors.New("failed to dial: proxy tunnel failed: 502 Bad Gateway")
	wrapped := fmt.Errorf(
		"scrape job: %w",
		fmt.Errorf("fetching search results: %w", fmt.Errorf("fetching listing page: %w", inner)),
	)

	if !IsTransient(wrapped) {
		t.Fatalf("expected deeply wrapped proxy error to be transient: %s", wrapped)
	}
}

// A caller whose scraper errors never mention a proxy can inject its own
// vocabulary instead of widening the package defaults.
func TestNewTransientClassifierWith_CustomContextPatterns(t *testing.T) {
	t.Parallel()

	custom := NewTransientClassifierWith(
		TransientConfig{ContextPatterns: []string{"listingfetcher", "crawler"}},
	)

	ownVocabulary := errors.New("crawler: connection refused")
	if !custom.IsTransient(ownVocabulary) {
		t.Fatalf("expected custom context pattern to make %q transient", ownVocabulary)
	}
	if IsTransient(ownVocabulary) {
		t.Fatalf("expected the default classifier to leave %q alone", ownVocabulary)
	}

	// Custom context patterns replace the defaults rather than extending them.
	defaultVocabulary := errors.New("proxy 3: connection refused")
	if custom.IsTransient(defaultVocabulary) {
		t.Fatalf("expected custom context patterns to replace the defaults, %q matched", defaultVocabulary)
	}

	// Tier 1 still needs no context at all.
	if !custom.IsTransient(errors.New("proxy tunnel failed")) {
		t.Fatal("expected unambiguous tier to stay context-free")
	}
}

func TestNewTransientClassifierWith_CustomTiers(t *testing.T) {
	t.Parallel()

	custom := NewTransientClassifierWith(
		TransientConfig{
			UnambiguousPatterns: []string{"UPSTREAM GATEWAY EXPLODED"},
			AmbiguousPatterns:   []string{"socket hangup"},
			BlockingPatterns:    []string{"bot wall"},
			ContextPatterns:     []string{"crawler"},
		},
	)

	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{"custom unambiguous needs no context", "Upstream Gateway Exploded", true},
		{"custom ambiguous with context", "crawler: socket hangup", true},
		{"custom ambiguous without context", "socket hangup", false},
		{"custom blocking with context", "crawler: hit a bot wall", true},
		{"custom blocking without context", "hit a bot wall", false},
		{"default patterns are replaced", "proxy tunnel failed", false},
		{"default ambiguous is replaced", "crawler: connection refused", false},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				if got := custom.IsTransient(errors.New(tt.msg)); got != tt.want {
					t.Fatalf("IsTransient(%q) = %t, want %t", tt.msg, got, tt.want)
				}
			},
		)
	}
}

func TestNewTransientClassifier_MatchesPackageDefaults(t *testing.T) {
	t.Parallel()

	classifier := NewTransientClassifier()

	messages := []string{
		"failed to dial: proxy tunnel failed: 502 Bad Gateway",
		"proxy 3 blocked: connection refused",
		"pq: connection refused",
		"upstream API returned status 403: forbidden",
	}

	for _, msg := range messages {
		err := errors.New(msg)
		if got, want := classifier.IsTransient(err), IsTransient(err); got != want {
			t.Fatalf("classifier and package helper disagree on %q: %t vs %t", msg, got, want)
		}
	}
}

func TestNewTransientClassifierWith_IsCaseInsensitive(t *testing.T) {
	t.Parallel()

	if !IsTransient(errors.New("FAILED TO DIAL: PROXY TUNNEL FAILED: 502 BAD GATEWAY")) {
		t.Fatal("expected matching to be case-insensitive")
	}
	if !IsTransient(errors.New("PROXY 3 BLOCKED: CONNECTION REFUSED")) {
		t.Fatal("expected context matching to be case-insensitive")
	}
}
