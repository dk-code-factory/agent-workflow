package workflows

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
)

func checkProtocolWireSizes(t *testing.T, d *workflowDocker) {
	t.Helper()
	helper := d.helper
	d.helper = func(ctx context.Context, argv []string, input []byte, env []string) ([]byte, []byte, int, bool) {
		stdout, stderr, code, handled := helper(ctx, argv, input, env)
		if handled && (len(input) > 1<<20 || len(stdout) > 1<<20) {
			t.Errorf("individual helper frame exceeded 1 MiB: request=%d response=%d", len(input), len(stdout))
		}
		return stdout, stderr, code, handled
	}
}

func TestStructuredRecoveryBudgetRejectsBeforeMutation(t *testing.T) {
	e, store, d := containerExecutor(t)
	payload := strings.Repeat("a", 600<<10)
	f := &protocolFixture{binding: payload, mutation: true, lostResponse: true}
	source := strings.ReplaceAll(structuredWorkflow, "effect: read_only", "effect: external_mutation")
	source = strings.Replace(source, "input: {value: ready}", "input: {value: "+payload+"}", 1)
	installProtocolWorkflow(t, e, d, f, source)
	checkProtocolWireSizes(t, d)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err == nil || !strings.Contains(err.Error(), "combined input/bindings/result budget") || run == nil {
		t.Fatalf("unrecoverable mutation input/bindings accepted: run=%v err=%v", run != nil, err)
	}
	f.mu.Lock()
	executions := f.execute
	f.mu.Unlock()
	if executions != 0 {
		t.Fatalf("mutation dispatched before recovery feasibility check: %d", executions)
	}
	records, err := store.ActionAttempts(t.Context(), run.ID)
	if err != nil || len(records) != 0 {
		t.Fatalf("unrecoverable attempt admitted: records=%d err=%v", len(records), err)
	}
}

func TestStructuredOversizedResultRetainsMutationBarrier(t *testing.T) {
	e, store, d := containerExecutor(t)
	payload := strings.Repeat("a", 600<<10)
	f := &protocolFixture{binding: payload, output: payload, mutation: true, unknown: 1}
	source := strings.ReplaceAll(structuredWorkflow, "effect: read_only", "effect: external_mutation")
	installProtocolWorkflow(t, e, d, f, source)
	checkProtocolWireSizes(t, d)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err == nil || !strings.Contains(err.Error(), "combined input/bindings/result budget") || run == nil {
		t.Fatalf("unrecoverable completion accepted: run=%v err=%v", run != nil, err)
	}
	records, err := store.ActionAttempts(t.Context(), run.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("missing mutation barrier: records=%d err=%v", len(records), err)
	}
	r := records[0]
	if r.State != "admitted" || len(r.Result.Output) != 0 || len(r.Result.Evidence) != 0 || len(r.Result.Receipt) != 0 {
		t.Fatalf("oversized candidate retained or mutation barrier released: state=%s result=%+v", r.State, r.Result)
	}
	f.mu.Lock()
	f.output = ""
	f.mu.Unlock()
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil {
		t.Fatal("unknown mutation reconciliation permitted reuse")
	}
	records, err = store.ActionAttempts(t.Context(), run.ID)
	if err != nil || len(records) != 1 || records[0].State != "admitted" || len(records[0].Reconciliations) != 1 || records[0].Reconciliations[0].Outcome != "unknown" {
		t.Fatalf("unknown observation did not preserve durable barrier: records=%+v err=%v", records, err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatalf("reconciled checkpoint cannot be reused: %v", err)
	}
	f.mu.Lock()
	executions, reconciles := f.execute, f.reconciles
	f.mu.Unlock()
	if executions != 1 || reconciles != 2 {
		t.Fatalf("mutation replayed instead of authoritative reconciliation: executions=%d reconciles=%d", executions, reconciles)
	}
	records, err = store.ActionAttempts(t.Context(), run.ID)
	if err != nil || len(records) != 1 || records[0].State != "completed" || len(records[0].Reconciliations) != 2 {
		t.Fatalf("authoritative completion not retained: records=%+v err=%v", records, err)
	}
}

func TestStructuredLargeRecoverableCheckpointReusesWithoutReplay(t *testing.T) {
	e, _, d := containerExecutor(t)
	payload := strings.Repeat("a", 300<<10)
	f := &protocolFixture{binding: payload, output: payload}
	installProtocolWorkflow(t, e, d, f, structuredWorkflow)
	checkProtocolWireSizes(t, d)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["capture"].Outputs["head"] != payload {
		t.Fatal("large business result was changed")
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execute != 1 || f.validations == 0 {
		t.Fatalf("large checkpoint not reused: executions=%d validations=%d", f.execute, f.validations)
	}
}

func TestStructuredHelperCredentialsDetachCallerMaps(t *testing.T) {
	for _, mode := range []string{"run", "resume"} {
		t.Run(mode, func(t *testing.T) {
			e, _, d := containerExecutor(t)
			f := &protocolFixture{binding: "resource"}
			source := strings.Replace(structuredWorkflow, "        argv: [helper]", "        argv: [helper]\n        env: {TOKEN: '${{ secrets.token }}'}", 1)
			installProtocolWorkflow(t, e, d, f, source)
			var runID string
			if mode == "resume" {
				run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", Secrets: map[string]string{"token": "original-secret"}})
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
			}
			f.mu.Lock()
			start := len(f.calls)
			f.mu.Unlock()
			caller := map[string]string{"token": "admitted-secret"}
			var changed sync.Once
			d.helper = func(ctx context.Context, argv []string, input []byte, env []string) ([]byte, []byte, int, bool) {
				stdout, stderr, code, handled := f.call(ctx, argv, input, env)
				var request struct{ Phase string }
				if json.Unmarshal(input, &request) == nil && request.Phase == "bindings" {
					changed.Do(func() { caller["token"] = "changed-after-admission" })
				}
				return stdout, stderr, code, handled
			}
			var err error
			if mode == "run" {
				_, err = e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", Secrets: caller})
			} else {
				_, err = e.Resume(t.Context(), runID, caller, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.calls[start:]) < 2 {
				t.Fatal("did not exercise hooks after caller changed its map")
			}
			for _, call := range f.calls[start:] {
				if !slices.Contains(call.env, "TOKEN=admitted-secret") || slices.Contains(call.env, "TOKEN=changed-after-admission") {
					t.Errorf("%s read caller-owned credential map: %v", call.phase, call.env)
				}
			}
		})
	}
}
