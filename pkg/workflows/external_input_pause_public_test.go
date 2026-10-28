package workflows_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type publicPauseAgent struct{ calls atomic.Int32 }

func (a *publicPauseAgent) RunAgent(context.Context, workflows.AgentRequest) (map[string]any, error) {
	a.calls.Add(1)
	return map[string]any{}, nil
}

// This implementation can use only exported types and methods. Static
// embedding intentionally hides unrelated SQLite extensions from Executor.
type publicPauseJournal struct {
	workflows.ExternalInputJournal
	sqlite   *workflows.SQLiteStore
	runID    string
	denial   error
	canceled bool
	healthy  bool
}

func (j *publicPauseJournal) ConsumeInput(ctx context.Context, id string) (workflows.ExternalInputGate, error) {
	if j.denial == nil {
		return j.ExternalInputJournal.ConsumeInput(ctx, id)
	}
	j.healthy = ctx.Err() == nil
	run, _, err := j.Load(ctx, j.runID)
	if err != nil {
		return workflows.ExternalInputGate{}, err
	}
	gates, err := j.Inputs(ctx, j.runID)
	if err != nil {
		return workflows.ExternalInputGate{}, err
	}
	if j.canceled {
		err = j.sqlite.RequestCancelGeneration(ctx, run.ID, run.Generation)
	} else {
		err = j.sqlite.InterruptInput(ctx, run.ID, run.Generation)
	}
	if err != nil {
		return workflows.ExternalInputGate{}, err
	}
	for _, g := range gates {
		if g.ID == id {
			return g, fmt.Errorf("host journal denies consumption: %w", j.denial)
		}
	}
	return workflows.ExternalInputGate{}, errors.New("fixture gate not found")
}

var _ workflows.ExternalInputJournal = (*publicPauseJournal)(nil)

func TestExternalInputPublicPauseDenialContract(t *testing.T) {
	testExternalInputPublicDenialContract(t, false)
}
func TestExternalInputPublicCancelDenialContract(t *testing.T) {
	testExternalInputPublicDenialContract(t, true)
}

