package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const conversationYAML = `jobs:
  solve:
    outputs:
      answer: ${{ steps.solution.answer }}
    steps:
      - id: solution
        conversation: [builder, reviewer]
        finishBy: reviewer
        prompt: 'Task: ${{ inputs.task }}'
        with:
          output:
            schema:
              type: object
              properties:
                answer: {type: string}
              required: [answer]
              additionalProperties: false
`

type conversationSession struct {
	turn  func(context.Context, SessionRequest, func(ProviderEvent)) (SessionResult, error)
	caps  ProviderCapabilities
	roles map[string]bool
	calls atomic.Int32
}

func newConversationSession(turn func(context.Context, SessionRequest, func(ProviderEvent)) (SessionResult, error)) *conversationSession {
	return &conversationSession{turn: turn, caps: ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, roles: map[string]bool{"builder": true, "reviewer": true}}
}
func (s *conversationSession) HasAgent(role string) bool                         { return s.roles[role] }
func (s *conversationSession) Capabilities(string) (ProviderCapabilities, error) { return s.caps, nil }
func (s *conversationSession) RunAgent(context.Context, AgentRequest) (map[string]any, error) {
	return nil, fmt.Errorf("ordinary RunAgent called for conversation")
}
func (s *conversationSession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	s.calls.Add(1)
	return s.turn(ctx, req, emit)
}
func conversationEvent(req SessionRequest, emit func(ProviderEvent)) SessionResult {
	session := "session-" + req.AgentID
	emit(ProviderEvent{Type: "session", SessionID: session})
	return SessionResult{SessionID: session, Status: RunStatusSucceeded, Outputs: map[string]any{"text": "turn complete"}}
}
func conversationCall(ctx context.Context, req SessionRequest, id, name, args string) error {
	_, err := req.Tools.Binding.Call(ctx, ToolRequest{CallID: id, Server: conversationToolServer, Tool: name, Arguments: json.RawMessage(args)})
	return err
}

func TestYAMLConversationRoutingSessionsAuthorityAndResult(t *testing.T) {
	var order []string
	owners := map[string]string{}
	runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
		result := conversationEvent(req, emit)
		if req.SessionOwnerID == "" {
			return result, errors.New("missing stable session state owner")
		}
		if previous := owners[req.AgentID]; previous != "" && previous != req.SessionOwnerID {
			return result, errors.New("participant state owner changed")
		}
		owners[req.AgentID] = req.SessionOwnerID
		order = append(order, req.AgentID)
		if !strings.Contains(req.Prompt, "${{ secrets.unavailable }}") {
			t.Errorf("message data was evaluated as expressions: %s", req.Prompt)
		}
		if len(order) > 2 && req.SessionID != "session-"+req.AgentID {
			t.Errorf("session not resumed: %+v", req)
		}
		if req.Tools == nil || len(req.Tools.Binding.Catalog()) != 2 {
			return result, errors.New("missing managed messaging tools")
		}
		switch len(order) {
		case 1:
			if err := conversationCall(ctx, req, "unauthorized", "finish", `{"answer":"wrong"}`); err == nil {
				return result, errors.New("non-finisher completed")
			}
			if err := conversationCall(ctx, req, "spoof", "send", `{"to":"reviewer","message":"review","from":"reviewer"}`); err == nil {
				return result, errors.New("sender spoof accepted")
			}
			if err := conversationCall(ctx, req, "first", "send", `{"to":"reviewer","message":"review"}`); err != nil {
				return result, err
			}
			if err := conversationCall(ctx, req, "first", "send", `{"to":"reviewer","message":"review"}`); err != nil {
				return result, err
			}
		case 2:
			for _, to := range []string{"reviewer", "outsider"} {
				if err := conversationCall(ctx, req, "invalid-"+to, "send", fmt.Sprintf(`{"to":%q,"message":"bad"}`, to)); err == nil {
					return result, fmt.Errorf("invalid peer accepted: %s", to)
				}
			}
			if err := conversationCall(ctx, req, "changes", "send", `{"to":"builder","message":"correct it"}`); err != nil {
				return result, err
			}
		case 3:
			if err := conversationCall(ctx, req, "updated", "send", `{"to":"reviewer","message":"fixed"}`); err != nil {
				return result, err
			}
		case 4:
			if err := conversationCall(ctx, req, "done", "finish", `{"answer":"approved"}`); err != nil {
				return result, err
			}
		default:
			return result, errors.New("extra participant turn")
		}
		return result, nil
	})
	e, store := durableExecutor(t, conversationYAML, runner)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "${{ secrets.unavailable }}"}})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusSucceeded || run.Jobs["solve"].Outputs["answer"] != "approved" {
		t.Fatalf("result unavailable: %+v", run)
	}
	if !slices.Equal(order, []string{"builder", "reviewer", "builder", "reviewer"}) {
		t.Fatal(order)
	}
	attempts, err := store.Attempts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 5 {
		t.Fatalf("expected parent and four durable turns: %d", len(attempts))
	}
	state := attempts[0].Conversation
	if state == nil || state.Messages != 3 || state.Calls != 4 || state.Turns != 4 || !state.Finished || len(state.Receipts) != 4 || len(state.Transcript) != 3 {
		t.Fatalf("wrong durable conversation: %+v", state)
	}
	if state.Transcript[0].From != "builder" || state.Transcript[0].To != "reviewer" {
		t.Fatal(state.Transcript)
	}
}

