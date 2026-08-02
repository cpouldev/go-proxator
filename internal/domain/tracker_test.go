package domain

import (
	"sync"
	"testing"
	"time"
)

func TestTrackerRecordAndPenalty(t *testing.T) {
	t.Parallel()

	tracker := &Tracker{}
	if got := tracker.PenaltyFor(1, "example.com"); got != 1.0 {
		t.Fatalf("penalty with no blocks = %f, want 1.0", got)
	}

	tracker.RecordBlock(1, "example.com")
	if got := tracker.PenaltyFor(1, "example.com"); got != DefaultBlockPenalty {
		t.Fatalf("penalty after a block = %f, want %f", got, DefaultBlockPenalty)
	}
	if got := tracker.PenaltyFor(2, "example.com"); got != 1.0 {
		t.Fatalf("penalty for another endpoint = %f, want 1.0", got)
	}
	if got := tracker.PenaltyFor(1, "example.org"); got != 1.0 {
		t.Fatalf("penalty for another domain = %f, want 1.0", got)
	}
}

func TestTrackerCustomSettings(t *testing.T) {
	t.Parallel()

	tracker := NewTracker(time.Hour, 0.25)
	tracker.RecordBlock(1, "example.com")

	if got := tracker.PenaltyFor(1, "example.com"); got != 0.25 {
		t.Fatalf("penalty = %f, want 0.25", got)
	}

	key := blockKey{endpointIndex: 1, domain: "example.com"}
	tracker.blocks.Store(key, &blockEntry{blockedAt: time.Now().Add(-30 * time.Minute)})
	if got := tracker.PenaltyFor(1, "example.com"); got != 0.25 {
		t.Fatalf("penalty before custom TTL = %f, want 0.25", got)
	}

	tracker.blocks.Store(key, &blockEntry{blockedAt: time.Now().Add(-2 * time.Hour)})
	if got := tracker.PenaltyFor(1, "example.com"); got != 1.0 {
		t.Fatalf("penalty after custom TTL = %f, want 1.0", got)
	}
}

func TestTrackerEmptyDomain(t *testing.T) {
	t.Parallel()

	tracker := &Tracker{}
	tracker.RecordBlock(1, "")
	if got := tracker.PenaltyFor(1, ""); got != 1.0 {
		t.Fatalf("penalty for empty domain = %f, want 1.0", got)
	}
}

func TestTrackerMultipleBlocksAreIdempotent(t *testing.T) {
	t.Parallel()

	tracker := &Tracker{}
	tracker.RecordBlock(1, "shop.example.net")
	tracker.RecordBlock(1, "shop.example.net")
	tracker.RecordBlock(1, "shop.example.net")

	if got := tracker.PenaltyFor(1, "shop.example.net"); got != DefaultBlockPenalty {
		t.Fatalf("penalty = %f, want %f", got, DefaultBlockPenalty)
	}
}

func TestTrackerBlocksExpire(t *testing.T) {
	t.Parallel()

	tracker := &Tracker{}
	key := blockKey{endpointIndex: 1, domain: "example.com"}
	tracker.blocks.Store(key, &blockEntry{blockedAt: time.Now().Add(-DefaultBlockTTL - time.Second)})

	if got := tracker.PenaltyFor(1, "example.com"); got != 1.0 {
		t.Fatalf("expired penalty = %f, want 1.0", got)
	}
	if _, ok := tracker.blocks.Load(key); ok {
		t.Fatal("expired entry was not removed")
	}
}

func TestTrackerBlockJustInsideTTL(t *testing.T) {
	t.Parallel()

	tracker := &Tracker{}
	tracker.blocks.Store(
		blockKey{endpointIndex: 3, domain: "example.com"},
		&blockEntry{blockedAt: time.Now().Add(-DefaultBlockTTL + time.Minute)},
	)
	if got := tracker.PenaltyFor(3, "example.com"); got != DefaultBlockPenalty {
		t.Fatalf("live penalty = %f, want %f", got, DefaultBlockPenalty)
	}
}

func TestFromURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rawURL string
		want   string
	}{
		{"https://www.example.com/catalogue/123", "www.example.com"},
		{"http://api.example.org/v1/search", "api.example.org"},
		{"https://shop.example.net:8080/path", "shop.example.net"},
		{"not-a-url", ""},
		{"", ""},
	}

	for _, test := range tests {
		t.Run(test.rawURL, func(t *testing.T) {
			t.Parallel()
			if got := FromURL(test.rawURL); got != test.want {
				t.Fatalf("FromURL(%q) = %q, want %q", test.rawURL, got, test.want)
			}
		})
	}
}

func TestTrackerConcurrent(t *testing.T) {
	t.Parallel()

	tracker := &Tracker{}
	domains := []string{"example.com", "example.org", "shop.example.net"}

	var waitGroup sync.WaitGroup
	for i := 0; i < 100; i++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			endpointIndex := index%3 + 1
			domain := domains[index%len(domains)]
			tracker.RecordBlock(endpointIndex, domain)
			_ = tracker.PenaltyFor(endpointIndex, domain)
		}(i)
	}
	waitGroup.Wait()

	if got := tracker.PenaltyFor(1, "example.com"); got != DefaultBlockPenalty {
		t.Fatalf("concurrent penalty = %f, want %f", got, DefaultBlockPenalty)
	}
}
