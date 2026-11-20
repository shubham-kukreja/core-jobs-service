// Package clock abstracts time so schedules, PIT reads and session expiry are
// testable with a fake clock.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// System is the real wall clock, always in UTC.
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }

// Fake is a manually advanced clock for tests. Safe for concurrent use.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a fake clock set to t.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

// Shifted is a running clock displaced to another date: it ticks with real
// time from wherever it was last set. End-to-end tests use it to run a
// calendar (days or months apart) while writes still get distinct,
// increasing timestamps, as they would in production. Safe for concurrent use.
type Shifted struct {
	mu     sync.Mutex
	offset time.Duration
}

// NewShifted returns a clock that reads t now and keeps running.
func NewShifted(t time.Time) *Shifted {
	return &Shifted{offset: time.Until(t)}
}

func (s *Shifted) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Add(s.offset).UTC()
}

// Set jumps the clock to t; it keeps running from there. Moving backwards
// is refused, as real time cannot.
func (s *Shifted) Set(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next := time.Until(t); next > s.offset {
		s.offset = next
	}
}
