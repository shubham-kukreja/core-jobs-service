// Package apperr defines the typed errors modules return and the API maps to
// the public error shape. Messages are safe to show clients; the wrapped
// cause is logged server-side only.
package apperr

import (
	"errors"
	"fmt"
)

// Code is a stable, public error code.
type Code string

const (
	Unauthorized        Code = "unauthorized"
	Forbidden           Code = "forbidden"
	NotFound            Code = "not_found"
	ValidationFailed    Code = "validation_failed"
	Conflict            Code = "conflict"
	IdempotencyMismatch Code = "idempotency_mismatch"
	InvalidParams       Code = "invalid_params"
	NotUserTriggerable  Code = "not_user_triggerable"
	RateLimited         Code = "rate_limited"
	QueueFull           Code = "queue_full"
	NotAvailable        Code = "not_available"
	PayloadTooLarge     Code = "payload_too_large"
	UnsupportedMedia    Code = "unsupported_media_type"
	Internal            Code = "internal"
)

// Detail pinpoints one problem, e.g. a rejected point or a config path.
type Detail struct {
	Index  *int           `json:"index,omitempty"`
	Path   string         `json:"path,omitempty"`  // JSON pointer, e.g. /scorecards/equity/signals
	Field  string         `json:"field,omitempty"` // request field name
	Reason string         `json:"reason"`          // machine-readable, e.g. out_of_range, weights_sum
	Meta   map[string]any `json:"meta,omitempty"`  // e.g. {"min":0,"max":100,"got":0.95}
}

// Error is a domain error with a public code and message.
type Error struct {
	Code    Code
	Message string
	Details []Detail
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// New builds an Error.
func New(code Code, msg string, details ...Detail) *Error {
	return &Error{Code: code, Message: msg, Details: details}
}

// Wrap builds an Error that keeps cause for logs but never exposes it.
func Wrap(cause error, code Code, msg string) *Error {
	return &Error{Code: code, Message: msg, cause: cause}
}

// As extracts an *Error from err.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// CodeOf returns err's code, or Internal for untyped errors.
func CodeOf(err error) Code {
	if e, ok := As(err); ok {
		return e.Code
	}
	return Internal
}

// Is reports whether err carries code.
func Is(err error, code Code) bool { return CodeOf(err) == code }
