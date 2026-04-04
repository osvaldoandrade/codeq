package codeq

import "fmt"

// Error is the base error type for codeQ SDK operations.
type Error struct {
	Message string
	Cause   error
}

// Error returns a human-readable description of the error.
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("codeq: %s: %v", e.Message, e.Cause)
	}
	return fmt.Sprintf("codeq: %s", e.Message)
}

// Unwrap returns the underlying cause, supporting errors.Is/As chains.
func (e *Error) Unwrap() error {
	return e.Cause
}

// APIError represents an HTTP error response from the codeQ API.
type APIError struct {
	StatusCode   int
	ResponseBody string
	Message      string
}

// Error returns a formatted message including the HTTP status code.
func (e *APIError) Error() string {
	if e.ResponseBody != "" {
		return fmt.Sprintf("codeq: API error (status %d): %s – %s", e.StatusCode, e.Message, e.ResponseBody)
	}
	return fmt.Sprintf("codeq: API error (status %d): %s", e.StatusCode, e.Message)
}

// AuthError indicates an authentication or authorization failure (HTTP 401/403).
type AuthError struct {
	Message string
}

// Error returns the authentication error message.
func (e *AuthError) Error() string {
	return fmt.Sprintf("codeq: auth error: %s", e.Message)
}

// TimeoutError indicates that an operation exceeded its deadline.
type TimeoutError struct {
	Message string
}

// Error returns the timeout error message.
func (e *TimeoutError) Error() string {
	return fmt.Sprintf("codeq: timeout: %s", e.Message)
}
