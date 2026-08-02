package limiter_test

import (
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// TestBacklogQueuesRatherThanCollapsing is the property this package exists
// for. Six callers arriving at once against a 1/s limit must be spread across
// six seconds; the earlier implementation clamped the bucket to zero before
// charging, so every backlogged caller got the same one-second wait and the
// effective rate was "as fast as the callers arrive".
func TestBacklogQueuesRatherThanCollapsing(t *testing.T) {
	l := limiter.New(limiter.PerSecond(1))
	now := time.Unix(0, 0)
	l.SetClock(func() time.Time { return now }, nil)

	var got []time.Duration
	for i := 0; i < 6; i++ {
		got = append(got, l.Reserve("host"))
	}

	want := []time.Duration{0, time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits = %v, want %v", got, want)
		}
	}
}

// TestBurstIsHonouredBeforeQueueing: a burst limit that does not actually let a
// burst through would serialize every parallel fetch for no reason.
func TestBurstIsHonouredBeforeQueueing(t *testing.T) {
	l := limiter.New(limiter.Limit{Rate: 1, Burst: 3})
	now := time.Unix(0, 0)
	l.SetClock(func() time.Time { return now }, nil)

	for i := 0; i < 3; i++ {
		if d := l.Reserve("host"); d != 0 {
			t.Fatalf("call %d within the burst waited %v", i, d)
		}
	}
	if d := l.Reserve("host"); d != time.Second {
		t.Errorf("first call past the burst waited %v, want 1s", d)
	}
}

// TestRefillDrainsTheBacklog: debt must be repaid by the passage of time, or a
// queue that formed once would penalize callers forever.
func TestRefillDrainsTheBacklog(t *testing.T) {
	l := limiter.New(limiter.PerSecond(1))
	now := time.Unix(0, 0)
	l.SetClock(func() time.Time { return now }, nil)

	for i := 0; i < 4; i++ {
		l.Reserve("host")
	}
	now = now.Add(10 * time.Second)

	if d := l.Reserve("host"); d != 0 {
		t.Errorf("after 10s of idle the next call waited %v, want 0", d)
	}
}
