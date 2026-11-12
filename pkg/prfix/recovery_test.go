package prfix

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"os"
	"strings"
	"testing"
)

func TestStoppedChecklistWorkerRecoversOriginalSessionAcrossLostCheckpoints(t *testing.T) {
	for _, fault := range []string{"task_prepare", "task_result"} {
		t.Run(fault, func(t *testing.T) {
			root, provider, req := fixture(t)
			prepared, e := Prepare(context.Background(), root, provider, req)
			if e != nil {
				t.Fatal(e)
			}
			session := &fakeSession{omitChecklist: true, failCheckpoint: "finish"}
			ex, run, runtime := executeFixture(t, root, provider, prepared, session, true)
			lost := false
			runtime.BeforeCheckpoint = func(name string, _ ActionOutput) error {
				if name == fault && !lost {
					lost = true
					return errors.New("lost recovery checkpoint")
				}
				return nil
			}
			_, e = resumeFixture(t, ex, run.ID, true)
			if e == nil || !lost {
				t.Fatal("recovery fault was not exercised", e)
			}
			runtime.BeforeCheckpoint = nil
			run, e = resumeFixture(t, ex, run.ID, true)
			if e != nil {
				t.Fatal(e)
			}
			if run.Status != workflows.RunStatusSucceeded || session.calls["completion"] != 2 {
				t.Fatal(run.Status, session.calls)
			}
		})
	}
}

func TestOptionalNativeFailureCompletesWithOriginalEvidence(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:optional", "required": false}}
	req.Adapters = map[string]Adapter{"custom:optional": {Identity: "custom:optional", Version: "v1", Kind: "inspection", Procedure: [][]Pass{{{ID: "read", Role: "codex", Assignment: "Inspect only.", Evidence: []string{"original"}}}}}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	session := &fakeSession{nativeError: func(o Occurrence) error {
		if o.Kind == "inspection" {
			return errors.New("provider usage limit")
		}
		return nil
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session)
	if run.Status != workflows.RunStatusSucceeded {
		t.Fatalf("settled optional failure blocked completion: %s", run.Status)
	}
	if session.calls["completion"] != 1 {
		t.Fatal("YAML optional task prevented later selected work", session.calls)
	}
	assertAcknowledgedNativeFailure(t, runtime, run.ID)
	path, _ := run.Outputs["private_report"].(string)
	var report struct {
		Optional []string `json:"optional_incomplete"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &report); err != nil || len(report.Optional) != 1 || !strings.Contains(report.Optional[0], "engine://") || !strings.Contains(report.Optional[0], "usage_limit") {
		t.Fatal("optional native omission lost", string(raw), err)
	}
}

func assertAcknowledgedNativeFailure(t *testing.T, runtime *Runtime, run string, expected ...int) {
	t.Helper()
	workers, e := runtime.Workers.List(context.Background(), run)
	if e != nil {
		t.Fatal(e)
	}
	acknowledged := 0
	for _, worker := range workers {
		messages, e := runtime.Workers.Messages(context.Background(), run, worker.ID)
		if e != nil {
			t.Fatal(e)
		}
		for _, msg := range messages {
			if msg.FailureAcknowledgement == nil {
				continue
			}
			acknowledged++
			if msg.Status != workflows.RunStatusFailed || msg.Error == "" || msg.FailureClass == "" || msg.FailureAcknowledgement.MessageID != msg.ID || msg.FailureAcknowledgement.Evidence == "" {
				t.Fatal("original native failure was lost", msg)
			}
		}
	}
	want := 1
	if len(expected) > 0 {
		want = expected[0]
	}
	if acknowledged != want {
		t.Fatal("failure was not acknowledged exactly once", acknowledged)
	}
}

func TestLostFailureAcknowledgementCheckpointRetainsRetry(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"instructions": "Inspect this bounded task."}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	failed := false
	session := &fakeSession{failCheckpoint: "status", nativeError: func(o Occurrence) error {
		if o.Kind == "hook" && !failed {
			failed = true
			return errors.New("provider usage limit")
		}
		return nil
	}}
	ex, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	assertAcknowledgedNativeFailure(t, runtime, run.ID)
	run, e = resumeFixture(t, ex, run.ID, true)
	if e != nil || run.Status != workflows.RunStatusSucceeded {
		t.Fatal("acknowledgement recovery failed", run, e)
	}
	assertAcknowledgedNativeFailure(t, runtime, run.ID)
	for id, calls := range session.calls {
		if id != "completion" && calls != 2 {
			t.Fatal("lost acknowledgement replayed native work", session.calls)
		}
	}
	if session.calls["completion"] != 1 {
		t.Fatal(session.calls)
	}
}

func TestExhaustedNativeRetryRetainsFailuresWithoutCoverage(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"instructions": "Inspect this required bounded task."}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	session := &fakeSession{nativeError: func(o Occurrence) error {
		if o.Kind == "hook" {
			return errors.New("provider usage limit")
		}
		return nil
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	if run.Status == workflows.RunStatusSucceeded || session.calls["completion"] != 0 {
		t.Fatal("exhausted retry was treated as completion", run.Status, session.calls)
	}
	assertAcknowledgedNativeFailure(t, runtime, run.ID, 2)
	workers, e := runtime.Workers.List(context.Background(), run.ID)
	if e != nil {
		t.Fatal(e)
	}
	unhandled := 0
	for _, worker := range workers {
		messages, e := runtime.Workers.Messages(context.Background(), run.ID, worker.ID)
		if e != nil {
			t.Fatal(e)
		}
		for _, msg := range messages {
			if msg.Status == workflows.RunStatusFailed && msg.FailureAcknowledgement == nil {
				unhandled++
			}
		}
	}
	if unhandled != 0 {
		t.Fatal("native error observation receipt was lost", unhandled)
	}
}

func TestOptionalNativeFailureCannotAcknowledgeUnsafeCleanup(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"instructions": "Inspect this optional bounded task.", "required": false}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	session := &fakeSession{nativeError: func(o Occurrence) error {
		if o.Kind == "hook" {
			return process.ErrCleanup
		}
		return nil
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	if run.Status != workflows.RunStatusNeedsAttention || session.calls["completion"] != 0 {
		t.Fatal("optional task bypassed unsafe cleanup", run.Status, session.calls)
	}
	workers, e := runtime.Workers.List(context.Background(), run.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, worker := range workers {
		messages, e := runtime.Workers.Messages(context.Background(), run.ID, worker.ID)
		if e != nil {
			t.Fatal(e)
		}
		for _, msg := range messages {
			if msg.FailureAcknowledgement != nil {
				t.Fatal("unsafe failure was acknowledged", msg)
			}
		}
	}
}

func TestCompletedUnchangedResumeRetainsOriginalAdmission(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	_, _, _ = executeFixture(t, root, provider, prepared, &fakeSession{})
	p, e := ReadPlan(prepared.Plan)
	if e != nil {
		t.Fatal(e)
	}
	resume := ResumeRequest(p, req.Config)
	resume.Capabilities = req.Capabilities
	next, e := Prepare(context.Background(), root, provider, resume)
	if e != nil {
		t.Fatal(e)
	}
	if next.RequestID != prepared.RequestID || next.Workflow != prepared.Workflow {
		t.Fatal("completed resume started a new execution", next, prepared)
	}
}