func TestYAMLConversationInvalidOutputIsTerminalWithoutRepair(t *testing.T) {
	runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
		result := conversationEvent(req, emit)
		if req.AgentID == "builder" {
			return result, conversationCall(ctx, req, "next", "send", `{"to":"reviewer","message":"review"}`)
		}
		// A provider swallowing the tool error cannot turn validation into success.
		if err := conversationCall(ctx, req, "invalid", "finish", `{"answer":42}`); err == nil {
			return result, errors.New("invalid output accepted")
		}
		return result, nil
	})
	e, _ := durableExecutor(t, conversationYAML, runner)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}})
	if err == nil || !strings.Contains(err.Error(), "invalid structured output") || run.Status != RunStatusFailed || runner.calls.Load() != 2 {
		t.Fatalf("invalid completion: run=%+v calls=%d err=%v", run, runner.calls.Load(), err)
	}
}

func TestYAMLConversationRejectedArgumentsCannotBeRepaired(t *testing.T) {
	for _, arguments := range []string{`{"answer":`, `[]`, `null`, `{"answer":42,"padding":"` + strings.Repeat("x", 1<<20) + `"}`} {
		name := "malformed"
		if len(arguments) > 1<<20 {
			name = "oversized"
		} else if json.Valid([]byte(arguments)) {
			name = arguments
		}
		t.Run(name, func(t *testing.T) {
			runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
				result := conversationEvent(req, emit)
				if req.AgentID == "builder" {
					return result, conversationCall(ctx, req, "next", "send", `{"to":"reviewer","message":"review"}`)
				}
				if err := conversationCall(ctx, req, "invalid", "finish", arguments); err == nil {
					return result, errors.New("invalid completion accepted")
				}
				if err := conversationCall(ctx, req, "repair", "finish", `{"answer":"approved"}`); err == nil {
					return result, errors.New("invalid completion repaired into success")
				}
				return result, nil
			})
			e, store := durableExecutor(t, conversationYAML, runner)
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}})
			if err == nil || !strings.Contains(err.Error(), "invalid structured output") || run.Status != RunStatusFailed || runner.calls.Load() != 2 {
				t.Fatalf("rejected completion: run=%+v calls=%d err=%v", run, runner.calls.Load(), err)
			}
			attempts, err := store.Attempts(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if attempts[0].Conversation.Failure == "" || attempts[0].Conversation.Finished {
				t.Fatalf("terminal failure not retained: %+v", attempts[0].Conversation)
			}
		})
	}
}

func TestYAMLConversationBudgetsAndStall(t *testing.T) {
	for _, tc := range []struct {
		name, fields, want string
		messages           int
		stall              bool
	}{
		{"turns", "        maxTurns: 2\n", "turn budget", 1, false},
		{"messages", "        maxMessages: 1\n", "message budget", 2, false},
		{"stall", "", "stalled", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.Replace(conversationYAML, "        finishBy: reviewer\n", "        finishBy: reviewer\n"+tc.fields, 1)
			runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
				result := conversationEvent(req, emit)
				to := "reviewer"
				if req.AgentID == "reviewer" {
					to = "builder"
				}
				for i := 0; i < tc.messages; i++ {
					if err := conversationCall(ctx, req, fmt.Sprint(i), "send", fmt.Sprintf(`{"to":%q,"message":"continue"}`, to)); err != nil {
						return result, err
					}
				}
				return result, nil
			})
			e, _ := durableExecutor(t, source, runner)
			_, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing %s: %v", tc.want, err)
			}
			if tc.name == "turns" && runner.calls.Load() != 2 {
				t.Fatal("call budget exceeded")
			}
			if tc.name == "messages" && runner.calls.Load() != 1 {
				t.Fatal("message exhaustion admitted another participant")
			}
		})
	}
}

func TestYAMLConversationCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			source := conversationYAML
			if timeout {
				// Exercise timeout inside a started turn, allowing durable admission
				// to finish first even on instrumented CI runners.
				source = strings.Replace(source, "        finishBy: reviewer", "        timeout: 2s\n        finishBy: reviewer", 1)
			}
			runner := newConversationSession(func(tc context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
				result := conversationEvent(req, emit)
				if !timeout {
					cancel()
				}
				<-tc.Done()
				return result, tc.Err()
			})
			e, _ := durableExecutor(t, source, runner)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}})
			want := context.Canceled
			if timeout {
				want = context.DeadlineExceeded
			}
			wantStatus := RunStatusCanceled
			if timeout {
				wantStatus = RunStatusFailed
			}
			matches := errors.Is(err, want)
			if timeout {
				matches = err != nil && strings.Contains(err.Error(), want.Error()) && run.Jobs["solve"].Steps["solution"].FailureClass == "timeout"
			}
			if !matches || run.Status != wantStatus || runner.calls.Load() != 1 {
				t.Fatalf("cancellation failed: %+v %v", run, err)
			}
		})
	}
}

func TestYAMLConversationIntegrityAndSessionIdentity(t *testing.T) {
	for _, sessionChange := range []bool{false, true} {
		t.Run(fmt.Sprint(sessionChange), func(t *testing.T) {
			runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
				result := conversationEvent(req, emit)
				if sessionChange {
					emit(ProviderEvent{Type: "session", SessionID: "different"})
					return result, nil
				}
				if err := conversationCall(ctx, req, "same", "send", `{"to":"reviewer","message":"one"}`); err != nil {
					return result, err
				}
				if err := conversationCall(ctx, req, "same", "send", `{"to":"reviewer","message":"two"}`); err == nil {
					return result, errors.New("conflicting tool identity accepted")
				}
				return result, nil
			})
			e, _ := durableExecutor(t, conversationYAML, runner)
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}})
			if !errors.Is(err, ErrActionIntegrity) || run.Status != RunStatusNeedsAttention || runner.calls.Load() != 1 {
				t.Fatalf("integrity failure lost: %+v %v", run, err)
			}
		})
	}
}

func TestYAMLConversationAdmissionBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Executor, *conversationSession)
	}{
		{"legacy-runner", func(e *Executor, s *conversationSession) {
			e.Agents = agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				t.Error("effect before admission")
				return nil, errors.New("effect before admission")
			})
		}},
		{"no-journal", func(e *Executor, s *conversationSession) { e.Store = nil }},
		{"missing-role", func(e *Executor, s *conversationSession) { delete(s.roles, "reviewer") }},
		{"no-resume", func(e *Executor, s *conversationSession) { s.caps.Resume = false }},
		{"no-managed-policy", func(e *Executor, s *conversationSession) { s.caps.ManagedPolicy = false }},
		{"no-sessions", func(e *Executor, s *conversationSession) { s.caps.Sessions = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newConversationSession(func(context.Context, SessionRequest, func(ProviderEvent)) (SessionResult, error) {
				t.Error("effect before admission")
				return SessionResult{}, errors.New("effect before admission")
			})
			e, _ := durableExecutor(t, conversationYAML, runner)
			tc.change(e, runner)
			if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil {
				t.Fatal("unsupported conversation admitted")
			}
		})
	}
}

func TestYAMLConversationValidation(t *testing.T) {
	for _, tc := range []struct{ old, new string }{
		{"[builder, reviewer]", "[builder, builder]"},
		{"[builder, reviewer]", "[builder]"},
		{"[builder, reviewer]", "[]"},
		{"[builder, reviewer]", "[builder, null]"},
		{"finishBy: reviewer", "finishBy: absent"},
		{"finishBy: reviewer", "finishBy: null"},
		{"finishBy: reviewer", "finishBy: reviewer\n        start: absent"},
		{"finishBy: reviewer", "finishBy: reviewer\n        maxTurns: 0"},
		{"finishBy: reviewer", "finishBy: reviewer\n        maxTurns: 101"},
		{"finishBy: reviewer", "finishBy: reviewer\n        maxMessages: 257"},
		{"finishBy: reviewer", "finishBy: reviewer\n        maxMessages: null"},
		{"finishBy: reviewer", "finishBy: reviewer\n        timeout: 25h"},
		{"finishBy: reviewer", "finishBy: reviewer\n        uses: ''"},
		{"type: object", "type: array"},
	} {
		t.Run(tc.new, func(t *testing.T) {
			source := strings.Replace(conversationYAML, tc.old, tc.new, 1)
			w, err := Parse([]byte(source))
			if err == nil {
				err = Validate(w)
			}
			if err == nil {
				t.Fatal("invalid conversation accepted")
			}
		})
	}
}

