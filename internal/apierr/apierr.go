// Package apierr holds the error handling for the generated Northflank client:
// decoding a failed response into an error, and deciding whether it is worth
// retrying.
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// APIError is a non-2xx response from the Northflank API.
type APIError struct {
	StatusCode int

	// Message is the human-readable message, when the body carries one.
	Message string

	// ID identifies the failed request on Northflank's side, when the body
	// carries one. Worth quoting in a support request.
	ID string

	// Body is the raw response body, always populated.
	Body []byte
}

func (e *APIError) Error() string {
	switch {
	case e.Message != "" && e.ID != "":
		return fmt.Sprintf("HTTP %d: %s (request %s)", e.StatusCode, e.Message, e.ID)

	case e.Message != "":
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)

	case len(e.Body) > 0:
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, truncate(strings.TrimSpace(string(e.Body)), 512))

	default:
		return fmt.Sprintf("HTTP %d (empty response body)", e.StatusCode)
	}
}

// IsNotFound reports whether the API said the resource does not exist, which
// callers treat as "remove from state" rather than as a failure.
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == http.StatusNotFound
}

// Response is implemented by every generated *HTTPResponse wrapper.
type Response interface {
	StatusCode() int
	GetBody() []byte
}

// Check converts a non-2xx response into an *APIError, and returns nil for
// anything in the 2xx range.
func Check(resp Response) error {
	status := resp.StatusCode()

	if status >= 200 && status < 300 {
		return nil
	}

	body := resp.GetBody()
	apiErr := &APIError{StatusCode: status, Body: body}

	// Northflank reports errors as {"error": {"status": ..., "message": "...",
	// "id": "..."}}. The OpenAPI document does not describe error bodies, so
	// a top-level "message" is accepted too, and anything that does not parse
	// falls back to the raw body in Error().
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			ID      string `json:"id"`
		} `json:"error"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal(body, &parsed); err == nil {
		apiErr.Message = parsed.Error.Message
		apiErr.ID = parsed.Error.ID

		if apiErr.Message == "" {
			apiErr.Message = parsed.Message
		}
	}

	return apiErr
}

// IsTransient reports whether an error looks worth retrying: a rate limit or a
// server-side fault, as opposed to a request the server rejected on its
// merits.
func IsTransient(err error) bool {
	var apiErr *APIError

	if !errors.As(err, &apiErr) {
		return false
	}

	return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= http.StatusInternalServerError
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}

	return s[:max] + "..."
}
