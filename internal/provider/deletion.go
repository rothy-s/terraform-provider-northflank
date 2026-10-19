package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// How long to wait for a deleted service or job to go, and how often to look.
// Variables so that tests can shorten them.
var (
	deleteTimeout      = 15 * time.Minute
	deletePollInterval = 10 * time.Second
)

// waitForDeleted polls get until it reports the object not found. Northflank
// accepts a delete before it has removed the object, which can take minutes,
// and the name stays taken until then: without the wait, a replacement fails
// to create its successor with "already exists".
func waitForDeleted(ctx context.Context, noun, id string, get func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	timedOut := fmt.Errorf("timed out after %s waiting for %s %q to be deleted", deleteTimeout, noun, id)

	for {
		err := get(ctx)

		if isNotFound(err) {
			return nil
		}

		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return timedOut
		}

		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return timedOut
		case <-time.After(deletePollInterval):
		}
	}
}