// Lose the write after a completed turn and the enclosing completion. The last
// durable state still owns that active turn, whose child receipt is complete.
type conversationFaultStore struct {
	*SQLiteStore
	loseCheckpoint bool
	loseCompletion bool
}

func (s *conversationFaultStore) RecordAttempt(ctx context.Context, a Attempt) error {
	if s.loseCheckpoint && a.Conversation != nil && a.Conversation.Active == nil && a.Conversation.Turns > 0 {
		s.loseCheckpoint = false
		return errors.New("injected conversation checkpoint storage failure")
	}
	return s.SQLiteStore.RecordAttempt(ctx, a)
}
func (s *conversationFaultStore) Complete(ctx context.Context, a Attempt, r StepExecution) error {
	if s.loseCompletion && a.Conversation != nil && a.Conversation.Active == nil {
		s.loseCompletion = false
		return errors.New("injected parent completion storage failure")
	}
	return s.SQLiteStore.Complete(ctx, a, r)
}

func TestYAMLConversationResumeReusesCompletedTurnAndBudget(t *testing.T) {
	for _, maxTurns := range []int{1, 8} {
		t.Run(fmt.Sprint(maxTurns), func(t *testing.T) {
			runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
				result := conversationEvent(req, emit)
				if req.AgentID == "builder" {
					return result, conversationCall(ctx, req, "next", "send", `{"to":"reviewer","message":"review"}`)
				}
				return result, conversationCall(ctx, req, "done", "finish", `{"answer":"approved"}`)
			})
			source := strings.Replace(conversationYAML, "finishBy: reviewer", fmt.Sprintf("finishBy: reviewer\n        maxTurns: %d", maxTurns), 1)
			e, store := durableExecutor(t, source, runner)
			e.Store = &conversationFaultStore{SQLiteStore: store, loseCheckpoint: true, loseCompletion: true}
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}})
			if err == nil || runner.calls.Load() != 1 {
				t.Fatalf("fault not reached: calls=%d %v", runner.calls.Load(), err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLite(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			e.Store = reopened
			resumed, err := e.Resume(context.Background(), run.ID, nil, false)
			if maxTurns == 1 {
				if err == nil || !strings.Contains(err.Error(), "turn budget") || runner.calls.Load() != 1 {
					t.Fatalf("resume reset budget: %+v calls=%d err=%v", resumed, runner.calls.Load(), err)
				}
			} else {
				if err != nil || resumed.Status != RunStatusSucceeded || runner.calls.Load() != 2 {
					t.Fatalf("completed turn replayed: %+v calls=%d err=%v", resumed, runner.calls.Load(), err)
				}
			}
		})
	}
}

func TestYAMLConversationResumeDoesNotResendUncertainTurn(t *testing.T) {
	runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
		result := conversationEvent(req, emit)
		if err := conversationCall(ctx, req, "sent", "send", `{"to":"reviewer","message":"review"}`); err != nil {
			return result, err
		}
		return result, errors.New("lost provider outcome")
	})
	e, _ := durableExecutor(t, conversationYAML, runner)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}})
	if err == nil {
		t.Fatal("missing uncertain outcome")
	}
	for _, retry := range []bool{false, true} {
		if _, err = e.Resume(context.Background(), run.ID, nil, retry); err == nil {
			t.Fatal("uncertain participant turn automatically resent")
		}
		if runner.calls.Load() != 1 {
			t.Fatalf("uncertain call repeated: %d", runner.calls.Load())
		}
	}
}

