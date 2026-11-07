package workflows

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestConversationLifecycleCombinationsRejectBeforeEffects(t *testing.T) {
	conversation := "conversation: [builder, reviewer], finishBy: builder, prompt: Work"
	for _, declaration := range []string{
		conversation + ", onError: [{argv: [echo, forbidden]}]",
		conversation + ", onFailure: [{argv: [echo, forbidden]}]",
		conversation + ", finally: [{argv: [echo, forbidden]}]",
		conversation + ", finally: []",
		conversation + ", handlerTimeout: 1s",
		conversation + `, handlerTimeout: ""`,
		conversation + ", handlerTimeout: null",
		conversation + ", onError: null",
		conversation + ", onFailure: null",
		conversation + ", finally: null",
		"argv: [echo, task], onError: [{" + conversation + "}]",
		"argv: [echo, task], onFailure: [{" + conversation + "}]",
		"argv: [echo, task], finally: [{" + conversation + "}]",
	} {
		t.Run(declaration, func(t *testing.T) {
			runner := newConversationSession(func(context.Context, SessionRequest, func(ProviderEvent)) (SessionResult, error) {
				return SessionResult{}, fmt.Errorf("unexpected participant call")
			})
			source := fmt.Sprintf("jobs: {solve: {steps: [{argv: [echo, effect]}, {%s}]}}", declaration)
			e, _ := durableExecutor(t, source, runner)
			ready := false
			e.Ready = func(context.Context) error { ready = true; return nil }
			if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), "conversations cannot own or appear inside lifecycle handlers") || ready || runner.calls.Load() != 0 {
				t.Fatalf("unsupported combination reached effects: ready=%v calls=%d err=%v", ready, runner.calls.Load(), err)
			}
		})
	}
}

func TestConversationBesideLifecycleRecoversLostBlockCompletion(t *testing.T) {
	const source = `schemas:
  Decision:
    type: object
    properties:
      answer: {type: string, enum: ['${{ steps.recovered.outputs.stdout }}']}
    required: [answer]
jobs:
  solve:
    steps:
      - id: joined
        sequence:
          - id: recovered
            argv: [sh, -c, exit 1]
            onError:
              - {id: fallback, argv: [printf, seed]}
            finally:
              - {argv: [printf, finalized]}
          - id: solution
            conversation: [builder, reviewer]
            finishBy: builder
            prompt: '${{ steps.recovered.outputs.stdout }}'
            with: {output: {schema: Decision}}
      - id: after
        argv: [printf, '${{ steps.solution.answer }}']
        finally:
          - {argv: [printf, '${{ steps.solution.answer }}']}
`
	runner := newConversationSession(func(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
		result := conversationEvent(req, emit)
		if !strings.Contains(req.Prompt, "seed") {
			return result, fmt.Errorf("recovered task output missing from conversation prompt: %s", req.Prompt)
		}
		return result, conversationCall(ctx, req, "complete", "finish", `{"answer":"seed"}`)
	})
	e, store := durableExecutor(t, source, runner)
	e.Slots = make(chan struct{}, 1)
	e.Store = &blockCompletionFault{SQLiteStore: store, failID: "joined"}
	// This checks recovery, not elapsed time. Allow journal writes and child
	// processes to finish under race instrumentation on slower CI runners.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run == nil || run.FailureClass != "storage" || runner.calls.Load() != 1 {
		t.Fatalf("lost block completion: run=%+v calls=%d err=%v", run, runner.calls.Load(), err)
	}
	e.Store = store
	run, err = e.Resume(ctx, run.ID, nil, false)
	if err != nil || run == nil || run.Status != RunStatusSucceeded || runner.calls.Load() != 1 {
		t.Fatalf("combined recovery replayed work: run=%+v calls=%d err=%v", run, runner.calls.Load(), err)
	}
	steps := run.Jobs["solve"].Steps
	finalization := steps["after"].Lifecycle
	recovered := steps["recovered"]
	if steps["after"].Outputs["stdout"] != "seed" || finalization == nil || len(finalization.Handlers["finally"]) != 1 || finalization.Handlers["finally"][0].Outputs["stdout"] != "seed" || recovered.Status != RunStatusSucceeded || recovered.Lifecycle == nil || recovered.Lifecycle.OriginalError == "" {
		t.Fatalf("conversation result or lifecycle evidence lost: %+v", steps)
	}
	attempts, err := store.Attempts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fallbacks := 0
	for _, attempt := range attempts {
		if attempt.StepID == "recovered/onError/fallback" {
			fallbacks++
		}
	}
	if fallbacks != 1 {
		t.Fatalf("fallback receipt replayed: %d", fallbacks)
	}
}
