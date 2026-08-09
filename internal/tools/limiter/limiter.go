// Package limiter provides per-key rate limiting.
//
// This is not an optimization. A worker pool hammering arXiv or NCBI without a
// limiter gets the user banned, and several providers document hard ceilings
// (NCBI: 3 req/s without a key). The limiter is keyed by provider or by domain
// so one slow host cannot stall requests to every other host.
package limiter

import (
	"context"
	"math"
	"sync"
	"time"
)

// Limit describes a token bucket: Rate tokens per second, Burst capacity.
//
// A zero Rate means unlimited. MinInterval is a floor between consecutive
// acquisitions for the same key, which is how robots.txt Crawl-delay is
// expressed — a bucket alone would let a burst through.
type Limit struct {
	Rate        float64
	Burst       int
	MinInterval time.Duration
}

// Unlimited is the zero limit.
var Unlimited = Limit{}

func (l Limit) unlimited() bool { return l.Rate <= 0 && l.MinInterval <= 0 }

// PerSecond builds a limit of n requests per second with burst n.
func PerSecond(n float64) Limit {
	b := int(math.Ceil(n))
	if b < 1 {
		b = 1
	}
	return Limit{Rate: n, Burst: b}
}

// WithDelay adds a minimum spacing between requests.
func (l Limit) WithDelay(d time.Duration) Limit {
	l.MinInterval = d
	return l
}

// bucket is one key's state.
type bucket struct {
	mu       sync.Mutex
	limit    Limit
	tokens   float64
	last     time.Time
	lastTake time.Time
}

func (b *bucket) reserve(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.last.IsZero() {
		b.last = now
		b.tokens = float64(b.limit.Burst)
	}

	// Refill.
	if b.limit.Rate > 0 {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * b.limit.Rate
			if max := float64(b.limit.Burst); b.tokens > max {
				b.tokens = max
			}
		}
	}
	b.last = now

	var wait time.Duration

	if b.limit.Rate > 0 {
		// Charge unconditionally and let the balance go negative. The debt is
		// the queue: a caller arriving while the bucket is already owed three
		// tokens waits for all three, not for the single token it needs.
		// Clamping to zero first would give every backlogged caller the same
		// one-token wait, which is no rate limit at all under concurrency —
		// exactly the case this package exists for.
		b.tokens--
		if b.tokens < 0 {
			wait = time.Duration(-b.tokens / b.limit.Rate * float64(time.Second))
		}
	}

	// Enforce spacing on top of the bucket. Crawl-delay is a floor between
	// requests, which a burst-capable bucket would otherwise ignore.
	if b.limit.MinInterval > 0 && !b.lastTake.IsZero() {
		earliest := b.lastTake.Add(b.limit.MinInterval)
		if d := earliest.Sub(now.Add(wait)); d > 0 {
			wait += d
		}
	}
	b.lastTake = now.Add(wait)

	return wait
}

// Limiter holds one bucket per key.
type Limiter struct {
	mu       sync.RWMutex
	buckets  map[string]*bucket
	limits   map[string]Limit
	fallback Limit
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// New creates a limiter. The fallback applies to keys with no explicit limit.
func New(fallback Limit) *Limiter {
	return &Limiter{
		buckets:  map[string]*bucket{},
		limits:   map[string]Limit{},
		fallback: fallback,
		now:      time.Now,
		sleep:    sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SetClock overrides time and sleeping, for tests.
func (l *Limiter) SetClock(now func() time.Time, sleep func(context.Context, time.Duration) error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now, l.sleep = now, sleep
}

// Set assigns a limit to a key, replacing any bucket state for it.
func (l *Limiter) Set(key string, lim Limit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits[key] = lim
	delete(l.buckets, key)
}

// SlowTo raises a key's minimum interval, and only ever raises it.
//
// Set is the wrong tool for honouring a robots.txt Crawl-delay under a worker
// pool, and dangerously so. Read-then-Set is not atomic, and Set DELETES the
// bucket — so K workers arriving together at a host that asks to be crawled
// slowly all read the default interval, all call Set, and each delete throws
// away the accumulated debt the previous ones had queued. The burst that
// Crawl-delay exists to prevent is reassembled, at exactly the sites that
// asked not to receive it.
//
// This does the compare and the assignment under one lock, and leaves the
// bucket alone: an interval that only ever rises cannot let a caller through
// sooner than the previous limit allowed, so existing debt stays valid.
func (l *Limiter) SlowTo(key string, minInterval time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cur, ok := l.limits[key]
	if !ok {
		cur = l.fallback
	}
	if minInterval <= cur.MinInterval {
		return
	}
	l.limits[key] = cur.WithDelay(minInterval)
}

// LimitFor reports the limit in force for a key.
func (l *Limiter) LimitFor(key string) Limit {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if lim, ok := l.limits[key]; ok {
		return lim
	}
	return l.fallback
}

func (l *Limiter) bucketFor(key string) *bucket {
	l.mu.RLock()
	b, ok := l.buckets[key]
	l.mu.RUnlock()
	if ok {
		return b
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		return b
	}
	lim, ok := l.limits[key]
	if !ok {
		lim = l.fallback
	}
	b = &bucket{limit: lim}
	l.buckets[key] = b
	return b
}

// Wait blocks until the key may proceed, or the context is done.
//
// Returns the context error on cancellation without consuming the wait, so a
// cancelled request does not silently sleep out its delay.
func (l *Limiter) Wait(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lim := l.LimitFor(key)
	if lim.unlimited() {
		return nil
	}

	l.mu.RLock()
	now, sleep := l.now, l.sleep
	l.mu.RUnlock()

	d := l.bucketFor(key).reserve(now())
	if d <= 0 {
		return nil
	}
	return sleep(ctx, d)
}

// Reserve reports how long Wait would block, without blocking. Exposed for
// diagnostics and for tests.
func (l *Limiter) Reserve(key string) time.Duration {
	lim := l.LimitFor(key)
	if lim.unlimited() {
		return 0
	}
	l.mu.RLock()
	now := l.now
	l.mu.RUnlock()
	return l.bucketFor(key).reserve(now())
}
