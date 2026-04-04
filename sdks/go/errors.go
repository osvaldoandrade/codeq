package codeq

import "fmt"

// Error represents an error returned by the CodeQ API.
type Error struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Message describes the error.
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("codeq: %d %s", e.StatusCode, e.Message)
}
