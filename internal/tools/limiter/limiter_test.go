package limiter_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// fakeClock lets the rate tests run instantly and deterministically instead of
// sleeping real seconds.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	slept  time.Duration
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.slept += d
	c.sleeps = append(c.sleeps, d)
	return nil
}

func (c *fakeClock) total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slept
}

func TestBurstThenThrottle(t *testing.T) {
	c := newFakeClock()
	l := limiter.New(limiter.Limit{Rate: 2, Burst: 2})
	l.SetClock(c.Now, c.Sleep)

	ctx := context.Background()

	// The burst is free.
	for i := 0; i < 2; i++ {
		if err := l.Wait(ctx, "example.com"); err != nil {
			t.Fatalf("burst request %d: %v", i, err)
		}
	}
	if got := c.total(); got != 0 {
		t.Fatalf("burst slept %v, want 0", got)
	}

	// The third waits for a token: 1/rate = 500ms.
	if err := l.Wait(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if got := c.total(); got < 400*time.Millisecond || got > 600*time.Millisecond {
		t.Errorf("third request slept %v, want ~500ms", got)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	c := newFakeClock()
	l := limiter.New(limiter.Limit{Rate: 1, Burst: 1})
	l.SetClock(c.Now, c.Sleep)

	ctx := context.Background()
	// One slow host must not stall requests to every other host.
	for _, host := range []string{"a.com", "b.com", "c.com"} {
		if err := l.Wait(ctx, host); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
	}
	if got := c.total(); got != 0 {
		t.Errorf("independent keys slept %v, want 0", got)
	}
}

// TestMinIntervalSurvivesBurst is why Limit carries MinInterval separately: a
// Crawl-delay is a floor between requests, and a burst-capable bucket alone
// would let several through back to back and ignore the site's request.
func TestMinIntervalSurvivesBurst(t *testing.T) {
	c := newFakeClock()
	l := limiter.New(limiter.Limit{Rate: 1000, Burst: 1000, MinInterval: time.Second})
	l.SetClock(c.Now, c.Sleep)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := l.Wait(ctx, "polite.example"); err != nil {
			t.Fatal(err)
		}
	}
	// First is free; the next two are spaced a second apart.
	if got := c.total(); got < 2*time.Second {
		t.Errorf("total sleep %v, want >= 2s from MinInterval", got)
	}
}

func TestUnlimitedNeverWaits(t *testing.T) {
	c := newFakeClock()
	l := limiter.New(limiter.Unlimited)
	l.SetClock(c.Now, c.Sleep)

	for i := 0; i < 100; i++ {
		if err := l.Wait(context.Background(), "fast.example"); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.total(); got != 0 {
		t.Errorf("unlimited slept %v", got)
	}
}

func TestPerKeyOverrideBeatsFallback(t *testing.T) {
	l := limiter.New(limiter.Limit{Rate: 1, Burst: 1})
	l.Set("slow.example", limiter.Limit{Rate: 1, Burst: 1, MinInterval: 5 * time.Second})

	if got := l.LimitFor("slow.example").MinInterval; got != 5*time.Second {
		t.Errorf("override MinInterval = %v, want 5s", got)
	}
	if got := l.LimitFor("other.example").MinInterval; got != 0 {
		t.Errorf("fallback picked up the override: %v", got)
	}
}

// TestCancelledContextDoesNotSleep: a cancelled request must return promptly
// rather than serving out a delay nobody is waiting for.
func TestCancelledContextDoesNotSleep(t *testing.T) {
	l := limiter.New(limiter.Limit{Rate: 0.001, Burst: 1})

	ctx, cancel := context.WithCancel(context.Background())
	// Consume the single burst token.
	if err := l.Wait(ctx, "slow.example"); err != nil {
		t.Fatal(err)
	}
	cancel()

	start := time.Now()
	err := l.Wait(ctx, "slow.example")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancelled Wait took %v", elapsed)
	}
}

func TestConcurrentWaitIsRaceFree(t *testing.T) {
	l := limiter.New(limiter.Limit{Rate: 10_000, Burst: 10_000})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := []string{"a", "b", "c"}[i%3]
			for j := 0; j < 20; j++ {
				if err := l.Wait(context.Background(), key); err != nil {
					t.Errorf("wait: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
