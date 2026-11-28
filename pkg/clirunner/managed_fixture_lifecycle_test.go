package clirunner

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func managedFixtureJoinBudget() time.Duration {
	if os.Getenv("AGENT_WORKFLOW_TEST_MANAGED_CONTAINER_IMAGE") != "" {
		return 70 * time.Second
	}
	return 35 * time.Second
}

// Callers cancel Serve before joining. The budget diagnoses overdue shutdown;
// only actual done releases ownership of stores, gateways and temporary files.
// A package/stage timeout can fail-stop a stuck worker but cannot prove join.
func managedFixtureJoin(done <-chan struct{}, budget time.Duration, overdue func(error)) error {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		err := fmt.Errorf("managed fixture Serve join overdue after %s", budget)
		overdue(err)
		<-done
		return err
	}
}

func TestManagedRepairExecuteJoinBudget(t *testing.T) {
	for _, container := range []bool{false, true} {
		t.Run(fmt.Sprint("container-", container), func(t *testing.T) {
			image, want := "", 35*time.Second
			if container {
				image, want = "owned-fixture-image", 70*time.Second
			}
			t.Setenv("AGENT_WORKFLOW_TEST_MANAGED_CONTAINER_IMAGE", image)
			if got := managedFixtureJoinBudget(); got != want {
				t.Fatalf("join budget=%s want=%s", got, want)
			}
			done := make(chan struct{})
			close(done)
			awaitManagedServe(t, done) // The real wrapper accepts an actual join.
		})
	}
}
