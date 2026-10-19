package provider

import (
	"context"
	"testing"
	"time"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// A delete is done only once the object reads as not found, since Northflank
// removes it some time after accepting the delete.
func TestWaitForDeleted(t *testing.T) {
	interval := deletePollInterval
	deletePollInterval = time.Millisecond
	t.Cleanup(func() { deletePollInterval = interval })

	reads := 0

	err := waitForDeleted(t.Context(), "service", "api", func(context.Context) error {
		reads++

		if reads < 3 {
			return nil
		}

		return &northflank.APIError{StatusCode: 404}
	})

	if err != nil {
		t.Fatal(err)
	}

	if reads != 3 {
		t.Errorf("reads = %d, want 3: the wait must last until the object is gone", reads)
	}
}

func TestWaitForDeletedTimesOut(t *testing.T) {
	interval, timeout := deletePollInterval, deleteTimeout
	deletePollInterval, deleteTimeout = time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { deletePollInterval, deleteTimeout = interval, timeout })

	err := waitForDeleted(t.Context(), "service", "api", func(context.Context) error { return nil })

	if err == nil {
		t.Fatal("waitForDeleted returned nil for an object that never goes")
	}
}
