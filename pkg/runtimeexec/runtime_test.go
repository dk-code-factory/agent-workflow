package runtimeexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

func TestStrictMacOSCleanupRequiresContainers(t *testing.T) {
	if err := (Policy{Cleanup: "strict"}).ValidateFor("darwin"); err == nil {
		t.Fatal("native strict cleanup advertised on macOS")
	}
	if err := (Policy{Cleanup: "process-group"}).ValidateFor("darwin"); err != nil {
		t.Fatal(err)
	}
	if err := (Policy{Mode: "container", Image: "fixture", Cleanup: "strict"}).ValidateFor("darwin"); err != nil {
		t.Fatal(err)
	}
	if err := (Policy{Cleanup: "strict"}).ValidateFor("linux"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderHomeCannotChangeRuntimeIdentity(t *testing.T) {
	original := os.Getenv("HOME")
	env := runtimeEnvironment([]string{"HOME=/provider-private", "CODEX_HOME=/provider-codex", "ANTHROPIC_API_KEY=explicit-fixture", "EXTERNAL_MCP_TOKEN=must-not-forward"}, "")
	var home string
	for _, assignment := range env {
		key, value, _ := strings.Cut(assignment, "=")
		if key == "HOME" {
			home = value
		}
	}
	if home != original {
		t.Fatal("provider HOME changed host runtime store", home)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "/provider-codex") || strings.Contains(joined, "must-not-forward") || !strings.Contains(joined, "explicit-fixture") {
		t.Fatal("host runtime environment forwarding is incorrect")
	}
}

func TestNetworkPolicyIsIndependentAndFailsClosed(t *testing.T) {
	for _, mode := range []string{"", "local", "container"} {
		p := Policy{Mode: mode, NetworkRuntime: "team"}
		if mode == "container" {
			p.Image = "fixture"
		}
		if err := p.ValidateFor("linux"); err != nil {
			t.Fatal(mode, err)
		}
		if err := p.ValidateFor("darwin"); err == nil {
			t.Fatal("unsupported platform accepted")
		}
		p.Runtime = "docker"
		if err := p.ValidateFor("linux"); err == nil {
			t.Fatal("Docker containment accepted")
		}
	}
	if err := CheckPolicy(context.Background(), Policy{NetworkRuntime: "team"}); err == nil {
		t.Fatal("unresolved selection silently joined host")
	}
	p := Policy{NetworkRuntime: "team", Network: &network.Reference{Name: "team", Identity: strings.Repeat("a", 32), Generation: 7, Endpoint: network.Endpoint("team", strings.Repeat("a", 32))}}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var saved Policy
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if *saved.Network != *p.Network {
		t.Fatal("admitted runtime identity changed", saved)
	}
	// A missing retained generation cannot be resolved as another current runtime.
	if _, err = PinPolicy(context.Background(), saved); err == nil {
		t.Fatal("unavailable retained runtime accepted")
	}
	if _, err = Reconcile(context.Background(), Record{ID: NewID(), State: "launching", Network: p.Network}); err == nil {
		t.Fatal("unconfirmed runtime cleanup treated as absent")
	}
}

type networkPreparationJournal struct {
	record     Record
	states     []string
	failState  string
	failure    error
	cancel     context.CancelFunc
	cleanupCtx error
}

func (j *networkPreparationJournal) AdmitInvocation(_ context.Context, record Record) error {
	j.record = record
	j.states = append(j.states, record.State)
	return nil
}

func (j *networkPreparationJournal) UpdateInvocation(ctx context.Context, record Record) error {
	j.states = append(j.states, record.State)
	if record.State == "launching" && j.cancel != nil {
		j.cancel()
	}
	if record.State == "cleaned" {
		j.cleanupCtx = ctx.Err()
	}
	if record.State == j.failState {
		return j.failure
	}
	j.record = record
	return nil
}

func TestNetworkPreparationFailuresCleanUnstartedInvocation(t *testing.T) {
	for _, failure := range []string{"missing-relative-command", "extra-files"} {
		for _, cleanupSaveFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cleanup-save-fails=%t", failure, cleanupSaveFails), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				journal := &networkPreparationJournal{cancel: cancel}
				persistErr := errors.New("cleanup persistence failed")
				if cleanupSaveFails {
					journal.failState, journal.failure = "cleaned", persistErr
				}
				ctx = WithBinding(ctx, Binding{Journal: journal, Dir: t.TempDir()})
				cmd := exec.Command("missing-workflow-command-" + NewID())
				if failure == "missing-relative-command" {
					if !errors.Is(cmd.Err, exec.ErrNotFound) {
						t.Fatalf("fixture command unexpectedly available: %v", cmd.Err)
					}
				} else {
					cmd = exec.Command(os.Args[0])
					file, err := os.Open(os.DevNull)
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					cmd.ExtraFiles = []*os.File{file}
				}
				ref := network.Reference{Name: "unit-no-runtime", Identity: strings.Repeat("a", 32), Generation: 1}
				ref.Endpoint = network.Endpoint(ref.Name, ref.Identity)
				// Exercise preparation after the public admission health check.
				// Real Wrap errors need no runtime service or fake health response.
				stop, err := superviseNetwork(ctx, cmd, ref)
				if err == nil || stop != nil || cmd.Process != nil {
					t.Fatalf("invalid setup launched or succeeded: stop=%v err=%v process=%v", stop != nil, err, cmd.Process)
				}
				if failure == "missing-relative-command" && !errors.Is(err, exec.ErrNotFound) {
					t.Fatalf("original command error lost: %v", err)
				}
				if !reflect.DeepEqual(journal.states, []string{"intent", "launching", "cleaned"}) || journal.cleanupCtx != nil || ctx.Err() != context.Canceled {
					t.Fatalf("cleanup order/context: states=%v cleanup=%v caller=%v", journal.states, journal.cleanupCtx, ctx.Err())
				}
				if cleanupSaveFails {
					if !errors.Is(err, persistErr) || !errors.Is(err, process.ErrCleanup) || journal.record.State != "launching" {
						t.Fatalf("failed cleanup persistence erased uncertainty: record=%+v err=%v", journal.record, err)
					}
				} else {
					if journal.record.State != "cleaned" || errors.Is(err, process.ErrCleanup) {
						t.Fatalf("proven unstarted invocation retained uncertainty: %+v %v", journal.record, err)
					}
					if record, err := Reconcile(context.Background(), journal.record); err != nil || record.State != "cleaned" {
						t.Fatalf("cleaned setup queried a nonexistent runtime member: %+v %v", record, err)
					}
				}
			})
		}
	}
}

func TestNetworkLaunchingSaveFailureDoesNotStart(t *testing.T) {
	persistErr := errors.New("launching persistence failed")
	journal := &networkPreparationJournal{failState: "launching", failure: persistErr}
	ctx := WithBinding(context.Background(), Binding{Journal: journal, Dir: t.TempDir()})
	ref := network.Reference{Name: "unit-no-runtime", Identity: strings.Repeat("a", 32), Generation: 1}
	ref.Endpoint = network.Endpoint(ref.Name, ref.Identity)
	cmd := exec.Command(os.Args[0])
	stop, err := superviseNetwork(ctx, cmd, ref)
	if !errors.Is(err, persistErr) || errors.Is(err, process.ErrCleanup) || stop != nil || cmd.Process != nil || journal.record.State != "cleaned" {
		t.Fatalf("failed launch journal admitted work or blocked proven cleanup: %+v %v", journal.record, err)
	}
}
