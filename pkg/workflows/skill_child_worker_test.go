package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type childAccountingSession struct {
	*suppliedSession
	fixture skillChildFixture
}

func (f *childAccountingSession) AdmitSkillTask(plan *SkillInvocation) (*SkillInvocation, error) {
	admitted, err := f.suppliedSession.AdmitSkillTask(plan)
	if err != nil {
		return nil, err
	}
	admitted.Model, admitted.ResolvedEffort = "claude-sonnet-4-6", "high"
	admitted.Children[0].Effort = "high"
	admitted.Children[0].Model = admitted.Model
	return admitted, nil
}

func (f *childAccountingSession) UsageProvider(string, string) (string, string) {
	return "claude-sdk", "claude-sonnet-4-6"
}

func (f *childAccountingSession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	session := f.fixture.Session
	for _, kind := range []string{"invocation", "result"} {
		o := SkillObservation{Key: kind + ":skill", Kind: kind, SessionID: session, CallID: "skill", Name: req.Skill.Binding.Name, Arguments: req.Skill.Arguments, Raw: json.RawMessage(`{"original_native_skill":true}`)}
		b, _ := json.Marshal(o)
		var data map[string]any
		_ = json.Unmarshal(b, &data)
		emit(ProviderEvent{Type: "skill_evidence", SessionID: session, Data: data})
	}
	var terminal map[string]any
	for _, original := range f.fixture.Observations {
		// The saved native fixture's custom subtype is selected at admission here.
		b, _ := json.Marshal(original)
		b = []byte(strings.ReplaceAll(string(b), "admitted-child", req.Skill.Children[0].Type))
		var o SkillChildObservation
		_ = json.Unmarshal(b, &o)
		if o.Kind == "parent_terminal" {
			terminal = childRaw(o)
			childObject(childObject(terminal["modelUsage"])["claude-sonnet-4-6"])["inputTokens"] = float64(64)
			o.Raw, _ = json.Marshal(terminal)
		}
		if o.Kind == "permit" {
			if err := req.AuthorizeSkillChild(ctx, o); err != nil {
				return SessionResult{}, err
			}
		} else {
			b, _ = json.Marshal(o)
			var data map[string]any
			_ = json.Unmarshal(b, &data)
			emit(ProviderEvent{Type: "skill_child_evidence", SessionID: session, Data: data})
		}
	}
	// Public parent usage (48) excludes the original child (32). The malformed
	// terminal aggregate64 is above the parent alone, below the retained total80.
	if err := req.ObserveUsage(UsageUpdate{Key: "parent", Provider: "claude-sdk", Model: req.Skill.Model, SessionID: session, Usage: childObject(terminal["usage"])}); err != nil {
		return SessionResult{}, err
	}
	update, err := SkillChildUsage(terminal)
	if err != nil {
		return SessionResult{}, err
	}
	return SessionResult{SessionID: session, Status: RunStatusSucceeded, Outputs: map[string]any{"text": "original parent output", "provider": update.Provider, "model": update.Model, "session_id": session, "usage": update.Usage, "cost_usd": update.CostUSD, "usage_counter_basis": update.CounterBasis}}, nil
}

func TestSkillChildAccountingRejectionFinishesOriginalWorkerAndWait(t *testing.T) {
	_, _, fixture := childLedgerFixture(t, "selected-child")
	source := strings.Replace(skillWorkflow, "evidence: {invocation: required}", fmt.Sprintf("evidence: {invocation: required, children: [review]}, children: [{name: review, prompt: %q, description: %q, tools: [Read], resources: [reference.txt], model: inherit, max-turns: 2, max-calls: 1, background: false}]", fixture.Prompt, fixture.Description), 1)
	_, inputs := suppliedSkillFixture(t)
	e, s, base := skillExecutorFixture(t, source, false)
	runner := &childAccountingSession{suppliedSession: base, fixture: fixture}
	e.Agents = runner
	e.Workers.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return runner, nil, nil, nil
	}
	run, err := e.Prepare(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = RunStatusRunning
	if err = s.Claim(t.Context(), run, RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	_, snap, err := s.Load(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var step Step
	for _, definition := range snap.Definitions {
		step = definition.Jobs["work"].Steps[0]
	}
	ctx := fenceContext(t.Context(), run)
	w, err := e.Workers.Spawn(ctx, WorkerSpec{RunID: run.ID, Role: "native", Definition: step.Definition, Skill: step.Skill, TaskWorkspace: step.Skill.Workspace})
	if err != nil {
		t.Fatal(err)
	}
	original, err := e.Workers.sendTask(ctx, run.ID, w.ID, "original", AgentRequest{Prompt: "Work.", Definition: step.Definition, Skill: step.Skill, Model: step.Skill.Model})
	if err != nil {
		t.Fatal(err)
	}
	waited, err := e.Workers.WaitMessage(ctx, WorkerWaitRequest{RunID: run.ID, OperationID: "original-wait", WorkerID: w.ID, MessageID: original.ID, Deadline: time.Now().Add(5 * time.Second)})
	if err != nil || waited.Status != RunStatusFailed {
		t.Fatal("accounting rejection hung or succeeded", waited, err)
	}
	reopened, err := OpenSQLite(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m := &WorkerManager{Store: reopened, Factory: e.Workers.Factory}
	workers, err := m.List(t.Context(), run.ID)
	if err != nil || len(workers) != 1 {
		t.Fatal(workers, err)
	}
	w = workers[0]
	messages, err := m.Messages(t.Context(), run.ID, w.ID)
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	msg := messages[0]
	if msg.Status != RunStatusFailed || !strings.Contains(msg.Error, "accounting remains incomplete") || !strings.Contains(w.Error, "accounting remains incomplete") || w.Status != RunStatusNeedsAttention || !w.SkillTurnUncertain || w.SkillActiveMessageID != msg.ID || w.Remaining != 31 || len(w.SkillChildPermits) != 1 {
		t.Fatal("original failure/hold/allowance not persisted", w, msg)
	}
	// Wait the exact original message through a fresh SQLite consumer. It must
	// return the failed receipt immediately instead of waiting for its deadline.
	waited, err = m.WaitMessage(t.Context(), WorkerWaitRequest{RunID: run.ID, OperationID: "after-accounting-rejection", WorkerID: w.ID, MessageID: msg.ID, Deadline: time.Now().Add(time.Second)})
	if err != nil || waited.Status != RunStatusFailed || waited.Error != msg.Error {
		t.Fatal("original failed waiter receipt lost", waited, err)
	}
	report, err := reopened.Usage(t.Context(), run.ID)
	if err != nil || report.Summary.Complete || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 80 || report.Summary.KnownCostUSD == nil || *report.Summary.KnownCostUSD != 0.000465 {
		t.Fatal("known original charges lost", report, err)
	}
	rows, err := reopened.ToolCalls(t.Context(), run.ID, true)
	if err != nil || len(rows) != 1 || rows[0].Status == "completed" {
		t.Fatal("incomplete accounting cleared original child hold", rows, err)
	}
	if _, err = m.Send(t.Context(), run.ID, w.ID, "replacement", "Retry"); err == nil {
		t.Fatal("failed original accounting dispatched replacement")
	}
	if len(runner.requests) != 1 {
		t.Fatal("original turn was replaced", len(runner.requests))
	}
}
