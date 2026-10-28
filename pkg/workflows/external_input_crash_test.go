package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func crashCheckpointSpec(t *testing.T, root string) ActionSpec[actionInput, actionOutput] {
	t.Helper()
	var calls atomic.Int32
	spec := readSpec(&calls)
	handler := spec.Handler
	spec.Handler = func(ctx context.Context, ac ActionContext, input actionInput) (ActionResult[actionOutput], error) {
		file, err := os.OpenFile(filepath.Join(root, "effects.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return ActionResult[actionOutput]{}, err
		}
		_, err = file.WriteString(input.Value + "\n")
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return ActionResult[actionOutput]{}, errors.Join(err, closeErr)
		}
		return handler(ctx, ac, input)
	}
	return spec
}

func TestExternalInputGateCrashProcess(t *testing.T) {
	root := os.Getenv("WORKFLOW_GATE_CRASH_ROOT")
	if root == "" {
		t.Skip("subprocess fixture")
	}
	s, err := OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{WorkspaceDir: root, Store: s}
	registerTestAction(t, e, crashCheckpointSpec(t, root))
	e.Store = &executionBoundaryJournal{SQLiteStore: s, afterGate: func(context.Context, ExternalInputGate) error { os.Exit(73); return nil }}
	_, err = e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	t.Fatalf("gate crash boundary not reached: %v", err)
}

