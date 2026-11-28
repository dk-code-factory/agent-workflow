package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type managedPolicyTerminalSession struct {
	*nativePolicyFixture
	provider        string
	mode            string
	calls           int
	afterCheckpoint func(context.Context) error
}

func (s *managedPolicyTerminalSession) UsageProvider(string, string) (string, string) {
	return s.provider, "fixture"
}

func (s *managedPolicyTerminalSession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	s.calls++
	result := SessionResult{Status: RunStatusSucceeded, SessionID: "original-session", Outputs: map[string]any{"text": "PRIVATE", "tool_policy": "provider annotation"}}
	var err error
	if s.provider == "claude-sdk" {
		err = BeginManagedSDKInvocation(ctx, req.RunID, req.InvocationID, "fixture")
	} else {
		err = BeginManagedCodexInvocation(ctx, req.RunID, req.InvocationID, "build", "policy", "fixture")
	}
	if err != nil {
		return result, err
	}
	emit(ProviderEvent{Type: "session", SessionID: result.SessionID})
	if req.Definition != nil && req.Definition.NativePolicy != nil {
		result.TaskPolicy = &NativeTaskPolicyEvidence{Grant: *req.Definition.NativePolicy, Agent: req.ParentWorkerID, InvocationID: req.InvocationID, SessionFingerprint: NativeTaskSessionFingerprint(result.SessionID)}
	}
	if s.provider == "claude-sdk" {
		if err = BindManagedSDKModel(ctx, req.RunID, req.InvocationID, "fixture", "fixture"); err != nil {
			return result, err
		}
		usage := map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
		if err = req.ObserveUsage(UsageUpdate{Key: "sdk-response:final", Provider: s.provider, Model: "fixture", SessionID: result.SessionID, Usage: usage, Incomplete: true}); err != nil {
			return result, err
		}
		result.ProviderStatus, result.ProviderVersion = "success", "claude-agent-sdk@0.3.322"
		result.Outputs["usage"], result.Outputs["cost_usd"] = usage, float64(0)
		if s.mode != "unknown" {
			err = CheckpointManagedSDKTerminal(ctx, result)
		}
	} else {
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
		if s.mode != "unknown" {
			err = CheckpointManagedCodexTerminal(ctx, result, w.ResponseID, w.Digest)
		}
	}
	if err != nil {
		return result, err
	}
	switch s.mode {
	case "policy":
		result.TaskPolicy.Grant.Mode = "unrestricted"
	case "annotation", "no-policy-annotation":
		result.Outputs["tool_policy"] = "changed annotation"
	case "output":
		result.Outputs["text"] = "changed output"
	case "identity":
		result.SessionID = "changed-session"
	}
	if s.afterCheckpoint != nil {
		err = s.afterCheckpoint(ctx)
	}
	return result, err
}

