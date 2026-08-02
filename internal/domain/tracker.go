// Package domain contains domain-aware routing state used by proxator.
package domain

import (
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBlockTTL is how long a per-domain block penalty lasts.
	DefaultBlockTTL = 5 * time.Minute

	// DefaultBlockPenalty is the weight multiplier applied to an endpoint that
	// was recently blocked for a target domain.
	DefaultBlockPenalty = 0.1
)

// Tracker remembers which endpoints were recently blocked by target domains.
// A block for one domain does not reduce the endpoint's weight for another.
type Tracker struct {
	blocks  sync.Map // blockKey -> *blockEntry
	ttl     time.Duration
	penalty float64
}

// NewTracker returns a Tracker with the supplied block lifetime and weight
// penalty. Non-positive values select the package defaults.
func NewTracker(ttl time.Duration, penalty float64) *Tracker {
	if ttl <= 0 {
		ttl = DefaultBlockTTL
	}
	if penalty <= 0 {
		penalty = DefaultBlockPenalty
	}
	return &Tracker{ttl: ttl, penalty: penalty}
}

// blockEntry is immutable once stored. Updates replace the pointer so reads
// remain race-free without a mutex.
type blockEntry struct {
	blockedAt time.Time
}

type blockKey struct {
	endpointIndex int
	domain        string
}

// RecordBlock notes that an endpoint was blocked for a domain.
func (t *Tracker) RecordBlock(endpointIndex int, domain string) {
	if domain == "" {
		return
	}
	key := blockKey{endpointIndex: endpointIndex, domain: domain}
	t.blocks.Store(key, &blockEntry{blockedAt: time.Now()})
}

// PenaltyFor returns the current weight multiplier for an endpoint/domain
// pair. Expired block records are removed on read.
func (t *Tracker) PenaltyFor(endpointIndex int, domain string) float64 {
	if domain == "" {
		return 1.0
	}
	key := blockKey{endpointIndex: endpointIndex, domain: domain}
	existing, ok := t.blocks.Load(key)
	if !ok {
		return 1.0
	}
	entry, ok := existing.(*blockEntry)
	if !ok {
		return 1.0
	}
	ttl := t.ttl
	if ttl <= 0 {
		ttl = DefaultBlockTTL
	}
	if time.Since(entry.blockedAt) > ttl {
		t.blocks.CompareAndDelete(key, existing)
		return 1.0
	}
	penalty := t.penalty
	if penalty <= 0 {
		penalty = DefaultBlockPenalty
	}
	return penalty
}

// FromURL returns the hostname of rawURL, or an empty string when it has no
// parseable host.
func FromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	// Hostname aliases rawURL's backing array; tracker keys outlive the request,
	// so clone to avoid pinning the full URL (query string included).
	return strings.Clone(u.Hostname())
}
