// Package ratelimit is a keyed token bucket: one bucket per client address,
// per project or per credential, all sharing one rate.
//
// It is in-process and therefore per-instance. Two instances behind a load
// balancer enforce the limit twice, once each, and a caller spread across both
// gets twice the rate — which is the price of not having a shared counter, and
// the reason DESIGN.md §9.1 names rate limiting as the one thing that would
// ever make Redis relevant. Until there is a second instance there is nothing
// to share, and a limiter that needs a network round trip to decide whether to
// serve a mock response would cost more than the response.
//
// Nothing here speaks HTTP. What a refused request is answered with, and what
// key it is counted under, belongs to internal/web.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Defaults for the bookkeeping around the buckets themselves. They are not
// about how fast anyone may go — that is the rate a Limiter is built with —
// but about the size of the table holding the answer.
const (
	// DefaultTTL is how long a bucket outlives the last request that touched
	// it. A bucket refills at the configured rate, so one untouched for longer
	// than it takes to refill completely holds no information: recreating it on
	// the next request gives exactly the same answer as keeping it would have.
	// A minute is far longer than that for any rate worth setting.
	DefaultTTL = time.Minute
	// DefaultSweep is how often expired buckets are removed. Sweeping is a walk
	// of the whole map under the lock, so it is done on a timer rather than on
	// every request.
	DefaultSweep = time.Minute
	// DefaultMaxKeys bounds the table. Each bucket is a handful of words, so a
	// hundred thousand of them is a few megabytes — enough for any real client
	// population, and a ceiling for a caller who cycles addresses to make the
	// limiter itself the leak.
	DefaultMaxKeys = 100_000
)

// BurstFactor and MinBurst turn a rate into a burst.
//
// One knob rather than two, because the pair is not independent in practice: a
// burst below the rate refuses a client who is exactly at the limit, and a
// burst far above it makes the limit meaningless for the short flood it exists
// to absorb. Twice the per-second rate lets a client that is idle for a second
// spend two seconds' worth at once, and the floor keeps a deliberately low rate
// from refusing an ordinary test run's opening handful of requests.
const (
	BurstFactor = 2
	MinBurst    = 20
)

// Burst is the bucket depth for a given rate.
func Burst(perSecond int) int {
	if b := perSecond * BurstFactor; b > MinBurst {
		return b
	}
	return MinBurst
}

// Options are a Limiter's bookkeeping settings. The zero value is usable:
// every field falls back to the default above.
type Options struct {
	TTL     time.Duration
	Sweep   time.Duration
	MaxKeys int
	// now is the clock, for tests that need a bucket to expire without waiting
	// a minute for it.
	now func() time.Time
}

// Limiter hands out one token bucket per key.
//
// A nil *Limiter allows everything, which is what a rate of zero produces. That
// is deliberate: "this limit is off" then needs no branch at every call site,
// only at the one that built it.
type Limiter struct {
	limit rate.Limit
	burst int
	opts  Options

	mu      sync.Mutex
	buckets map[string]*bucket

	// cleared counts how many times the table was emptied for being over its
	// cap, which is the one event here worth an operator's attention.
	cleared int64
}

type bucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

// New returns a limiter allowing perSecond requests per key per second, or nil
// if perSecond is not positive. The caller keeps the nil and calls Allow on it
// regardless.
func New(perSecond int, opts Options) *Limiter {
	if perSecond <= 0 {
		return nil
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.Sweep <= 0 {
		opts.Sweep = DefaultSweep
	}
	if opts.MaxKeys <= 0 {
		opts.MaxKeys = DefaultMaxKeys
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	return &Limiter{
		limit:   rate.Limit(perSecond),
		burst:   Burst(perSecond),
		opts:    opts,
		buckets: make(map[string]*bucket),
	}
}

// Allow reports whether this key may make one more request now, spending a
// token if it may. A nil Limiter allows everything.
func (l *Limiter) Allow(key string) bool {
	if l == nil {
		return true
	}

	now := l.opts.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		// A new key in a table that is already at its ceiling. Make room before
		// adding to it, rather than after — a limiter that grows past its cap
		// while deciding whether it may is not a bounded limiter.
		if len(l.buckets) >= l.opts.MaxKeys {
			l.makeRoom(now)
		}
		b = &bucket{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}

	b.seen = now
	return b.limiter.AllowN(now, 1)
}

// Len reports how many buckets are held. It is what the gauge in
// internal/metrics reads, so that a table quietly filling up is visible before
// it hits its ceiling.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Cleared reports how many times the table has been emptied for being over its
// cap. Anything above zero means the cap is too low for the client population
// or that somebody is cycling keys.
func (l *Limiter) Cleared() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cleared
}

// Run sweeps expired buckets until ctx is cancelled. It is meant to run in its
// own goroutine for the lifetime of the process; a limiter that is never swept
// still stays bounded, by makeRoom, but only by the blunter of the two
// mechanisms.
func (l *Limiter) Run(ctx context.Context) {
	if l == nil {
		return
	}

	ticker := time.NewTicker(l.opts.Sweep)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.sweep(l.opts.now())
		}
	}
}

// sweep drops buckets nothing has touched for the TTL, and reports how many
// are left.
func (l *Limiter) sweep(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sweepLocked(now)
}

func (l *Limiter) sweepLocked(now time.Time) int {
	for key, b := range l.buckets {
		if now.Sub(b.seen) >= l.opts.TTL {
			delete(l.buckets, key)
		}
	}
	return len(l.buckets)
}

// makeRoom brings the table back under its cap: expired buckets first, and if
// that is not enough, all of them.
//
// Emptying it is crude, and it is the honest trade. Every bucket in the table
// is a client currently being counted, so there is no unimportant one to evict;
// picking the least recently seen would need an ordering maintained on every
// request to save a walk that happens only when a caller is deliberately
// cycling keys. What emptying costs is a moment of leniency — everyone starts
// with a full bucket again — which is the right way for a guard to fail: a
// refusal handed to the wrong client is worse than a request too many served
// to the right one.
func (l *Limiter) makeRoom(now time.Time) {
	if l.sweepLocked(now) < l.opts.MaxKeys {
		return
	}
	l.buckets = make(map[string]*bucket)
	l.cleared++
}
