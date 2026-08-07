package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// clock is a hand-wound time source. Every test here is about what happens
// after a second, or after a minute, and none of them should take that long.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestARateOfZeroIsNoLimiterAtAll(t *testing.T) {
	for _, perSecond := range []int{0, -1} {
		l := New(perSecond, Options{})
		if l != nil {
			t.Fatalf("New(%d) = %v, want nil", perSecond, l)
		}
		// The nil is the point: callers keep it and call Allow on it.
		for i := range 1000 {
			if !l.Allow("anyone") {
				t.Fatalf("New(%d): request %d refused by a limiter that is off", perSecond, i)
			}
		}
		if got := l.Len(); got != 0 {
			t.Errorf("Len() on a nil limiter = %d, want 0", got)
		}
		if got := l.Cleared(); got != 0 {
			t.Errorf("Cleared() on a nil limiter = %d, want 0", got)
		}
	}
}

func TestABurstIsSpentAndThenRefills(t *testing.T) {
	c := newClock()
	const perSecond = 10
	l := New(perSecond, Options{now: c.Now})

	burst := Burst(perSecond)
	for i := range burst {
		if !l.Allow("ip") {
			t.Fatalf("request %d of the burst was refused; the bucket holds %d", i, burst)
		}
	}
	if l.Allow("ip") {
		t.Fatal("the request after the burst was allowed; the bucket should be empty")
	}

	// One second at ten per second is ten more tokens, and no more than ten.
	c.advance(time.Second)
	for i := range perSecond {
		if !l.Allow("ip") {
			t.Fatalf("request %d after a second of refill was refused", i)
		}
	}
	if l.Allow("ip") {
		t.Fatal("the bucket refilled by more than one second's worth")
	}
}

func TestOneKeyDoesNotSpendAnother(t *testing.T) {
	c := newClock()
	l := New(1, Options{now: c.Now})

	for range Burst(1) + 5 {
		l.Allow("noisy")
	}
	if l.Allow("noisy") {
		t.Fatal("the noisy key was still allowed after spending its bucket")
	}
	if !l.Allow("quiet") {
		t.Fatal("a key that has made no requests was refused")
	}
}

func TestBurstFollowsTheRateButNeverDropsBelowTheFloor(t *testing.T) {
	cases := []struct{ perSecond, want int }{
		{1, MinBurst},
		{10, MinBurst},
		{MinBurst / BurstFactor, MinBurst},
		{50, 100},
		{1000, 2000},
	}
	for _, tc := range cases {
		if got := Burst(tc.perSecond); got != tc.want {
			t.Errorf("Burst(%d) = %d, want %d", tc.perSecond, got, tc.want)
		}
	}
}

func TestASweepDropsBucketsNothingHasTouched(t *testing.T) {
	c := newClock()
	l := New(5, Options{TTL: time.Minute, now: c.Now})

	l.Allow("a")
	l.Allow("b")
	if got := l.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}

	// Half the TTL, then a touch on one of them: the touched bucket survives the
	// sweep that the untouched one does not.
	c.advance(30 * time.Second)
	l.Allow("a")
	c.advance(31 * time.Second)

	if got := l.sweep(c.Now()); got != 1 {
		t.Fatalf("after the sweep Len() = %d, want 1", got)
	}
	if _, ok := l.buckets["a"]; !ok {
		t.Error("the sweep dropped the bucket that had just been used")
	}
	if _, ok := l.buckets["b"]; ok {
		t.Error("the sweep kept a bucket older than the TTL")
	}
}

func TestATableOverItsCapIsEmptiedRatherThanGrown(t *testing.T) {
	c := newClock()
	l := New(5, Options{TTL: time.Hour, MaxKeys: 4, now: c.Now})

	// Four live keys — nothing is expired, so the sweep cannot help — and then a
	// fifth, which is what forces the choice.
	for _, key := range []string{"a", "b", "c", "d"} {
		l.Allow(key)
	}
	if got := l.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}

	l.Allow("e")

	if got := l.Len(); got != 1 {
		t.Fatalf("after the cap was reached Len() = %d, want 1 — the table should hold only the new key", got)
	}
	if got := l.Cleared(); got != 1 {
		t.Errorf("Cleared() = %d, want 1", got)
	}
	// Leniency, not refusal: the request that triggered the clear is served.
	if _, ok := l.buckets["e"]; !ok {
		t.Error("the key that forced the clear was not added afterwards")
	}
}

func TestExpiredBucketsAreEnoughToStayUnderTheCap(t *testing.T) {
	c := newClock()
	l := New(5, Options{TTL: time.Minute, MaxKeys: 4, now: c.Now})

	for _, key := range []string{"a", "b", "c"} {
		l.Allow(key)
	}
	c.advance(2 * time.Minute)
	l.Allow("live")

	// "live" is under the cap, so nothing was swept yet.
	if got := l.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}

	l.Allow("another")

	if got := l.Cleared(); got != 0 {
		t.Errorf("Cleared() = %d, want 0 — expiring three stale buckets was room enough", got)
	}
	if got := l.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 (live, another)", got)
	}
}

func TestConcurrentCallersShareOneBucket(t *testing.T) {
	c := newClock()
	const perSecond = 10
	l := New(perSecond, Options{now: c.Now})

	const goroutines, each = 8, 50
	allowed := make(chan int, goroutines)

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int
			for range each {
				if l.Allow("shared") {
					n++
				}
			}
			allowed <- n
		}()
	}
	wg.Wait()
	close(allowed)

	var total int
	for n := range allowed {
		total += n
	}

	// The clock never moves, so the bucket never refills: exactly one burst may
	// get through however many goroutines are asking.
	if want := Burst(perSecond); total != want {
		t.Errorf("%d requests allowed across %d goroutines, want exactly %d", total, goroutines, want)
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	// A nil limiter's Run returns immediately rather than blocking on a ticker
	// it never built.
	done := make(chan struct{})
	go func() {
		defer close(done)
		var l *Limiter
		l.Run(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run on a nil limiter did not return")
	}
}
