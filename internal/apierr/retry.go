package apierr

import (
	"context"
	"fmt"
	"time"
)

// Retry runs fn, retrying with linear backoff while the API reports a
// transient failure. It gives up after attempts tries and returns the last
// error, wrapped so callers can still inspect the *APIError.
//
// Transient here means a 429 rate limit or a 5xx fault. Northflank rate-limits
// API tokens per minute, and Terraform applies independent resources in
// parallel, so a large apply can run into 429s that clear up on their own.
//
// Only ever wrap idempotent operations in this. Retrying a create can produce
// duplicates, since a failure does not prove the resource was not made.
func Retry(ctx context.Context, attempts int, baseDelay time.Duration, fn func() error) error {
	var err error

	for attempt := 1; attempt <= attempts; attempt++ {
		err = fn()

		if err == nil || !IsTransient(err) {
			return err
		}

		if attempt == attempts {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * baseDelay):
		}
	}

	return fmt.Errorf("giving up after %d attempts: %w", attempts, err)
}
