package clirunner

import (
	"testing"
	"time"
)

// A provider/cleanup regression must fail this fixture before the outer suite
// deadline, including when a failure jumps directly to deferred cancellation.
func awaitManagedServe(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Error("managed worker scheduler did not join after cancellation")
	}
}
