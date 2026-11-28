package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func managedTaskFixture(t *testing.T, provider string) (*SQLiteStore, context.Context, *Run, Attempt, Attempt) {
	t.Helper()
	s, run := executionStore(t)
	run.Status = RunStatusRunning
	if err := s.Claim(t.Context(), run, RunStatusFailed); err != nil {
		t.Fatal(err)
	}
	ctx := fenceContext(t.Context(), run)
	parent, _, err := s.BeginConversation(ctx, run.ID, "job", "conversation", 1, "original", "")
	if err != nil {
		t.Fatal(err)
	}
	parent.Conversation = &ConversationState{Roles: []string{"a", "b"}, Active: &ConversationDelivery{ID: "delivery", From: "engine", To: "a", Message: "original"}, Calls: 1, MaxCalls: 2, MaxMessages: 4, Deadline: time.Now().UTC().Add(time.Minute)}
	if err = s.RecordAttempt(ctx, parent); err != nil {
		t.Fatal(err)
	}
	turn, _, err := s.Begin(ctx, run.ID, "job", "conversation/conversation/0", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx = invocationContext(ctx, s, &turn)
	ctx = withManagedConversationAttempt(ctx, &conversationExecution{e: &Executor{Store: s}, attempt: &parent, state: parent.Conversation}, turn, nil)
	identity := UsageIdentity{ID: turn.ID, RunID: run.ID, TaskAttemptID: turn.ID, Kind: "task", Provider: provider, Model: "fixture"}
	if err = s.BeginUsage(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if provider == "claude-sdk" {
		err = BeginManagedSDKInvocation(ctx, run.ID, turn.ID, "fixture")
	} else {
		err = BeginManagedCodexInvocation(ctx, run.ID, turn.ID, "build", "policy", "fixture")
	}
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, run, parent, turn
}
func TestManagedConversationOriginalCoordinateHold(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		t.Run(provider, func(t *testing.T) {
			s, ctx, run, parent, turn := managedTaskFixture(t, provider)
			before, _ := s.Attempts(ctx, run.ID)
			// Retry, replay-safe, changed input and parent replay cannot erase an older
			// unresolved native attempt, even if a newer row claims success.
			later := turn
			later.ID = "later-success"
			later.Number++
			later.Status = RunStatusSucceeded
			later.FinishedAt = time.Now().UTC()
			raw, _ := json.Marshal(later)
			if _, err := s.db.Exec(`INSERT INTO attempts VALUES(?,?,?,?,?,?,?,?)`, later.ID, later.RunID, later.JobID, later.StepID, later.Iteration, later.Number, later.Status, raw); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.BeginInput(ctx, run.ID, turn.JobID, turn.StepID, turn.Iteration, true, "changed", true, ""); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("older unresolved marker bypassed", err)
			}
			if _, _, err := s.BeginConversation(ctx, run.ID, parent.JobID, parent.StepID, parent.Iteration, "original", ""); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("parent replay reset active turn", err)
			}
			after, _ := s.Attempts(ctx, run.ID)
			if len(after) != len(before)+1 {
				t.Fatal("hold mutated attempt history", len(before), len(after))
			}
			// Bind a distinct physical sibling to the same actual logical execution.
			snapRaw, _ := json.Marshal(Snapshot{Version: 1, Metadata: map[string]any{"iteration_logical_run_id": "logical-original"}})
			if _, err := s.db.Exec(`UPDATE runs SET snapshot=? WHERE id=?`, snapRaw, run.ID); err != nil {
				t.Fatal(err)
			}
			sibling := &Run{ID: "sibling", Status: RunStatusRunning, Jobs: map[string]JobExecution{}}
			if err := s.Admit(t.Context(), sibling, Snapshot{Version: 1, Metadata: map[string]any{"iteration_logical_run_id": "logical-original"}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.BeginInput(t.Context(), sibling.ID, turn.JobID, turn.StepID, 1, true, "changed", true, "logical-original"); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("logical sibling granted new attempt", err)
			}
			// Existing settled journals remain reusable; merely flipping marker state
			// without atomic final accounting must still be rejected.
			path := "$.managed_sdk_attempt.state"
			state := "settled"
			key := managedSDKAttemptKey
			if provider == "codex-app-server" {
				path = "$.managed_codex_admission.state"
				state = "completed"
				key = managedCodexAdmissionKey
			}
			if _, err := s.db.Exec(`UPDATE usage_observations SET record=json_set(record,?,?) WHERE invocation_id=? AND observation_key=?`, path, state, turn.ID, key); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Begin(ctx, run.ID, turn.JobID, turn.StepID, 1, true); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("uncommitted final accounting bypassed", err)
			}
			if _, err := s.db.Exec(`UPDATE usage_invocations SET record=json_set(record,'$.finished',json('true')) WHERE id=?`, turn.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE attempts SET status='succeeded' WHERE id=?`, turn.ID); err != nil {
				t.Fatal(err)
			}
			got, reused, err := s.Begin(ctx, run.ID, turn.JobID, turn.StepID, 1, true)
			if err != nil || !reused || got.ID != later.ID {
				t.Fatal("settled committed journal not reused", got, reused, err)
			}
		})
	}
}
func TestManagedConversationTaskOriginAndAuthority(t *testing.T) {
	s, ctx, run, parent, turn := managedTaskFixture(t, "claude-sdk")
	binding, _ := runtimeexec.BindingFrom(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tag := ctx.Value(managedTaskKey{}).(managedTaskAdmission)
	if _, err = managedTaskSnapshot(ctx, tx, binding, turn.ID, tag); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	for _, change := range []string{"deadline", "active", "calls"} {
		t.Run(change, func(t *testing.T) {
			altered := parent
			copy := *parent.Conversation
			altered.Conversation = &copy
			switch change {
			case "deadline":
				copy.Deadline = copy.Deadline.Add(time.Minute)
			case "active":
				copy.Active = &ConversationDelivery{ID: "foreign"}
			case "calls":
				copy.Calls++
			}
			if err := s.RecordAttempt(ctx, altered); err != nil {
				t.Fatal(err)
			}
			if err := CheckManagedSDKAuthority(ctx, run.ID, turn.ID); err == nil {
				t.Fatal("changed persisted allowance admitted")
			}
			if err := s.RecordAttempt(ctx, parent); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A nested physical attempt can use only its actual persisted origin owner.
	physical := &Run{ID: "physical-child", OriginID: run.ID, Status: RunStatusRunning, Jobs: map[string]JobExecution{}}
	if err := s.Admit(ctx, physical, Snapshot{Version: 1, Metadata: map[string]any{"origin_run_id": run.ID}}); err != nil {
		t.Fatal(err)
	}
	nestedParent := parent
	nestedParent.ID = "nested-parent"
	nestedParent.RunID = physical.ID
	nestedTurn := turn
	nestedTurn.ID = "nested-turn"
	nestedTurn.RunID = physical.ID
	for _, a := range []Attempt{nestedParent, nestedTurn} {
		raw, _ := json.Marshal(a)
		if _, err := s.db.Exec(`INSERT INTO attempts VALUES(?,?,?,?,?,?,?,?)`, a.ID, a.RunID, a.JobID, a.StepID, a.Iteration, a.Number, a.Status, raw); err != nil {
			t.Fatal(err)
		}
	}
	nestedCtx := withManagedConversationAttempt(ctx, &conversationExecution{attempt: &nestedParent, state: nestedParent.Conversation}, nestedTurn, nil)
	nestedTag := nestedCtx.Value(managedTaskKey{}).(managedTaskAdmission)
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = managedTaskSnapshot(nestedCtx, tx, binding, nestedTurn.ID, nestedTag); err != nil {
		t.Fatal("real nested origin rejected", err)
	}
	foreign := binding
	foreign.RunID = "foreign"
	if _, err = managedTaskSnapshot(nestedCtx, tx, foreign, nestedTurn.ID, nestedTag); err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatal("foreign origin admitted", err)
	}
	tx.Rollback()
}

func TestManagedConversationReplayQueryUsesOriginIndex(t *testing.T) {
	s, _, run, _, _ := managedTaskFixture(t, "claude-sdk")
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+managedTaskCoordinateQuery, run.ID, run.ID, managedSDKAttemptKey, managedCodexAdmissionKey, "job", "conversation", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		t.Log(detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(plan, "\n")
	if !strings.Contains(all, "usage_origin") || strings.Contains(all, "SCAN u") || strings.Contains(all, "SCAN r") || !strings.Contains(all, "SEARCH u USING INDEX") {
		t.Fatal("global historical observation scan", all)
	}
}

type managedConversationSession struct {
	*conversationSession
	provider string
}

func (s *managedConversationSession) UsageProvider(string, string) (string, string) {
	return s.provider, "fixture"
}

func TestManagedConversationRedactedAuthority(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		for _, change := range []string{"unchanged", "persisted-prompt", "persisted-delivery", "live-prompt-collision", "live-delivery-collision"} {
			t.Run(provider+"/"+change, func(t *testing.T) {
				secrets := map[string]string{"first": "credential-one", "second": "credential-two"}
				var store *SQLiteStore
				var rejected bool
				runner := &managedConversationSession{provider: provider}
				runner.conversationSession = newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
					var result SessionResult
					tag := ctx.Value(managedTaskKey{}).(managedTaskAdmission)
					if req.AgentID == "reviewer" && change != "unchanged" {
						if strings.HasPrefix(change, "live-") {
							if change == "live-prompt-collision" {
								tag.parent.Conversation.Prompt = strings.ReplaceAll(tag.parent.Conversation.Prompt, secrets["first"], secrets["second"])
							} else {
								tag.parent.Conversation.Active.Message = strings.ReplaceAll(tag.parent.Conversation.Active.Message, secrets["first"], secrets["second"])
							}
						} else {
							parent := redactAttempt(*tag.parent, secrets)
							if change == "persisted-prompt" {
								parent.Conversation.Prompt += " changed"
							} else {
								active := *parent.Conversation.Active
								active.ID = "foreign-delivery"
								parent.Conversation.Active = &active
							}
							if err := store.RecordAttempt(ctx, parent); err != nil {
								return result, err
							}
						}
					}
					var err error
					if provider == "claude-sdk" {
						err = BeginManagedSDKInvocation(ctx, req.RunID, req.InvocationID, "fixture")
					} else {
						err = BeginManagedCodexInvocation(ctx, req.RunID, req.InvocationID, "build", "policy", "fixture")
					}
					if req.AgentID == "reviewer" && change != "unchanged" {
						rejected = err != nil
						if err == nil {
							err = errors.New("changed authority admitted")
						}
						return result, err
					}
					if err != nil {
						return result, err
					}
					result = conversationEvent(req, emit)
					if req.AgentID == "builder" {
						err = conversationCall(ctx, req, "session-send", "send", `{"to":"reviewer","message":"review credential-one"}`)
					} else {
						err = conversationCall(ctx, req, "session-finish", "finish", `{"answer":"credential-one"}`)
					}
					if err != nil {
						return result, err
					}
					if provider == "claude-sdk" {
						if err = BindManagedSDKModel(ctx, req.RunID, req.InvocationID, "fixture", "fixture"); err != nil {
							return result, err
						}
						usage := map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
						if err = req.ObserveUsage(UsageUpdate{Key: "sdk-response:final", Provider: provider, Model: "fixture", SessionID: result.SessionID, Usage: usage, Incomplete: true}); err != nil {
							return result, err
						}
						result.ProviderStatus, result.ProviderVersion = "success", "claude-agent-sdk@0.3.322"
						result.Outputs["usage"], result.Outputs["cost_usd"] = usage, float64(0)
						return result, CheckpointManagedSDKTerminal(ctx, result)
					}
					if err = BindManagedCodexInvocation(ctx, req.RunID, req.InvocationID, result.SessionID, "turn"); err != nil {
						return result, err
					}
					w := managedUsageWitness("final", 10)
					w.SessionID, w.TurnID = result.SessionID, "turn"
					w.Batch = mustManagedRecoveryJSON(managedCodexBatchIdentity{ThreadID: w.SessionID, TurnID: w.TurnID, InvocationID: req.InvocationID, PolicyID: w.PolicyID, BuildID: w.BuildID, BatchID: "batch-final", ResponseID: w.ResponseID, CallIDs: []string{}})
					if err = RecordManagedCodexUsage(ctx, req.RunID, req.InvocationID, w); err != nil {
						return result, err
					}
					result.ProviderStatus, result.ProviderVersion = "completed", "agent-workflow-managed-v1/build"
					result.Outputs["usage"], result.Outputs["usage_incomplete"] = w.Usage, false
					return result, CheckpointManagedCodexTerminal(ctx, result, w.ResponseID, w.Digest)
				})
				source := strings.Replace(conversationYAML, "inputs.task", "secrets.first", 1)
				e, s := durableExecutor(t, source, runner)
				store = s
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}, Secrets: secrets})
				if change != "unchanged" {
					if err == nil || !rejected {
						t.Fatalf("authority substitution was not rejected: run=%+v rejected=%v err=%v", run, rejected, err)
					}
					return
				}
				if err != nil || run.Status != RunStatusSucceeded || runner.calls.Load() != 2 {
					t.Fatalf("unchanged redacted authority did not complete: run=%+v calls=%d err=%v", run, runner.calls.Load(), err)
				}
				attempts, err := store.Attempts(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				state := attempts[0].Conversation
				if state.Prompt != "Task: [REDACTED]" || state.Transcript[0].Message != "review [REDACTED]" || state.Result.(map[string]any)["answer"] != "[REDACTED]" {
					t.Fatalf("conversation data did not redact: %+v", state)
				}
				if state.Sessions["builder"] != "session-builder" || state.Sessions["reviewer"] != "session-reviewer" || state.Transcript[0].ID != "solution/conversation/0/session-send" {
					t.Fatalf("protocol identities changed: %+v", state)
				}
				if _, ok := state.Receipts["solution/conversation/0/session-send"]; !ok {
					t.Fatal("receipt identity changed")
				}
				rows, err := store.db.Query(`SELECT record FROM attempts UNION ALL SELECT record FROM usage_observations UNION ALL SELECT record FROM usage_invocations`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					var raw string
					if err = rows.Scan(&raw); err != nil {
						t.Fatal(err)
					}
					for _, secret := range secrets {
						if strings.Contains(raw, secret) {
							t.Fatal("secret persisted in journal")
						}
					}
				}
				if err = rows.Err(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type managedReusableGate struct{ admitted <-chan struct{} }

func (g managedReusableGate) Admission(context.Context, map[string]*Workflow) (map[string]any, error) {
	return nil, nil
}
func (g managedReusableGate) RunAction(ctx context.Context, req ActionRequest) (map[string]any, error) {
	if req.With["commit"] == "right" {
		select {
		case <-g.admitted:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return map[string]any{}, nil
}

func TestManagedConversationIndependentReusableChildren(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		for _, mode := range []string{"admitted-edge", "nested-edge", "legacy-edge-metadata-label"} {
			legacy := mode == "legacy-edge-metadata-label"
			t.Run(provider+"/"+mode, func(t *testing.T) {
				left, right := make(chan struct{}), make(chan struct{})
				var store *SQLiteStore
				runner := &managedConversationSession{provider: provider}
				runner.conversationSession = newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
					var result SessionResult
					var err error
					if provider == "claude-sdk" {
						err = BeginManagedSDKInvocation(ctx, req.RunID, req.InvocationID, "fixture")
					} else {
						err = BeginManagedCodexInvocation(ctx, req.RunID, req.InvocationID, "build", "policy", "fixture")
					}
					if err != nil {
						return result, err
					}
					if req.AgentID == "builder" {
						if strings.Contains(req.Prompt, "left") {
							if legacy {
								tag := ctx.Value(managedTaskKey{}).(managedTaskAdmission)
								// Older admitted snapshots have no private edge. A public
								// metadata label must not manufacture isolation for them.
								if _, err = store.db.ExecContext(ctx, `UPDATE runs SET snapshot=json_remove(snapshot,'$.reusable_invocation') WHERE id=?`, tag.Attempt.RunID); err != nil {
									return result, err
								}
							}
							close(left)
							select {
							case <-right:
							case <-ctx.Done():
								return result, ctx.Err()
							}
						} else {
							close(right)
						}
					}
					result = conversationEvent(req, emit)
					if req.AgentID == "builder" {
						err = conversationCall(ctx, req, "send", "send", `{"to":"reviewer","message":"review"}`)
					} else {
						err = conversationCall(ctx, req, "finish", "finish", `{"answer":"approved"}`)
					}
					if err != nil {
						return result, err
					}
					if provider == "claude-sdk" {
						if err = BindManagedSDKModel(ctx, req.RunID, req.InvocationID, "fixture", "fixture"); err != nil {
							return result, err
						}
						usage := map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
						if err = req.ObserveUsage(UsageUpdate{Key: "sdk-response:final", Provider: provider, Model: "fixture", SessionID: result.SessionID, Usage: usage, Incomplete: true}); err != nil {
							return result, err
						}
						result.ProviderStatus, result.ProviderVersion = "success", "claude-agent-sdk@0.3.322"
						result.Outputs["usage"], result.Outputs["cost_usd"] = usage, float64(0)
						return result, CheckpointManagedSDKTerminal(ctx, result)
					}
					if err = BindManagedCodexInvocation(ctx, req.RunID, req.InvocationID, result.SessionID, "turn"); err != nil {
						return result, err
					}
					w := managedUsageWitness("final", 10)
					w.SessionID, w.TurnID = result.SessionID, "turn"
					w.Batch = mustManagedRecoveryJSON(managedCodexBatchIdentity{ThreadID: w.SessionID, TurnID: w.TurnID, InvocationID: req.InvocationID, PolicyID: w.PolicyID, BuildID: w.BuildID, BatchID: "batch-final", ResponseID: w.ResponseID, CallIDs: []string{}})
					if err = RecordManagedCodexUsage(ctx, req.RunID, req.InvocationID, w); err != nil {
						return result, err
					}
					result.ProviderStatus, result.ProviderVersion = "completed", "agent-workflow-managed-v1/build"
					result.Outputs["usage"], result.Outputs["usage_incomplete"] = w.Usage, false
					return result, CheckpointManagedCodexTerminal(ctx, result, w.ResponseID, w.Digest)
				})
				e, journal := durableExecutor(t, "jobs:\n  left:\n    uses: workflows/child.yml\n    with: {task: left}\n  right:\n    uses: workflows/child.yml\n    with: {task: right}\n", runner)
				store = journal
				e.MaxParallel = 4
				e.Actions = managedReusableGate{admitted: left}
				child := "on:\n  workflow_call:\n    inputs:\n      task: {type: string}\n" + conversationYAML
				child = strings.Replace(child, "    steps:\n", "    steps:\n      - id: gate\n        uses: workspace/create\n        with: {commit: '${{ inputs.task }}'}\n", 1)
				writeWorkflow(t, e.WorkspaceDir, "child.yml", child)
				if mode == "nested-edge" {
					writeWorkflow(t, e.WorkspaceDir, "main.yml", "jobs:\n  left:\n    uses: workflows/middle.yml\n    with: {task: left}\n  right:\n    uses: workflows/middle.yml\n    with: {task: right}\n")
					writeWorkflow(t, e.WorkspaceDir, "middle.yml", "on:\n  workflow_call:\n    inputs:\n      task: {type: string}\njobs:\n  nested:\n    uses: workflows/child.yml\n    with: {task: '${{ inputs.task }}'}\n")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml", Metadata: map[string]any{"reusable_invocation": map[string]any{"parent_run_id": "caller-parent", "job_id": "caller-job", "iteration": 1}}})
				if legacy {
					if err == nil || !strings.Contains(err.Error(), "original managed conversation attempt") || runner.calls.Load() != 1 {
						t.Fatalf("legacy original escaped its hold: calls=%d err=%v", runner.calls.Load(), err)
					}
					select {
					case <-right:
						t.Fatal("metadata label admitted a second provider")
					default:
					}
					return
				}
				if err != nil || run.Status != RunStatusSucceeded || runner.calls.Load() != 4 {
					t.Fatalf("independent reusable conversations did not finish: calls=%d err=%v", runner.calls.Load(), err)
				}
				if run.Jobs["left"].ChildRunID == run.Jobs["right"].ChildRunID {
					t.Fatal("parallel jobs did not retain distinct execution identities")
				}
				_, rootSnapshot, loadErr := store.Load(ctx, run.ID)
				if loadErr != nil || rootSnapshot.ReusableInvocation != nil {
					t.Fatal("caller metadata manufactured a reusable edge", loadErr)
				}
				for _, job := range []string{"left", "right"} {
					child, snapshot, loadErr := store.Load(ctx, reusableChildID(run.ID, job, 1))
					if loadErr != nil || snapshot.ReusableInvocation == nil || snapshot.ReusableInvocation.ParentRunID != run.ID || snapshot.ReusableInvocation.JobID != job || snapshot.ReusableInvocation.Iteration != 1 || child.OriginID != run.ID || child.Status != RunStatusSucceeded {
						t.Fatal("child execution lost common origin or completion", child, loadErr)
					}
				}
			})
		}
	}
}
