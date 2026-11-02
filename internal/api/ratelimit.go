package api

import (
	"sync"
	"time"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/clock"
)

// Limiter decides whether a request identified by key may proceed. The
// in-memory implementation suits one instance; a shared store (Redis,
// Postgres) can implement the same interface for several.
type Limiter interface {
	Allow(key string) (ok bool, retryAfter time.Duration)
}

// FailureTracker counts failures per key (login email, IP) in a window.
type FailureTracker interface {
	Blocked(key string) (bool, time.Duration)
	Fail(key string)
	Reset(key string)
}

// Rate is a token-bucket rate: Per refills Burst tokens.
type Rate struct {
	Burst int
	Per   time.Duration
}

// memLimiter is a token bucket per key with idle-key eviction.
type memLimiter struct {
	mu      sync.Mutex
	rate    Rate
	clock   clock.Clock
	buckets map[string]*bucket
	calls   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewMemLimiter returns an in-memory token-bucket limiter.
func NewMemLimiter(r Rate, c clock.Clock) Limiter {
	return &memLimiter{rate: r, clock: c, buckets: map[string]*bucket{}}
}

func (l *memLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	perToken := l.rate.Per / time.Duration(l.rate.Burst)
	l.calls++
	if l.calls%1024 == 0 {
		l.evict(now)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.rate.Burst), last: now}
		l.buckets[key] = b
	}
	b.tokens = min(float64(l.rate.Burst), b.tokens+float64(now.Sub(b.last))/float64(perToken))
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) * float64(perToken))
	return false, max(wait, time.Second)
}

// evict drops buckets that have been full long enough to be indistinguishable
// from new ones.
func (l *memLimiter) evict(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > l.rate.Per {
			delete(l.buckets, k)
		}
	}
}

// memFailures blocks a key after Max failures within Window.
type memFailures struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	clock  clock.Clock
	m      map[string]*failWindow
}

type failWindow struct {
	count int
	start time.Time
}

// NewMemFailureTracker blocks a key for the rest of the window after max failures.
func NewMemFailureTracker(maxFailures int, window time.Duration, c clock.Clock) FailureTracker {
	return &memFailures{max: maxFailures, window: window, clock: c, m: map[string]*failWindow{}}
}

func (f *memFailures) current(key string) *failWindow {
	w := f.m[key]
	if w != nil && f.clock.Now().Sub(w.start) >= f.window {
		delete(f.m, key)
		return nil
	}
	return w
}

func (f *memFailures) Blocked(key string) (bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.current(key)
	if w == nil || w.count < f.max {
		return false, 0
	}
	return true, max(f.window-f.clock.Now().Sub(w.start), time.Second)
}

func (f *memFailures) Fail(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.m) > maxTrackedKeys {
		f.evictExpired()
	}
	w := f.current(key)
	if w == nil {
		w = &failWindow{start: f.clock.Now()}
		f.m[key] = w
	}
	w.count++
}

// maxTrackedKeys bounds memory when an attacker sprays random emails.
const maxTrackedKeys = 100_000

func (f *memFailures) evictExpired() {
	now := f.clock.Now()
	for k, w := range f.m {
		if now.Sub(w.start) >= f.window {
			delete(f.m, k)
		}
	}
}

func (f *memFailures) Reset(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
}