func TestYAMLConversationLostCompletionReconstructsWithoutProvider(t *testing.T) {
	source := strings.Replace(conversationYAML, "finishBy: reviewer", "finishBy: builder", 1)
	runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
		result := conversationEvent(req, emit)
		return result, conversationCall(ctx, req, "done", "finish", `{"answer":"approved"}`)
	})
	e, store := durableExecutor(t, source, runner)
	e.Store = &conversationFaultStore{SQLiteStore: store, loseCheckpoint: true, loseCompletion: true}
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}})
	if err == nil {
		t.Fatal("missing injected failure")
	}
	// Recovery must consume the completed turn even after elapsed time expires.
	attempts, readErr := store.Attempts(context.Background(), run.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	parent := attempts[0]
	parent.Conversation.Deadline = time.Now().Add(-time.Hour)
	if err := store.RecordAttempt(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	e.Store = store
	resumed, err := e.Resume(context.Background(), run.ID, nil, false)
	if err != nil || resumed.Status != RunStatusSucceeded || runner.calls.Load() != 1 {
		t.Fatalf("lost completion repeated provider: %+v calls=%d err=%v", resumed, runner.calls.Load(), err)
	}
}

func TestYAMLConversationNestedCapacityOne(t *testing.T) {
	var active atomic.Int32
	runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
		if active.Add(1) != 1 {
			t.Error("conversation exceeded shared capacity")
		}
		defer active.Add(-1)
		result := conversationEvent(req, emit)
		return result, conversationCall(ctx, req, "finish", "finish", `{"text":"done"}`)
	})
	source := `jobs:
  solve:
    steps:
      - parallel:
          - id: first
            conversation: [builder, reviewer]
            finishBy: builder
            prompt: first
          - sequence:
              - id: second
                conversation: [builder, reviewer]
                finishBy: builder
                prompt: second
`
	e, _ := durableExecutor(t, source, runner)
	e.Slots = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != RunStatusSucceeded || runner.calls.Load() != 2 {
		t.Fatalf("nested conversations failed: %+v %v", run, err)
	}
}

func TestYAMLConversationRedactsDataPreservingProtocolIdentities(t *testing.T) {
	runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
		result := conversationEvent(req, emit)
		if req.AgentID == "builder" {
			return result, conversationCall(ctx, req, "session-send", "send", `{"to":"reviewer","message":"credential session- must not persist"}`)
		}
		return result, conversationCall(ctx, req, "session-finish", "finish", `{"answer":"session-"}`)
	})
	e, store := durableExecutor(t, conversationYAML, runner)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "task"}, Secrets: map[string]string{"credential": "session-"}})
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := store.Attempts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := attempts[0].Conversation
	if state.Sessions["builder"] != "session-builder" || state.Sessions["reviewer"] != "session-reviewer" {
		t.Fatalf("session identity changed by credential redaction: %+v", state.Sessions)
	}
	if state.Transcript[0].Message != "credential [REDACTED] must not persist" {
		t.Fatalf("unredacted transcript: %+v", state.Transcript)
	}
	if state.Transcript[0].ID != "solution/conversation/0/session-send" {
		t.Fatal("delivery identity changed")
	}
	if _, ok := state.Receipts["solution/conversation/0/session-send"]; !ok {
		t.Fatal("receipt identity changed")
	}
	if state.Result.(map[string]any)["answer"] != "[REDACTED]" {
		t.Fatalf("unredacted completion: %+v", state.Result)
	}
}

func TestYAMLConversationJSONFormatHasObjectToolSchema(t *testing.T) {
	for _, output := range []string{"json", "{format: json}"} {
		t.Run(output, func(t *testing.T) {
			runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
				result := conversationEvent(req, emit)
				catalog := req.Tools.Binding.Catalog()
				var schema map[string]any
				if err := json.Unmarshal(catalog[1].InputSchema, &schema); err != nil || schema["type"] != "object" {
					return result, fmt.Errorf("invalid MCP object schema: %s %v", catalog[1].InputSchema, err)
				}
				return result, conversationCall(ctx, req, "done", "finish", `{"arbitrary":"object"}`)
			})
			source := "jobs: {solve: {steps: [{conversation: [builder, reviewer], finishBy: builder, prompt: task, with: {output: " + output + "}}]}}"
			e, _ := durableExecutor(t, source, runner)
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err != nil || run.Status != RunStatusSucceeded {
				t.Fatalf("JSON contract failed: %+v %v", run, err)
			}
		})
	}
}

type conversationItemSession struct {
	*conversationSession
	after atomic.Int32
}

func (s *conversationItemSession) RunAgent(context.Context, AgentRequest) (map[string]any, error) {
	if s.after.Add(1) == 1 {
		return nil, errors.New("later item step failed")
	}
	return map[string]any{"text": "recovered"}, nil
}

