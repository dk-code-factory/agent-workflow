package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/controller"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestReadExternalInputAnswer(t *testing.T) {
	for _, raw := range []string{`null`, " \n null\t ", ``, `true false`, `{"a":`, strings.Repeat(" ", (1<<20)+1)} {
		if _, err := readAnswer("-", strings.NewReader(raw)); err == nil {
			t.Fatal("invalid answer accepted")
		}
	}
	for _, raw := range []string{`false`, `0`, `""`, `[]`, `{"policy":"approve"}`, `1e400`, `{"large":[1e400,9007199254740993]}`} {
		if out, err := readAnswer("-", strings.NewReader(raw)); err != nil || string(out) != raw {
			t.Fatal(string(out), err)
		}
	}
	path := filepath.Join(t.TempDir(), "answer.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := readAnswer(path, nil); err != nil || string(out) != `{"ok":true}` {
		t.Fatal(string(out), err)
	}
}
func TestForegroundInputWaitReturnsHandleAndDistinctStatus(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	source := `jobs: {decide: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: evidence}}]}}`
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err := command(context.Background(), []string{"run", "--foreground", "--workspace", root, "workflows/main.yml"}, &out, &stderr)
	if !errors.Is(err, errInputWaiting) || !strings.Contains(out.String(), `"status": "waiting"`) || !strings.Contains(out.String(), `"gate_id"`) {
		t.Fatal(out.String(), stderr.String(), err)
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := store.List(context.Background())
	store.Close()
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	out.Reset()
	if err = command(context.Background(), []string{"inputs", "--workspace", root, runs[0].ID}, &out, &stderr); err != nil || !strings.Contains(out.String(), `"prompt": "q"`) {
		t.Fatal(out.String(), err)
	}
}

type inputCompletionAgent struct {
	started chan struct{}
	release chan struct{}
}

func (a *inputCompletionAgent) RunAgent(ctx context.Context, _ workflows.AgentRequest) (map[string]any, error) {
	close(a.started)
	select {
	case <-a.release:
		return map[string]any{"text": "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestInputCLICompletionPreservesLargeNumbers(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	source := `jobs:
  ask: {steps: [{id: q, uses: input/wait, with: {prompt: q, evidence-id: evidence}}]}
  after: {needs: ask, steps: [{uses: agent/fixture, with: {prompt: after}}]}
`
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent := &inputCompletionAgent{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := clirunner.LoadConfig(filepath.Join(root, "agents.yml"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	roles, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner := &controller.Server{Dir: store.Dir, Store: store, Executor: &workflows.Executor{WorkspaceDir: root, Store: store, Agents: agent, MaxParallel: 4, Roles: roles}}
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- owner.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-ownerDone; err != nil {
			t.Error(err)
		}
	}()
	api := controller.NewClient(store.Dir)
	for api.Call(ctx, "GET", "/health", nil, nil) != nil {
		if err := pause(ctx, time.Millisecond); err != nil {
			t.Fatal("controller did not start", err)
		}
	}
	var out, stderr bytes.Buffer
	if err = command(ctx, []string{"run", "--workspace", root, "workflows/main.yml"}, &out, &stderr); !errors.Is(err, errInputWaiting) {
		t.Fatal(out.String(), stderr.String(), err)
	}
	var waiting workflows.Run
	if err = json.Unmarshal(out.Bytes(), &waiting); err != nil || waiting.Status != workflows.RunStatusWaiting {
		t.Fatal(out.String(), err)
	}
	out.Reset()
	if err = command(ctx, []string{"interrupt", "--workspace", root, waiting.ID}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var gates []workflows.ExternalInputGate
	if err = api.Call(ctx, "GET", "/runs/"+waiting.ID+"/inputs", nil, &gates); err != nil || len(gates) != 1 {
		t.Fatal(gates, err)
	}
	value := `{"nested":[{"large":1e400,"integer":9007199254740993}]}`
	answerFile := filepath.Join(root, "answer.json")
	if err = os.WriteFile(answerFile, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = command(ctx, []string{"answer", "--workspace", root, "--gate", gates[0].ID, "--evidence-id", "evidence", "--answer-id", "numeric-cli-answer", "--answer-file", answerFile, waiting.ID}, &out, &stderr); err != nil {
		t.Fatal(out.String(), err)
	}
	var accepted workflows.ExternalInputGate
	if err = json.Unmarshal(out.Bytes(), &accepted); err != nil || accepted.State != "accepted" || accepted.Answer == nil {
		t.Fatal("CLI answer changed", out.String(), err)
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, accepted.Answer.Value); err != nil || compact.String() != value {
		t.Fatal("CLI answer changed numeric JSON", out.String(), err)
	}
	out.Reset()
	completed := make(chan error, 1)
	go func() { completed <- command(ctx, []string{"resume", "--workspace", root, waiting.ID}, &out, &stderr) }()
	select {
	case <-agent.started:
	case <-ctx.Done():
		t.Fatal("continuation did not reach dependent work", ctx.Err())
	}
	select {
	case err := <-completed:
		t.Fatal("CLI returned before continuation completed", err)
	default:
	}
	close(agent.release)
	select {
	case err = <-completed:
		if err != nil {
			t.Fatal(out.String(), stderr.String(), err)
		}
	case <-ctx.Done():
		t.Fatal("CLI completion polling did not finish", ctx.Err())
	}
	decoder := json.NewDecoder(bytes.NewReader(out.Bytes()))
	decoder.UseNumber()
	var done workflows.Run
	if err = decoder.Decode(&done); err != nil {
		t.Fatal("CLI encoded invalid numeric JSON", out.String(), err)
	}
	if done.Status != workflows.RunStatusSucceeded || done.Generation != waiting.Generation+1 {
		t.Fatal(done)
	}
	answer := done.Jobs["ask"].Steps["q"].Outputs["answer"].(map[string]any)
	nested := answer["nested"].([]any)[0].(map[string]any)
	if nested["large"] != json.Number("1e400") || nested["integer"] != json.Number("9007199254740993") {
		t.Fatal("CLI completion changed numeric answer", out.String())
	}
	if err = api.Call(ctx, "GET", "/runs/"+waiting.ID+"/inputs", nil, &gates); err != nil || gates[0].State != "consumed" || string(gates[0].Answer.Value) != value {
		t.Fatal("completion lost consumed answer", gates, err)
	}
}