func testExternalInputPublicDenialContract(t *testing.T, canceled bool) {
	for _, kind := range []string{"pure", "wrapped_pure_join", "storage", "provider", "cleanup", "integrity", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			source := `jobs: {x: {steps: [{id: before, uses: agent/a, with: {prompt: before}}, {id: q, uses: input/wait, with: {prompt: q, evidence-id: e}}, {id: after, uses: agent/a, with: {prompt: after}}]}}`
			if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			sqlite, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
			if err != nil {
				t.Fatal(err)
			}
			defer sqlite.Close()
			marker := workflows.ErrExternalInputPaused
			if canceled {
				marker = workflows.ErrExternalInputCanceled
			}
			denial := error(marker)
			var genuine error
			switch kind {
			case "wrapped_pure_join":
				denial = fmt.Errorf("outer control: %w", errors.Join(marker, fmt.Errorf("inner control: %w", marker)))
			case "storage":
				genuine = errors.New("fixture database write failed")
			case "provider":
				genuine = fmt.Errorf("fixture provider failed: %w", workflows.ErrProviderFailure)
			case "cleanup":
				genuine = fmt.Errorf("fixture cleanup failed: %w", process.ErrCleanup)
			case "integrity":
				genuine = fmt.Errorf("fixture integrity failed: %w", workflows.ErrActionIntegrity)
			case "conflict":
				genuine = fmt.Errorf("fixture genuine conflict: %w", workflows.ErrExternalInput)
			}
			if genuine != nil {
				denial = errors.Join(denial, genuine)
			}
			if !errors.Is(marker, workflows.ErrExternalInput) {
				t.Fatal("public control marker lost conflict compatibility")
			}
			journal := &publicPauseJournal{ExternalInputJournal: sqlite, sqlite: sqlite, denial: denial, canceled: canceled}
			agent := &publicPauseAgent{}
			executor := &workflows.Executor{WorkspaceDir: root, Store: journal, Agents: agent}
			run, err := executor.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
			if err != nil || run.Status != workflows.RunStatusWaiting || agent.calls.Load() != 1 {
				t.Fatal(run, err)
			}
			journal.runID = run.ID
			gates, err := journal.Inputs(ctx, run.ID)
			if err != nil || len(gates) != 1 {
				t.Fatal(gates, err)
			}
			accepted, err := journal.AcceptInput(ctx, run.ID, gates[0].ID, workflows.ExternalInputAnswer{AnswerID: "public-answer", EvidenceID: "e", Value: json.RawMessage(`true`)})
			if err != nil {
				t.Fatal(err)
			}
			paused, resumeErr := executor.Resume(ctx, run.ID, nil, false)
			if paused == nil || !journal.healthy || ctx.Err() != nil || agent.calls.Load() != 1 {
				t.Fatal("pause was not denied under a healthy context", paused, resumeErr)
			}
			saved, _, err := journal.Load(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			control, err := journal.InputControl(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			retained, err := journal.Inputs(ctx, run.ID)
			if err != nil || len(retained) != 1 || saved.Status != paused.Status || saved.Error != paused.Error {
				t.Fatal("control denial lost coherent outcome", retained, saved, err)
			}
			if canceled {
				g := retained[0]
				if !control.Canceled || control.Paused || g.ID != accepted.ID || g.State != "canceled" || !reflect.DeepEqual(g.Answer, accepted.Answer) || len(g.History) != 3 || !reflect.DeepEqual(g.History[:2], accepted.History) || g.History[2].State != "canceled" {
					t.Fatal("cancel denial consumed answer or lost invalidation/history", g, control)
				}
				journal.denial = nil
				if _, err = executor.Resume(ctx, run.ID, nil, true); err == nil || !strings.Contains(err.Error(), "canceled input gates") {
					t.Fatal("canceled accepted answer revived", err)
				}
				after, _, err := journal.Load(ctx, run.ID)
				afterGates, gateErr := journal.Inputs(ctx, run.ID)
				if err != nil || gateErr != nil || after.Generation != saved.Generation || !reflect.DeepEqual(afterGates, retained) || agent.calls.Load() != 1 {
					t.Fatal("rejected canceled continuation changed state/effects", after, afterGates, err, gateErr)
				}
			} else if !reflect.DeepEqual(retained[0], accepted) || !control.Paused || control.Canceled {
				t.Fatal("pause denial lost accepted history/control", retained, control)
			}
			if genuine != nil {
				if resumeErr == nil || !strings.Contains(resumeErr.Error(), genuine.Error()) || paused.Status == workflows.RunStatusInterrupted || paused.Status == workflows.RunStatusSucceeded || !strings.Contains(paused.Error, genuine.Error()) {
					t.Fatal("pause sentinel suppressed a genuine joined failure", paused, resumeErr, genuine)
				}
				return
			}
			if canceled {
				if resumeErr != nil || paused.Status != workflows.RunStatusCanceled || paused.FinishedAt.IsZero() {
					t.Fatal("pure public cancel denial did not honor durable cancellation", paused, resumeErr)
				}
				return
			}
			if resumeErr != nil || paused.Status != workflows.RunStatusInterrupted || paused.Error != "" || paused.FailureClass != "" || !paused.FinishedAt.IsZero() {
				t.Fatal("public pure pause denial became failure", paused, resumeErr)
			}
			journal.denial = nil
			done, err := executor.Resume(ctx, run.ID, nil, false)
			if err != nil || done.Status != workflows.RunStatusSucceeded || agent.calls.Load() != 2 {
				t.Fatal("explicit continuation replayed preparation or lost answer", done, err, agent.calls.Load())
			}
			final, err := journal.Inputs(ctx, run.ID)
			if err != nil || final[0].ID != accepted.ID || final[0].State != "consumed" || len(final[0].History) != 3 {
				t.Fatal("public pause continuation changed gate history", final, err)
			}
		})
	}
}
