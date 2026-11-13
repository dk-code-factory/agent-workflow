package workflows

import (
	"context"
	"testing"
)

// Downgrade fixtures must remove the newer tables before replaying migration.
const dropTaskStateFixtureSchema = `DROP TABLE task_state_requests; DROP TABLE task_claims; DROP TABLE task_budgets; DROP TABLE task_deadlines; DROP TABLE task_group_members; DROP TABLE task_groups; `

func TestTaskGroupAdmissionPreservesReplacementIdentity(t *testing.T) {
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", "jobs: {one: {steps: [{uses: agent/fake, with: {prompt: test}}]}}")
	store, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	executor := &Executor{WorkspaceDir: root, Store: store, Agents: agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		return map[string]any{"text": "done"}, nil
	})}
	for _, prepare := range []bool{true, false} {
		for i := 0; i < 2; i++ {
			req := RunRequest{Ref: "workflows/main.yml", TaskGroup: "logical-work", Metadata: map[string]any{"custom": "keep"}}
			var run *Run
			if prepare {
				run, err = executor.Prepare(context.Background(), req)
			} else {
				run, err = executor.Run(context.Background(), req)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, snapshot, err := store.Load(context.Background(), run.ID)
			if err != nil || snapshot.Metadata["task_group"] != "logical-work" || snapshot.Metadata["custom"] != "keep" {
				t.Fatalf("task group was not retained: %+v, %v", snapshot.Metadata, err)
			}
		}
	}
}

func TestTaskGroupAdmissionRejectsConflictingMetadata(t *testing.T) {
	for _, metadata := range []map[string]any{{"task_group": 3}, {"task_group": "different"}} {
		req := RunRequest{TaskGroup: "selected", Metadata: metadata}
		if err := admitTaskGroup(&req); err == nil {
			t.Fatal("conflicting task group accepted")
		}
	}
}