func TestYAMLConversationOrderedItemRetryRetainsReceiptsAndBounds(t *testing.T) {
	for _, mode := range []string{"uncertain", "completed", "checkpoint", "budget", "messages", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			var builderOwner string
			runner := &conversationItemSession{conversationSession: newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
				result := conversationEvent(req, emit)
				if req.AgentID == "builder" {
					if mode == "checkpoint" && builderOwner != "" {
						if req.SessionOwnerID != builderOwner || req.SessionID != "session-builder" {
							return result, errors.New("physical retry replaced participant session")
						}
						return result, conversationCall(ctx, req, "done", "finish", `{"answer":"approved"}`)
					}
					builderOwner = req.SessionOwnerID
					if err := conversationCall(ctx, req, "sent", "send", `{"to":"reviewer","message":"review"}`); err != nil {
						return result, err
					}
					if mode == "uncertain" {
						return result, errors.New("lost provider outcome")
					}
					return result, nil
				}
				if mode == "checkpoint" || mode == "messages" {
					return result, conversationCall(ctx, req, "next", "send", `{"to":"builder","message":"approved"}`)
				}
				return result, conversationCall(ctx, req, "done", "finish", `{"answer":"approved"}`)
			})}
			e, store := iterationExecutor(t, runner)
			child := "on:\n  workflow_call:\n    inputs:\n      task: {type: string}\n      ordinal: {type: number}\n      identity: {type: string}\n" + conversationYAML
			if mode == "completed" {
				child += "      - id: after\n        uses: agent/reviewer\n        with: {prompt: after}\n"
			}
			if mode == "budget" {
				child = strings.Replace(child, "finishBy: reviewer", "finishBy: reviewer\n        maxTurns: 1", 1)
			}
			if mode == "messages" {
				child = strings.Replace(child, "finishBy: reviewer", "finishBy: reviewer\n        maxMessages: 1", 1)
			}
			if mode == "checkpoint" {
				child = strings.Replace(child, "finishBy: reviewer", "finishBy: builder", 1)
			}
			writeWorkflow(t, e.WorkspaceDir, "child.yml", child)
			if mode == "checkpoint" || mode == "budget" || mode == "messages" || mode == "deadline" {
				e.Store = &conversationFaultStore{SQLiteStore: store, loseCheckpoint: true, loseCompletion: true}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			run, err := e.Run(ctx, iterationRequest("task"))
			if err == nil {
				t.Fatal("missing initial item failure")
			}
			before := runner.calls.Load()
			firstChild := run.Jobs["review"].Items[0].ChildID
			attempts, err := store.Attempts(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var state *ConversationState
			for _, attempt := range attempts {
				if attempt.Conversation != nil {
					state = attempt.Conversation
					if mode == "deadline" {
						state.Deadline = time.Now().Add(-time.Hour)
						if err := store.RecordAttempt(ctx, attempt); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if state == nil || state.Calls != int(before) || state.Messages != 1 {
				t.Fatalf("missing retained conversation: %+v calls=%d", state, before)
			}
			e.Store = store
			resumed, err := e.Resume(ctx, run.ID, nil, true)
			items := resumed.Jobs["review"].Items
			if len(items) != 1 || items[0].ChildID == firstChild {
				t.Fatalf("fresh physical retry not reached: %+v %v", resumed, err)
			}
			switch mode {
			case "completed":
				if err != nil || resumed.Status != RunStatusSucceeded || runner.calls.Load() != before || runner.after.Load() != 2 {
					t.Fatalf("completed conversation repeated: calls=%d after=%d run=%+v err=%v", runner.calls.Load(), runner.after.Load(), resumed, err)
				}
			case "checkpoint":
				if err != nil || resumed.Status != RunStatusSucceeded || runner.calls.Load() != before+2 {
					t.Fatalf("completed turn not reconstructed: calls=%d run=%+v err=%v", runner.calls.Load(), resumed, err)
				}
			case "messages":
				if err == nil || !strings.Contains(err.Error(), "message budget") || runner.calls.Load() != before+1 {
					t.Fatalf("retry reset message budget: calls=%d run=%+v err=%v", runner.calls.Load(), resumed, err)
				}
			default:
				want := "incomplete item effect"
				if mode == "budget" {
					want = "turn budget"
				} else if mode == "deadline" {
					want = "deadline exceeded"
				}
				if err == nil || !strings.Contains(err.Error(), want) || runner.calls.Load() != before {
					t.Fatalf("retry reset retained boundary: calls=%d want=%s run=%+v err=%v", runner.calls.Load(), want, resumed, err)
				}
			}
		})
	}
}
