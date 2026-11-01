package jobs

import (
	"errors"
	"time"
)

// retryableError, permanentError and snoozeError tell the engine adapter how
// to treat a handler failure.
type (
	retryableError struct{ err error }
	permanentError struct{ err error }
	snoozeError    struct{ d time.Duration }
)

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }
func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }
func (e snoozeError) Error() string    { return "snoozed for " + e.d.String() }

// Retryable marks err as transient: the engine retries with backoff.
// Untyped errors are treated as retryable too.
func Retryable(err error) error { return retryableError{err} }

// Permanent marks err as final: the run fails now, without retries.
func Permanent(err error) error { return permanentError{err} }

// Snooze re-runs the job after d without counting an attempt, e.g. when a
// source has not published yet.
func Snooze(d time.Duration) error { return snoozeError{d} }

// IsPermanent reports whether err (or anything it wraps) is permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// SnoozeDuration returns the snooze duration if err is a snooze.
func SnoozeDuration(err error) (time.Duration, bool) {
	var s snoozeError
	if errors.As(err, &s) {
		return s.d, true
	}
	return 0, false
}
