package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

type automaticRolesRunner struct {
	AgentRunner
	roles      json.RawMessage
	admissions int
	factory    func(json.RawMessage) (AgentRunner, error)
}

func (r *automaticRolesRunner) AdmittedRoles(context.Context) (json.RawMessage, error) {
	r.admissions++
	return append(json.RawMessage(nil), r.roles...), nil
}

func (r *automaticRolesRunner) RunnerForRoles(roles json.RawMessage) (AgentRunner, error) {
	return r.factory(roles)
}

type retainedRolesRunner struct {
	AgentRunner
	check func([]string) error
}

func (r retainedRolesRunner) Preflight(_ context.Context, ids []string) error { return r.check(ids) }
func (retainedRolesRunner) SecretValues() map[string]string {
	return map[string]string{"provider": "retained-provider-secret"}
}

func TestResumeRestoresAutomaticallyAdmittedRoles(t *testing.T) {
	for _, scenario := range []string{"automatic", "explicit-precedence", "factory-error", "changed-identity"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			ref := network.Reference{Name: "shared", Identity: strings.Repeat("a", 32), Generation: 2, Endpoint: network.Endpoint("shared", strings.Repeat("a", 32))}
			type roles struct {
				Model   string
				Network network.Reference
			}
			admitted := roles{Model: "admitted-model", Network: ref}
			raw, err := json.Marshal(admitted)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var received []json.RawMessage
			var factoryErr error
			current := ref
			factory := func(raw json.RawMessage) (AgentRunner, error) {
				received = append(received, append(json.RawMessage(nil), raw...))
				if factoryErr != nil {
					return nil, factoryErr
				}
				var saved roles
				if err := json.Unmarshal(raw, &saved); err != nil {
					return nil, err
				}
				return retainedRolesRunner{AgentRunner: agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
					calls.Add(1)
					return map[string]any{"model": saved.Model, "text": "retained-provider-secret"}, nil
				}), check: func(ids []string) error {
					if !reflect.DeepEqual(ids, []string{"builder"}) || saved.Network != current {
						return errors.New("retained runtime identity changed")
					}
					return nil
				}}, nil
			}
			shared := &automaticRolesRunner{roles: raw, factory: factory, AgentRunner: agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				return nil, errors.New("shared unpinned runner used")
			})}
			e, journal := durableExecutor(t, "jobs: {x: {steps: [{uses: agent/builder, with: {prompt: hi}}]}}", shared)
			run, err := e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			_, snap, err := journal.Load(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			live := roles{Model: "replacement-model", Network: network.Reference{Name: "other", Identity: strings.Repeat("b", 32), Generation: 3, Endpoint: network.Endpoint("other", strings.Repeat("b", 32))}}
			shared.roles, err = json.Marshal(live)
			if err != nil {
				t.Fatal(err)
			}
			liveRaw := append(json.RawMessage(nil), shared.roles...)
			switch scenario {
			case "explicit-precedence":
				e.RoleRunner = factory
				shared.factory = func(json.RawMessage) (AgentRunner, error) {
					t.Fatal("automatic factory overrode explicit RoleRunner")
					return nil, nil
				}
			case "factory-error":
				factoryErr = errors.New("retained role factory failed")
			case "changed-identity":
				current = live.Network
			}
			result, err := e.Resume(ctx, run.ID, nil, false)
			if len(received) != 2 || !bytes.Equal(received[1], snap.Roles) || !bytes.Equal(received[0], raw) {
				t.Fatalf("factory did not receive retained roles: %q", received)
			}
			if shared.admissions != 1 || e.Agents != shared || !bytes.Equal(shared.roles, liveRaw) || e.Roles != nil {
				t.Fatal("resume re-admitted roles or mutated the shared executor/runner")
			}
			if scenario == "factory-error" || scenario == "changed-identity" {
				if err == nil || calls.Load() != 0 {
					t.Fatalf("resume did not fail closed: result=%+v error=%v calls=%d", result, err, calls.Load())
				}
				if scenario == "factory-error" && !errors.Is(err, factoryErr) || scenario == "changed-identity" && !strings.Contains(err.Error(), "retained runtime identity changed") {
					t.Fatalf("resume lost reconstruction/preflight error: %v", err)
				}
				saved, _, loadErr := journal.Load(ctx, run.ID)
				attempts, attemptErr := journal.Attempts(ctx, run.ID)
				if loadErr != nil || attemptErr != nil || saved.Status != RunStatusQueued || len(attempts) != 0 {
					t.Fatalf("failed reconstruction launched work: run=%+v attempts=%+v errors=%v/%v", saved, attempts, loadErr, attemptErr)
				}
				return
			}
			if err != nil || result == nil || result.Status != RunStatusSucceeded || calls.Load() != 1 {
				t.Fatalf("resume failed: result=%+v error=%v calls=%d", result, err, calls.Load())
			}
			saved, _, err := journal.Load(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, completed := range []*Run{result, saved} {
				outputs := completed.Jobs["x"].Steps["step_1"].Outputs
				if outputs["model"] != admitted.Model || outputs["text"] != "[REDACTED]" {
					t.Fatalf("retained runner selection or redaction lost: %+v", outputs)
				}
			}
		})
	}
}
