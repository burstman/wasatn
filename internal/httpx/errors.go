// Package httpx holds HTTP plumbing shared by every handler: error types, JSON
// responses, CSRF protection, rate limiting, request logging and client IP
// extraction.
//
// Handlers are written as func(*kit.Kit) error, so they return an error instead
// of writing a status code by hand. This package provides the error vocabulary
// the central error handler turns into responses.
package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is an HTTP-aware error. Handlers return it to control the response
// status and the user-facing message without touching the ResponseWriter.
type Error struct {
	Status  int
	Message string
	// Err is the underlying cause. It is logged but never shown to the user.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// WithCause attaches an underlying error for logging.
func (e *Error) WithCause(err error) *Error {
	return &Error{Status: e.Status, Message: e.Message, Err: err}
}

// WithMessage replaces the user-facing message.
func (e *Error) WithMessage(msg string) *Error {
	return &Error{Status: e.Status, Message: msg, Err: e.Err}
}

// BadRequest reports invalid user input (400).
func BadRequest(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

// Unauthorized reports a missing or invalid session (401).
func Unauthorized(msg string) *Error {
	if msg == "" {
		msg = "Please sign in to continue."
	}
	return &Error{Status: http.StatusUnauthorized, Message: msg}
}

// Forbidden reports an authenticated user lacking access (403).
func Forbidden(msg string) *Error {
	if msg == "" {
		msg = "You do not have access to this resource."
	}
	return &Error{Status: http.StatusForbidden, Message: msg}
}

// NotFound reports a missing resource (404).
func NotFound(msg string) *Error {
	if msg == "" {
		msg = "Not found."
	}
	return &Error{Status: http.StatusNotFound, Message: msg}
}

// Conflict reports a uniqueness or state conflict (409).
func Conflict(msg string) *Error {
	if msg == "" {
		msg = "That conflicts with existing data."
	}
	return &Error{Status: http.StatusConflict, Message: msg}
}

// TooManyRequests reports rate limiting (429).
func TooManyRequests(msg string) *Error {
	if msg == "" {
		msg = "Too many requests. Please slow down."
	}
	return &Error{Status: http.StatusTooManyRequests, Message: msg}
}

// Internal wraps an unexpected error (500). The cause is logged, never shown.
func Internal(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Message: "Something went wrong.", Err: err}
}

// StatusOf returns the HTTP status an error should produce, defaulting to 500.
func StatusOf(err error) int {
	var httpErr *Error
	if errors.As(err, &httpErr) {
		return httpErr.Status
	}
	return http.StatusInternalServerError
}

// MessageOf returns the user-facing message for an error. Unexpected errors are
// reported generically so internals never leak to the browser.
func MessageOf(err error) string {
	var httpErr *Error
	if errors.As(err, &httpErr) {
		return httpErr.Message
	}
	return "Something went wrong."
}