func TestExternalInputGateCrashSameChild(t *testing.T) {
	for _, guard := range []string{"none", "missing", "step", "evidence", "payload", "uncertain"} {
		for _, composition := range []string{"ordered", "ordered/reusable", "ordered/ordered", "ordered/repeat"} {
			for _, checkpoint := range []bool{false, true} {
				for _, retry := range []bool{false, true} {
					if guard != "none" && (composition != "ordered" || checkpoint || retry) {
						continue
					}
					t.Run(fmt.Sprint(guard, "/", composition, "/checkpoint=", checkpoint, "/retry=", retry), func(t *testing.T) {
						root := t.TempDir()
						nodes := strings.Split(composition, "/")
						for index, node := range nodes {
							name := fmt.Sprint("layer", index, ".yml")
							prefix := "on: {workflow_call: {}}\n"
							if index == 0 {
								name = "main.yml"
								prefix = ""
							}
							options := ""
							if node == "ordered" {
								options = ", for-each: {items: [{id: a}], key: item.id}"
							}
							if node == "repeat" {
								options = ", repeat: {max-attempts: 2, until: 'iteration.number == 2'}"
							}
							writeWorkflow(t, root, name, prefix+fmt.Sprintf("jobs: {review: {uses: workflows/layer%d.yml%s}}\n", index+1, options))
						}
						source := "on: {workflow_call: {}}\njobs:\n  question:\n    steps:\n"
						if checkpoint {
							source += "      - {id: before, uses: action/capture, with: {value: before}}\n"
						}
						source += "      - {uses: input/wait, with: {prompt: question, evidence-id: pinned}}\n"
						writeWorkflow(t, root, fmt.Sprint("layer", len(nodes), ".yml"), source)
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExternalInputGateCrashProcess$")
						command.Env = append(os.Environ(), "WORKFLOW_GATE_CRASH_ROOT="+root)
						output, err := command.CombinedOutput()
						var exit *exec.ExitError
						if !errors.As(err, &exit) || exit.ExitCode() != 73 {
							t.Fatalf("crash helper: %v %s", err, output)
						}
						s, err := OpenSQLite(filepath.Join(root, ".agent-workflow"))
						if err != nil {
							t.Fatal(err)
						}
						defer s.Close()
						runs, err := s.List(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						var origin string
						for _, run := range runs {
							if run.ID == run.OriginID {
								origin = run.ID
							}
							if len(run.Jobs) != 0 {
								t.Fatalf("crash saved job result: %+v", run)
							}
						}
						gate := inputGate(t, s, origin)
						if gate.State != "pending" {
							t.Fatal(gate)
						}
						switch guard {
						case "missing":
							if _, err := s.db.Exec("DELETE FROM input_gates WHERE id=?", gate.ID); err != nil {
								t.Fatal(err)
							}
						case "step", "evidence", "payload":
							if guard == "step" {
								gate.StepID = "undeclared"
							}
							if guard == "evidence" {
								gate.EvidenceID = "unadmitted-evidence"
							}
							if guard == "payload" {
								gate.Payload = json.RawMessage(`{"changed":true}`)
							}
							gate.Fingerprint = inputFingerprint(gate)
							data, err := json.Marshal(gate)
							if err != nil {
								t.Fatal(err)
							}
							if _, err := s.db.Exec("UPDATE input_gates SET record=? WHERE origin_id=?", data, origin); err != nil {
								t.Fatal(err)
							}
						case "uncertain":
							if _, _, err := s.Begin(context.Background(), gate.RunID, "uncertain", "effect", 1, false); err != nil {
								t.Fatal(err)
							}
						}
						if err := s.Recover(context.Background()); err != nil {
							t.Fatal(err)
						}
						if guard != "missing" {
							answerGate(t, s, gate)
						}
						e := &Executor{WorkspaceDir: root, Store: s}
						registerTestAction(t, e, crashCheckpointSpec(t, root))
						run, err := e.Resume(context.Background(), origin, nil, retry)
						if guard != "none" {
							if err == nil {
								t.Fatalf("unsafe gate snapshot resumed: %+v", run)
							}
							gates, loadErr := s.Inputs(context.Background(), origin)
							if loadErr != nil {
								t.Fatal(loadErr)
							}
							if guard == "missing" {
								if len(gates) != 0 {
									t.Fatal("missing gate replaced", gates)
								}
							} else if len(gates) != 1 || gates[0].State != "accepted" || len(gates[0].History) != 2 {
								t.Fatal("unsafe answer/history changed", gates)
							}
							if data, _ := os.ReadFile(filepath.Join(root, "effects.log")); len(data) != 0 {
								t.Fatal("unsafe effect ran", string(data))
							}
							return
						}
						if err != nil || run == nil {
							t.Fatalf("crash continuation: %+v %v", run, err)
						}
						if strings.Contains(composition, "repeat") {
							if run.Status != RunStatusWaiting {
								t.Fatalf("second repeat: %+v", run)
							}
							gates, err := s.Inputs(context.Background(), origin)
							if err != nil || len(gates) != 2 || gates[0].ID != gate.ID || gates[0].State != "consumed" {
								t.Fatalf("first repeat replaced: %+v %v", gates, err)
							}
							answerGate(t, s, gates[1])
							run, err = e.Resume(context.Background(), origin, nil, false)
						}
						if err != nil || run.Status != RunStatusSucceeded {
							t.Fatalf("completion: %+v %v", run, err)
						}
						gates, err := s.Inputs(context.Background(), origin)
						if err != nil {
							t.Fatal(err)
						}
						if gates[0].ID != gate.ID || gates[0].RunID != gate.RunID || gates[0].State != "consumed" || len(gates[0].History) != 3 {
							t.Fatalf("crash gate replaced: %+v", gates)
						}
						wantGates := 1
						if strings.Contains(composition, "repeat") {
							wantGates = 2
						}
						if len(gates) != wantGates {
							t.Fatal("extra question", gates)
						}
						effects, _ := os.ReadFile(filepath.Join(root, "effects.log"))
						wantEffects := ""
						if checkpoint {
							wantEffects = strings.Repeat("before\n", wantGates)
						}
						if string(effects) != wantEffects {
							t.Fatalf("checkpoint replay: %q", effects)
						}
						current, err := s.List(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						for _, physical := range current {
							records, err := s.IterationAttempts(context.Background(), physical.ID, "review")
							if err != nil {
								t.Fatal(err)
							}
							for _, record := range records {
								if record.Attempt != 1 {
									t.Fatal("allocated fresh physical attempt", record)
								}
							}
						}
					})
				}
			}
		}
	}
}
