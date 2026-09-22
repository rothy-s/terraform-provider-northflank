package apierr

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// Tests pass an explicit tiny delay so they never actually sleep.
const (
	testAttempts = 4
	testDelay    = time.Microsecond
)

func TestIsTransient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"500 is transient", &APIError{StatusCode: http.StatusInternalServerError}, true},
		{"502 is transient", &APIError{StatusCode: http.StatusBadGateway}, true},
		{"503 is transient", &APIError{StatusCode: http.StatusServiceUnavailable}, true},
		{"429 is transient", &APIError{StatusCode: http.StatusTooManyRequests}, true},
		{"400 is not", &APIError{StatusCode: http.StatusBadRequest}, false},
		{"404 is not", &APIError{StatusCode: http.StatusNotFound}, false},
		{"409 is not", &APIError{StatusCode: http.StatusConflict}, false},
		{"non-API errors are not", errors.New("dial tcp: connection refused"), false},
		{"nil is not", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransient(tt.err); got != tt.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryTransientRetriesUntilSuccess(t *testing.T) {
	calls := 0
	err := Retry(t.Context(), testAttempts, testDelay, func() error {
		calls++

		if calls < 3 {
			return &APIError{StatusCode: http.StatusInternalServerError}
		}

		return nil
	})

	if err != nil {
		t.Fatalf("Retry() error = %v, want nil", err)
	}

	if calls != 3 {
		t.Errorf("fn was called %d times, want 3", calls)
	}
}

func TestRetryTransientDoesNotRetryPermanentErrors(t *testing.T) {
	calls := 0
	err := Retry(t.Context(), testAttempts, testDelay, func() error {
		calls++
		return &APIError{StatusCode: http.StatusBadRequest, Message: "bad name"}
	})

	if err == nil {
		t.Fatal("Retry() = nil, want the original error")
	}

	if calls != 1 {
		t.Errorf("fn was called %d times, want 1 -- a 400 must not be retried", calls)
	}

	// The caller still needs to inspect the status, e.g. for IsNotFound.
	var apiErr *APIError

	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("error = %v, want an *APIError carrying the 400", err)
	}
}

func TestRetryTransientGivesUp(t *testing.T) {
	calls := 0
	err := Retry(t.Context(), testAttempts, testDelay, func() error {
		calls++
		return &APIError{StatusCode: http.StatusInternalServerError}
	})

	if err == nil {
		t.Fatal("Retry() = nil, want an error")
	}

	if calls != testAttempts {
		t.Errorf("fn was called %d times, want %d", calls, testAttempts)
	}

	// Wrapping must survive, so 404-style checks still work after exhaustion.
	var apiErr *APIError

	if !errors.As(err, &apiErr) {
		t.Errorf("error = %v, want it to wrap an *APIError", err)
	}
}

func TestRetryTransientHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	calls := 0
	err := Retry(ctx, testAttempts, testDelay, func() error {
		calls++
		cancel() // cancel during the first backoff
		return &APIError{StatusCode: http.StatusInternalServerError}
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}

	if calls != 1 {
		t.Errorf("fn was called %d times, want 1", calls)
	}
}
