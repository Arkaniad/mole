package cache

import (
	"sync"
	"time"
)

// Entry is a cached artifact.
type Entry struct {
	Key string
	// Claims is how much evidence the artifact produced. A count rather than
	// the claims themselves: within a session they are already in the store
	// under the lead that first found them, and re-inserting copies would
	// inflate every claim-count metric §14.3 reads while adding nothing a
	// reader or the report can use.
	Claims int
	// Summary is what the actor concluded, which is what a planner-visible
	// result has to carry (§4).
	Summary string
	// Text is the extracted document, for a URL-keyed entry. This is the field
	// that makes two queries converging on one page pay for one fetch.
	Text string
	// Title and PublishedAt travel with the text so a reused document produces
	// the same claim metadata as a freshly fetched one.
	Title       string
	PublishedAt *time.Time

	StoredAt time.Time
}

// Cache stores artifacts for one session.
//
// Session-scoped and in memory, which is a deliberate limit rather than an
// oversight. A cross-session cache has to answer "how stale is too stale", and
// that question has a different answer per question type — a settled fact keeps
// for months, a "current consensus" for days. §14.2's corpus is what would
// settle it, so the durable version waits for data rather than for a guess.
//
// The within-session win is the one §9.3 actually names, and it needs none of
// that: a page fetched twenty seconds ago has not changed.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]*Entry

	// Hits and Misses are reported so a session can show whether the cache
	// earned its keep. An unmeasured cache is an assumption.
	hits, misses int
}

// New creates an empty cache.
func New() *Cache {
	return &Cache{entries: map[string]*Entry{}}
}

// Get returns a cached artifact.
//
// A nil cache is a miss, so callers need no conditional at every site.
func (c *Cache) Get(key string) (*Entry, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil, false
	}
	c.hits++
	return e, true
}

// Put stores an artifact. The first write for a key wins: a later run of the
// same artifact produced the same document, and overwriting would churn without
// changing anything.
func (c *Cache) Put(e *Entry) {
	if c == nil || e == nil || e.Key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[e.Key]; exists {
		return
	}
	if e.StoredAt.IsZero() {
		e.StoredAt = time.Now()
	}
	c.entries[e.Key] = e
}

// Stats reports cache effectiveness.
type Stats struct {
	Entries, Hits, Misses int
}

// Stats returns the counters.
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Stats{Entries: len(c.entries), Hits: c.hits, Misses: c.misses}
}