func TestManagedNativePolicyTerminalPublication(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		for _, mode := range []string{"success", "no-policy", "no-policy-annotation", "policy", "annotation", "output", "identity", "unknown", "cancelled", "recovery", "retained-policy", "retained-output"} {
			t.Run(fmt.Sprintf("%s/%s", provider, mode), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				m, runID := workerFixture(t, &fakeSession{})
				run, _, err := m.Store.Load(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				if err = m.Store.Claim(ctx, run, RunStatusRunning); err != nil {
					t.Fatal(err)
				}
				f := &managedPolicyTerminalSession{provider: provider, mode: mode, nativePolicyFixture: &nativePolicyFixture{nativeTaskFixture: &nativeTaskFixture{}}}
				m.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
					return f, []string{"Read"}, map[string]string{"credential": "PRIVATE"}, nil
				}
				definition, err := f.AdmitNativeTask(&AgentDefinition{ToolPolicy: "read-only"})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "no-policy" || mode == "no-policy-annotation" {
					definition = &AgentDefinition{}
				}
				worker, err := m.Spawn(ctx, WorkerSpec{RunID: runID, Role: "reviewer", Turns: 2, Tools: []string{"Read"}, Definition: definition})
				if err != nil {
					t.Fatal(err)
				}
				message, err := m.Send(ctx, runID, worker.ID, "original", "validate")
				if err != nil {
					t.Fatal(err)
				}
				recovering := mode == "recovery" || mode == "retained-policy" || mode == "retained-output"
				if recovering {
					f.afterCheckpoint = func(tc context.Context) error { return m.Store.Claim(tc, run, RunStatusRunning) }
				} else if mode == "cancelled" {
					f.afterCheckpoint = func(context.Context) error { cancel(); return context.Canceled }
				}
				m.turn(ctx, worker, nil)
				if recovering {
					if err = m.Store.Recover(ctx); err != nil {
						t.Fatal(err)
					}
					current, err := m.Store.QueueResume(ctx, runID, "new-owner", true, 128)
					if err != nil {
						t.Fatal(err)
					}
					current.Status = RunStatusRunning
					if err = m.Store.Claim(ctx, current, RunStatusQueued); err != nil {
						t.Fatal(err)
					}
					if err = m.Init(ctx); err != nil {
						t.Fatal(err)
					}
					worker, err = m.Get(ctx, runID, worker.ID)
					if err != nil {
						t.Fatal(err)
					}
					if mode != "recovery" {
						key, field := managedSDKAttemptKey, "managed_sdk_attempt"
						if provider == "codex-app-server" {
							key, field = "codex-response:final", "managed_codex_recovery"
						}
						var raw []byte
						if err = m.Store.db.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, message.ID, key).Scan(&raw); err != nil {
							t.Fatal(err)
						}
						var fields map[string]any
						if err = decodeJSONWithNumbers(raw, &fields); err != nil {
							t.Fatal(err)
						}
						terminal := fields[field].(map[string]any)["terminal"].(map[string]any)
						if mode == "retained-policy" {
							terminal["task_policy"].(map[string]any)["grant"].(map[string]any)["mode"] = "unrestricted"
						} else {
							terminal["outputs"].(map[string]any)["text"] = "changed"
						}
						raw, _ = json.Marshal(fields)
						if _, err = m.Store.db.ExecContext(ctx, `UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key=?`, raw, message.ID, key); err != nil {
							t.Fatal(err)
						}
					}
					candidate, err := m.admitManagedCodexRecovery(ctx, worker)
					if mode != "recovery" {
						if err == nil || candidate != nil {
							t.Fatal("altered retained terminal admitted", candidate, err)
						}
						return
					}
					if err != nil {
						t.Fatal("original terminal recovery rejected", err)
					}
					recoveryCtx := context.WithValue(ctx, managedCodexResumeKey{}, managedCodexResumeContext{message: candidate.message, recovery: candidate.recovery, sdk: candidate.sdk})
					m.turn(recoveryCtx, candidate.worker, nil)
				}
				messages, err := m.Messages(context.WithoutCancel(ctx), runID, worker.ID)
				if err != nil || len(messages) != 1 {
					t.Fatal(messages, err)
				}
				report, err := m.Store.Usage(context.WithoutCancel(ctx), runID)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "success" && mode != "no-policy" && mode != "recovery" {
					if mode != "unknown" && (messages[0].Status == RunStatusSucceeded || report.Summary.Complete) {
						t.Fatal("changed or unknown terminal published", messages, report)
					}
					if err = managedWorkerHold(context.WithoutCancel(ctx), m.Store.db, worker.ID); err == nil {
						t.Fatal("failed original hold erased")
					}
					return
				}
				got := messages[0]
				if got.ID != message.ID || got.Status != RunStatusSucceeded || got.Result.Outputs["text"] == "PRIVATE" || !report.Summary.Complete || f.calls != 1 {
					t.Fatal("original policy terminal not published exactly once", got, report, f.calls)
				}
				if mode == "no-policy" {
					if got.Result.TaskPolicy != nil || got.Result.Outputs["tool_policy"] != nil {
						t.Fatal("untrusted policy annotation published", got)
					}
				} else if got.Result.TaskPolicy == nil || got.Result.TaskPolicy.TaskAttemptID != got.TaskAttemptID || got.Result.Outputs["tool_policy"] == nil {
					t.Fatal("controller policy receipt missing", got)
				}
				if err = managedWorkerHold(ctx, m.Store.db, worker.ID); err != nil {
					t.Fatal("completed original remains held", err)
				}
			})
		}
	}
}
