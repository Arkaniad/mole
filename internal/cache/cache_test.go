package cache_test

import (
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/cache"
)

// This file exists because cache.go had none. key_test.go covered key.go only,
// so the documented first-write-wins rule, the nil-receiver contract, and the
// hit/miss counters the CLI prints were all unverified — mutating Put to
// overwrite and deleting both counter increments left the whole suite green.

// TestFirstWriteWins. A later run of the same artifact produced the same
// document, so overwriting churns without changing anything — and if the two
// ever DID differ, the later one is the surprise, not the earlier.
func TestFirstWriteWins(t *testing.T) {
	c := cache.New()
	c.Put(&cache.Entry{Key: "u:x", Text: "original", Title: "first"})
	c.Put(&cache.Entry{Key: "u:x", Text: "replacement", Title: "second"})

	got, ok := c.Get("u:x")
	if !ok {
		t.Fatal("the entry vanished")
	}
	if got.Text != "original" || got.Title != "first" {
		t.Errorf("got %q/%q, want the first write to win", got.Text, got.Title)
	}
}

// TestNilCacheIsAMiss, so callers need no conditional at every site — which is
// how the actor and executor are written.
func TestNilCacheIsAMiss(t *testing.T) {
	var c *cache.Cache

	if _, ok := c.Get("u:x"); ok {
		t.Error("a nil cache reported a hit")
	}
	c.Put(&cache.Entry{Key: "u:x", Text: "t"}) // must not panic
	if s := c.Stats(); s.Entries != 0 || s.Hits != 0 || s.Misses != 0 {
		t.Errorf("a nil cache reported stats: %+v", s)
	}
}

// TestEmptyKeysAreNeverStored. QueryKey returns "" for a query with no usable
// words; storing under it would make every such query a hit for every other.
func TestEmptyKeysAreNeverStored(t *testing.T) {
	c := cache.New()
	c.Put(&cache.Entry{Key: "", Text: "orphan"})
	c.Put(nil)

	if _, ok := c.Get(""); ok {
		t.Error("an empty key produced a hit")
	}
	if n := c.Stats().Entries; n != 0 {
		t.Errorf("%d entries stored under an empty key", n)
	}
}

// TestCountersTrackEffectiveness. "An unmeasured cache is an assumption" — and
// these are what `mole research` prints, so they have to be right.
func TestCountersTrackEffectiveness(t *testing.T) {
	c := cache.New()
	c.Put(&cache.Entry{Key: "u:a", Text: "a"})

	c.Get("u:a")       // hit
	c.Get("u:a")       // hit
	c.Get("u:missing") // miss

	s := c.Stats()
	if s.Hits != 2 {
		t.Errorf("hits = %d, want 2", s.Hits)
	}
	if s.Misses != 1 {
		t.Errorf("misses = %d, want 1", s.Misses)
	}
	if s.Entries != 1 {
		t.Errorf("entries = %d, want 1", s.Entries)
	}
	// An empty-key lookup is neither: there was nothing to look for.
	c.Get("")
	if got := c.Stats().Misses; got != 1 {
		t.Errorf("an empty-key lookup counted as a miss (%d)", got)
	}
}

// TestStoredAtIsFilledIn, so an entry's age is knowable when a durable cache
// eventually needs a staleness rule.
func TestStoredAtIsFilledIn(t *testing.T) {
	c := cache.New()
	c.Put(&cache.Entry{Key: "u:a", Text: "a"})

	got, _ := c.Get("u:a")
	if got.StoredAt.IsZero() {
		t.Error("StoredAt was left zero")
	}
	if time.Since(got.StoredAt) > time.Minute {
		t.Errorf("StoredAt = %v, which is not now", got.StoredAt)
	}
}

// TestConcurrentAccessIsSafe. M5 turns on a worker pool against one shared
// cache, and the counters are mutated on every read.
func TestConcurrentAccessIsSafe(t *testing.T) {
	c := cache.New()
	var wg sync.WaitGroup

	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c.Put(&cache.Entry{Key: "u:shared", Text: "body"})
				c.Get("u:shared")
				c.Get("u:absent")
				c.Stats()
			}
		}(w)
	}
	wg.Wait()

	s := c.Stats()
	if s.Entries != 1 {
		t.Errorf("entries = %d, want 1 after concurrent first-write-wins", s.Entries)
	}
	if s.Hits+s.Misses != 16*100*2 {
		t.Errorf("hits+misses = %d, want %d — a counter update was lost",
			s.Hits+s.Misses, 16*100*2)
	}
}
