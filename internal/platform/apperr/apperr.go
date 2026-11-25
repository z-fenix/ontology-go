// Package apperr defines the structured error type shared across services.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is an API-facing error with a stable code and HTTP status.
type Error struct {
	Code       string         `json:"errorCode"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
	HTTPStatus int            `json:"-"`
	wrapped    error
}

func (e *Error) Error() string {
	if e.wrapped != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.wrapped)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.wrapped }

// New builds an Error.
func New(status int, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), HTTPStatus: status}
}

// Wrap attaches a cause while keeping code and status.
func (e *Error) Wrap(err error) *Error {
	cp := *e
	cp.wrapped = err
	return &cp
}

// WithDetails attaches structured details.
func (e *Error) WithDetails(d map[string]any) *Error {
	cp := *e
	cp.Details = d
	return &cp
}

// As extracts an *Error from err, or synthesizes a 500.
func As(err error) *Error {
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return New(http.StatusInternalServerError, "INTERNAL", "%v", err)
}

// Common constructors.
func NotFound(format string, args ...any) *Error {
	return New(http.StatusNotFound, "NOT_FOUND", format, args...)
}
func Conflict(code, format string, args ...any) *Error {
	return New(http.StatusConflict, code, format, args...)
}
func BadRequest(code, format string, args ...any) *Error {
	return New(http.StatusBadRequest, code, format, args...)
}
