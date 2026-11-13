package workflows

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const structuredWorkflow = `schemas:
  Input: {type: object, properties: {value: {type: string}}, required: [value], additionalProperties: false}
  Output: {type: object, properties: {head: {type: string}}, required: [head], additionalProperties: false}
jobs:
  capture:
    container: ubuntu:24.04
    outputs: {head: '${{ steps.current.outputs.head }}'}
    steps:
      - id: current
        argv: [helper]
        protocol: action-v1
        effect: read_only
        input-schema: Input
        output-schema: Output
        input: {value: ready}
`

type protocolFixture struct {
	mu                                                 sync.Mutex
	binding                                            string
	output                                             string
	execute, validations, reconciles, unknown          int
	mutation, lostResponse, badOutput, badCapabilities bool
	receipts                                           map[string]bool
	requests                                           []map[string]any
	env                                                []string
	calls                                              []protocolHookCall
}

type protocolHookCall struct {
	phase, operationKey string
	env                 []string
}

func (f *protocolFixture) call(argv []string, input []byte, env []string) ([]byte, []byte, int, bool) {
	if len(argv) == 0 || argv[0] != "helper" {
		return nil, nil, 0, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var r map[string]any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if err := decoder.Decode(&r); err != nil {
		return nil, []byte(err.Error()), 1, true
	}
	f.requests = append(f.requests, r)
	f.env = append([]string(nil), env...)
	phase, _ := r["phase"].(string)
	v := map[string]any{"protocol": "action-v1", "phase": phase}
	ac, ok := r["context"].(map[string]any)
	if !ok {
		return nil, []byte(fmt.Sprintf("missing context in %s", input)), 1, true
	}
	key := ac["operation_key"].(string)
	f.calls = append(f.calls, protocolHookCall{phase: phase, operationKey: key, env: append([]string(nil), env...)})
	head := f.output
	if head == "" {
		head = "ready"
	}
	result := map[string]any{"output": map[string]any{"head": head}, "evidence": map[string]any{"head": "ready"}}
	switch phase {
	case "bindings":
		v["capabilities"] = []string{"bindings", "execute", "validate_checkpoint", "reconcile"}
		v["read_only_phases"] = []string{"bindings", "validate_checkpoint", "reconcile"}
		if f.badCapabilities {
			v["capabilities"] = []string{"bindings"}
		}
		v["bindings"] = map[string]any{"resource": f.binding}
	case "execute":
		f.execute++
		if f.mutation {
			if f.receipts == nil {
				f.receipts = map[string]bool{}
			}
			f.receipts[key] = true
		}
		if f.lostResponse {
			return nil, []byte("response lost after effect"), 1, true
		}
		if f.badOutput {
			result["output"] = map[string]any{"wrong": true}
		}
		v["result"] = result
	case "validate_checkpoint":
		f.validations++
		v["valid"] = true
	case "reconcile":
		f.reconciles++
		v["outcome"] = "unknown"
		if f.reconciles > f.unknown {
			v["proof"] = map[string]any{"operation_key": key}
			if f.receipts[key] {
				v["outcome"] = "completed"
				v["result"] = result
			} else {
				v["outcome"] = "no_effect"
			}
		}
	default:
		return nil, []byte("unsupported phase"), 1, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, []byte(err.Error()), 1, true
	}
	return b, nil, 0, true
}
func installProtocolWorkflow(t *testing.T, e *Executor, d *workflowDocker, f *protocolFixture, source string) {
	t.Helper()
	d.helper = f.call
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/test.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestStructuredHelperOutputReferencesAcrossScopes(t *testing.T) {
	e, _, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource-a"}
	source := structuredWorkflow + `      - id: local
        argv: [echo, '${{ steps.current.outputs.head }}']
  consumer:
    needs: capture
    steps:
      - id: read
        argv: [echo, '${{ needs.capture.steps.current.outputs.head }}']
`
	installProtocolWorkflow(t, e, d, f, source)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	for _, consumer := range []struct{ job, step string }{{"capture", "local"}, {"consumer", "read"}} {
		if got := run.Jobs[consumer.job].Steps[consumer.step].Outputs["stdout"]; got != "ready\n" {
			t.Fatalf("%s.%s could not consume the helper output: %q", consumer.job, consumer.step, got)
		}
	}
}

func TestStructuredHelperCompletedRecoveryUsesOwnedValidationAndBindings(t *testing.T) {
	e, store, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource-a"}
	installProtocolWorkflow(t, e, d, f, structuredWorkflow)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["capture"].Outputs["head"] != "ready" {
		t.Fatal(run)
	}
	records, err := store.ActionAttempts(t.Context(), run.ID)
	if err != nil || len(records) != 1 || records[0].State != "completed" {
		t.Fatalf("%+v %v", records, err)
	}
	_, snap, err := store.Load(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Definitions["workflows/test.yml"].Jobs["capture"].Steps[0].OutputSchema != "Output" {
		t.Fatal("schema authoring name changed")
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.execute != 1 || f.validations == 0 {
		t.Errorf("executions=%d validation=%d", f.execute, f.validations)
	}
	requests := append([]map[string]any(nil), f.requests...)
	f.mu.Unlock()
	validatedOwner := false
	for _, r := range requests {
		owner := r["runtime_owner"].(map[string]any)
		id := owner["run_id"].(string)
		if id != "" && id != run.ID {
			validatedOwner = true
			var n int
			if err := store.db.QueryRow("SELECT count(*) FROM invocations WHERE run_id=?", id).Scan(&n); err != nil || n == 0 {
				t.Fatalf("unowned validation %s %v", id, err)
			}
			ac, ok := r["context"].(map[string]any)
			if !ok {
				t.Fatal("missing context", r)
			}
			if ac["run_id"] != run.ID {
				t.Fatal("original action context overwritten")
			}
		}
	}
	if !validatedOwner {
		t.Fatal("completed validation did not obtain a runtime owner")
	}
	f.mu.Lock()
	f.binding = "resource-b"
	f.mu.Unlock()
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil || !strings.Contains(err.Error(), "bindings changed") {
		t.Fatalf("binding drift accepted: %v", err)
	}
}
func TestStructuredMutationResponseLossRemainsUnknownUntilReconciliation(t *testing.T) {
	e, store, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource", mutation: true, lostResponse: true, unknown: 2}
	source := strings.ReplaceAll(structuredWorkflow, "effect: read_only", "effect: external_mutation")
	installProtocolWorkflow(t, e, d, f, source)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err == nil || run == nil {
		t.Fatalf("%+v %v", run, err)
	}
	for i := 0; i < 2; i++ {
		if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil {
			t.Fatal("unknown mutation was retried")
		}
	}
	final, err := e.Resume(t.Context(), run.ID, nil, false)
	if err != nil || final.Status != RunStatusSucceeded {
		t.Fatalf("%+v %v", final, err)
	}
	f.mu.Lock()
	if f.execute != 1 || f.reconciles != 3 {
		t.Errorf("executions=%d reconciles=%d", f.execute, f.reconciles)
	}
	f.mu.Unlock()
	records, err := store.ActionAttempts(t.Context(), run.ID)
	if err != nil || len(records) != 1 || len(records[0].Reconciliations) != 3 || records[0].State != "completed" {
		t.Fatalf("%+v %v", records, err)
	}
}
func TestStructuredHelperLostCompletionCommitReconcilesWithoutReplay(t *testing.T) {
	e, store, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource", mutation: true}
	source := strings.ReplaceAll(structuredWorkflow, "effect: read_only", "effect: external_mutation")
	installProtocolWorkflow(t, e, d, f, source)
	if _, err := store.db.Exec("CREATE TRIGGER lose_helper_checkpoint BEFORE UPDATE ON action_attempts BEGIN SELECT RAISE(FAIL,'lost completion commit'); END"); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err == nil || run == nil {
		t.Fatalf("%+v %v", run, err)
	}
	if _, err = store.db.Exec("DROP TRIGGER lose_helper_checkpoint"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execute != 1 || f.reconciles != 1 {
		t.Fatalf("replayed effect: execute=%d reconcile=%d", f.execute, f.reconciles)
	}
}
func TestStructuredHelperInputOutputAndCapabilityValidation(t *testing.T) {
	for _, mode := range []string{"input", "output", "capabilities"} {
		t.Run(mode, func(t *testing.T) {
			e, _, d := containerExecutor(t)
			f := &protocolFixture{binding: "resource", badOutput: mode == "output", badCapabilities: mode == "capabilities"}
			source := structuredWorkflow
			if mode == "input" {
				source = strings.Replace(source, "input: {value: ready}", "input: {value: 123}", 1)
			}
			installProtocolWorkflow(t, e, d, f, source)
			if _, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"}); err == nil {
				t.Fatal("invalid contract accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if mode != "output" && f.execute != 0 {
				t.Fatal("effect dispatched before validation")
			}
			if mode == "input" && len(f.requests) != 0 {
				t.Fatal("invalid business input dispatched")
			}
		})
	}
}

func TestStructuredHelpersCannotAppearInsideOrdinaryLifecycleHandlers(t *testing.T) {
	for _, handler := range []string{"onError", "onFailure", "finally"} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%t", handler, nested), func(t *testing.T) {
				helper := "{argv: [helper], protocol: action-v1, effect: external_mutation, input-schema: {type: object}, output-schema: {type: object}}"
				if nested {
					helper = "{argv: [echo, nested], finally: [" + helper + "]}"
				}
				source := "jobs: {build: {container: ubuntu:24.04, steps: [{argv: [echo, ordinary], " + handler + ": [" + helper + "]}]}}"
				w, err := Parse([]byte(source))
				if err != nil {
					t.Fatal(err)
				}
				if err = Validate(w); err == nil || !strings.Contains(err.Error(), "structured helpers cannot appear inside ordinary lifecycle handlers") {
					t.Fatalf("structured handler was not rejected: %v", err)
				}
			})
		}
	}
}

func TestContainerProtocolRejectsFramingVersionsAndLimits(t *testing.T) {
	for _, data := range []string{`{"protocol":"action-v1","phase":"execute"} {}`, `{"protocol":"v2","phase":"execute"}`, `{"protocol":"action-v1","phase":"bindings"}`, `{"protocol":"action-v1","phase":"execute","unknown":true}`, `not-json`} {
		if _, err := decodeContainerResponse([]byte(data), "execute"); err == nil {
			t.Fatal("accepted", data)
		}
	}
	for _, limit := range []int{1 << 20, 4 << 20} {
		b := boundedProtocolOutput{limit: limit}
		if _, err := b.Write(make([]byte, limit)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Write([]byte("x")); err == nil || !b.overflow {
			t.Fatal("unbounded protocol output")
		}
	}
}

func TestContainerRecoveryRequestReservesOnlyAdmittedCapability(t *testing.T) {
	for _, group := range []string{"", "grouped"} {
		t.Run("group="+group, func(t *testing.T) {
			store := taskStateStore(t)
			run, ctx := taskStateRun(t, store, group)
			e := &Executor{Store: store}
			ac := ActionContext{RunID: run.ID, JobID: "capture", StepID: "capture", AttemptID: strings.Repeat("a", 32), Attempt: 1, Generation: 1, OperationKey: strings.Repeat("k", 64)}
			f := executionFence{id: strings.Repeat("0", 32), generation: math.MaxInt64}
			input, bindings := json.RawMessage(`{}`), json.RawMessage(`{}`)
			var descriptor *taskStateDescriptor
			if group != "" {
				descriptor = &taskStateDescriptor{Socket: taskStateMount + "/rpc.sock", Token: strings.Repeat("t", 64), Group: group}
			}
			result := ActionValue{Output: json.RawMessage(`{"value":""}`)}
			empty, err := encodeContainerRequest(f, ac, "validate_checkpoint", input, bindings, result, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			value := strings.Repeat("x", (1<<20)-len(empty))
			result.Output, err = json.Marshal(map[string]string{"value": value})
			if err != nil {
				t.Fatal(err)
			}
			wire, err := encodeContainerRequest(f, ac, "validate_checkpoint", input, bindings, result, descriptor)
			if err != nil || len(wire) != 1<<20 {
				t.Fatalf("boundary recovery request: %d bytes, %v", len(wire), err)
			}
			if err = e.validateContainerRecoveryRequest(ctx, ac, input, bindings, result); err != nil {
				t.Fatalf("rejected recovery request that fits its actual descriptor: %v", err)
			}
			result.Output, err = json.Marshal(map[string]string{"value": value + "x"})
			if err != nil {
				t.Fatal(err)
			}
			if err = e.validateContainerRecoveryRequest(ctx, ac, input, bindings, result); err == nil {
				t.Fatal("accepted recovery request over the wire limit")
			}
		})
	}
}
func TestStructuredEnvCredentialsStayOutOfSnapshotsAndIdentities(t *testing.T) {
	e, store, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource"}
	source := strings.Replace(structuredWorkflow, "        argv: [helper]", "        argv: [helper]\n        env: {TOKEN: '${{ secrets.token }}'}", 1)
	installProtocolWorkflow(t, e, d, f, source)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", Secrets: map[string]string{"token": "private-credential-value"}})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	env := strings.Join(f.env, "\n")
	resumeStart := len(f.calls)
	expectedTokens := map[string]string{}
	for _, call := range f.calls {
		expectedTokens[call.operationKey] = "rotated-credential"
	}
	f.mu.Unlock()
	if !strings.Contains(env, "TOKEN=private-credential-value") {
		t.Fatal("credential was not explicitly injected")
	}
	assertStructuredCredentialsNotStored(t, store, "private-credential-value")
	if _, err = e.Resume(t.Context(), run.ID, map[string]string{"token": "rotated-credential"}, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	assertResumedHookCredentials(t, f.calls[resumeStart:], expectedTokens)
	f.mu.Unlock()
	assertStructuredCredentialsNotStored(t, store, "private-credential-value", "rotated-credential")
}

func assertStructuredCredentialsNotStored(t *testing.T, store *SQLiteStore, values ...string) {
	t.Helper()
	for _, location := range []struct{ table, column string }{
		{"runs", "record"},
		{"runs", "snapshot"},
		{"action_attempts", "record"},
		{"invocations", "record"},
		{"attempts", "record"},
	} {
		query := fmt.Sprintf("SELECT count(*) FROM %s WHERE CAST(%s AS TEXT) LIKE ?", location.table, location.column)
		for _, value := range values {
			var count int
			if err := store.db.QueryRow(query, "%"+value+"%").Scan(&count); err != nil || count != 0 {
				t.Errorf("credential persisted in %s.%s: %d %v", location.table, location.column, count, err)
			}
		}
	}
}

func assertResumedHookCredentials(t *testing.T, calls []protocolHookCall, expectedTokens map[string]string) {
	t.Helper()
	if len(expectedTokens) == 0 {
		t.Error("no original helper operations recorded")
	}
	seen := map[string]map[string]bool{}
	for _, call := range calls {
		want, ok := expectedTokens[call.operationKey]
		if !ok {
			t.Errorf("unexpected resumed operation %s", call.operationKey)
			continue
		}
		var tokens []string
		for _, assignment := range call.env {
			if strings.HasPrefix(assignment, "TOKEN=") {
				tokens = append(tokens, strings.TrimPrefix(assignment, "TOKEN="))
			}
		}
		if len(tokens) != 1 || tokens[0] != want {
			t.Errorf("%s hook for %s received TOKEN=%q, want %q", call.phase, call.operationKey, tokens, want)
		}
		if seen[call.operationKey] == nil {
			seen[call.operationKey] = map[string]bool{}
		}
		seen[call.operationKey][call.phase] = true
	}
	for operation := range expectedTokens {
		for _, phase := range []string{"bindings", "validate_checkpoint"} {
			if !seen[operation][phase] {
				t.Errorf("resumed operation %s did not receive a %s hook", operation, phase)
			}
		}
	}
}

func TestStructuredInputPreservesJSONNumbersAndLiteralNestedExpressions(t *testing.T) {
	e, _, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource"}
	source := strings.Replace(structuredWorkflow, "value: {type: string}", "value: {type: object}", 1)
	source = "on: {workflow_call: {inputs: {payload: {type: object}}}}\n" + strings.Replace(source, "input: {value: ready}", "input: {value: '${{ inputs.payload }}'}", 1)
	installProtocolWorkflow(t, e, d, f, source)
	payload := map[string]any{"large": json.Number("9007199254740993"), "nested": []any{true, nil, "", map[string]any{"literal": "${{ secrets.do_not_evaluate }}"}}}
	if _, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", Inputs: map[string]any{"payload": payload}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		input := r["input"].(map[string]any)
		b, _ := json.Marshal(input["value"])
		want, _ := json.Marshal(payload)
		if !bytes.Equal(b, want) {
			t.Fatalf("input changed: %s want %s", b, want)
		}
	}
}
func TestNestedStructuredCallsRetainDistinctOperationsAndCredentialAliases(t *testing.T) {
	e, store, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource"}
	helper := "on: {workflow_call: {secrets: {local: {required: true}}}}\n" + strings.Replace(structuredWorkflow, "        argv: [helper]", "        argv: [helper]\n        env: {TOKEN: '${{ secrets.local }}'}", 1)
	installProtocolWorkflow(t, e, d, f, helper)
	caller := `jobs:
  first:
    uses: workflows/test.yml
    secrets: {local: '${{ secrets.alpha }}'}
  second:
    uses: workflows/test.yml
    secrets: {local: '${{ secrets.beta }}'}
`
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/caller.yml"), []byte(caller), 0600); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/caller.yml", Secrets: map[string]string{"alpha": "same-credential", "beta": "same-credential"}})
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ActionAttempts(t.Context(), run.ID)
	if err != nil || len(records) != 2 {
		t.Fatalf("%+v %v", records, err)
	}
	if records[0].Context.OperationKey == records[1].Context.OperationKey || records[0].Context.CredentialRefs == records[1].Context.CredentialRefs {
		t.Fatal("reusable invocations or credential aliases collapsed")
	}
	rotated := map[string]string{"alpha": "rotated-alpha", "beta": "rotated-beta"}
	expectedTokens := map[string]string{}
	seenAliases := map[string]bool{}
	for _, record := range records {
		var references map[string]string
		if err := json.Unmarshal([]byte(record.Context.CredentialRefs), &references); err != nil {
			t.Fatal(err)
		}
		alias := references["TOKEN"]
		value, ok := rotated[alias]
		if !ok || len(references) != 1 {
			t.Fatalf("unexpected retained credential aliases: %v", references)
		}
		expectedTokens[record.Context.OperationKey] = value
		seenAliases[alias] = true
	}
	if len(seenAliases) != 2 {
		t.Fatalf("nested helpers did not retain both root aliases: %v", seenAliases)
	}
	f.mu.Lock()
	resumeStart := len(f.calls)
	f.mu.Unlock()
	assertStructuredCredentialsNotStored(t, store, "same-credential")
	if _, err = e.Resume(t.Context(), run.ID, rotated, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	assertResumedHookCredentials(t, f.calls[resumeStart:], expectedTokens)
	if f.execute != 2 || f.validations != 2 {
		t.Fatalf("execute=%d validation=%d", f.execute, f.validations)
	}
	assertStructuredCredentialsNotStored(t, store, "same-credential", "rotated-alpha", "rotated-beta")
}

func TestStructuredProtocolWorksOnStandaloneDockerSteps(t *testing.T) {
	e, _, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource"}
	source := strings.Replace(structuredWorkflow, "    container: ubuntu:24.04\n", "", 1)
	source = strings.Replace(source, "        argv: [helper]", "        uses: docker://ubuntu:24.04\n        with: {entrypoint: helper, args: ''}", 1)
	installProtocolWorkflow(t, e, d, f, source)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execute != 1 || f.validations != 1 {
		t.Fatal("standalone checkpoint was not reused")
	}
}
func TestOrdinaryCommandEnvUsesExistingRedaction(t *testing.T) {
	e, _, _ := containerExecutor(t)
	source := `jobs: {build: {steps: [{id: env, argv: [sh, -c, 'printf "%s" "$TOKEN"'], env: {TOKEN: '${{ secrets.token }}'}}]}}`
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/test.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", Secrets: map[string]string{"token": "ordinary-private-token"}})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["build"].Steps["env"].Outputs["stdout"] != "[REDACTED]" {
		t.Fatal(run)
	}
}
