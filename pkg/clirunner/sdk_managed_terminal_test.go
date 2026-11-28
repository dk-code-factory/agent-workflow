package clirunner

import "testing"

// Overdue shutdown fails immediately but retains fixture ownership until the
// actual join. The outer package/stage fail-stop is failed evidence, not join.
func awaitManagedServe(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if err := managedFixtureJoin(done, managedFixtureJoinBudget(), func(err error) { t.Error(err) }); err != nil {
		// Explicit pre-reopen callers must not dispatch more work after an
		// overdue shutdown. Fatal is safe only after the primitive joined.
		t.Fatal(err)
	}
}
