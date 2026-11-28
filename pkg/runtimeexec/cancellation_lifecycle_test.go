//go:build linux || darwin

package runtimeexec

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

type cancellationLifecycleJournal struct {
	mu       sync.Mutex
	record   Record
	cleanups int
}

func (j *cancellationLifecycleJournal) AdmitInvocation(_ context.Context, record Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.record = record
	return nil
}

func (j *cancellationLifecycleJournal) UpdateInvocation(_ context.Context, record Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.record = record
	if record.State == "cleaned" || record.State == "uncertain" {
		j.cleanups++
	}
	return nil
}

func (j *cancellationLifecycleJournal) snapshot() (Record, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.record, j.cleanups
}

func TestSupervisorCancellationDefersFinalizationUntilWaitOwnerCleanup(t *testing.T) {
	for _, pendingWait := range []bool{false, true} {
		name := "before-wait"
		if pendingWait {
			name = "pending-wait"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			journal := &cancellationLifecycleJournal{}
			ctx = WithBinding(ctx, Binding{Journal: journal, Dir: t.TempDir()})
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "printf ready; read reply")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cleanup, err := Supervise(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			// Capture the termination-only callback before any adapter overrides it.
			terminate := cmd.Cancel
			if terminate == nil {
				cleanup()
				t.Fatal("supervisor did not install cancellation")
			}
			wait := make(chan error, 1)
			started, waiting, joined := false, false, false
			defer func() {
				_ = terminate()
				if started && !joined {
					if !waiting {
						go func() { wait <- cmd.Wait() }()
					}
					<-wait
				}
				cleanup()
			}()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			started = true
			ready := make([]byte, len("ready"))
			if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready" {
				t.Fatalf("fixture readiness: %q %v", ready, err)
			}
			if !pendingWait {
				if err := terminate(); err != nil {
					t.Fatal(err)
				}
			}
			waiting = true
			go func() { wait <- cmd.Wait() }()
			var cancellations sync.WaitGroup
			for range 8 {
				cancellations.Go(func() { _ = terminate() })
			}
			cancellations.Wait()
			if record, count := journal.snapshot(); record.State != "launching" || count != 0 {
				t.Fatalf("termination finalized before owner cleanup: %+v count=%d", record, count)
			}
			if !joined {
				<-wait
				joined = true
			}
			cleanup()
			cleanup()
			_ = terminate()
			if record, count := journal.snapshot(); record.State != "cleaned" || count != 1 {
				t.Fatalf("post-Wait cleanup was not exactly once: %+v count=%d", record, count)
			} else if err := process.ReadCompletion(record.CompletionPath, record.Token); err != nil {
				t.Fatalf("guardian completion proof was lost: %v", err)
			}
		})
	}
}

func TestSupervisorStartFailureFinalizesOnceWithoutWait(t *testing.T) {
	journal := &cancellationLifecycleJournal{}
	ctx := WithBinding(t.Context(), Binding{Journal: journal, Dir: t.TempDir()})
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0")
	cmd.Dir = filepath.Join(t.TempDir(), "missing-directory")
	cleanup, err := Supervise(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := cmd.Start(); err == nil {
		_ = cmd.Wait()
		t.Fatal("fixture unexpectedly started")
	}
	cleanup()
	cleanup()
	if record, count := journal.snapshot(); record.State != "cleaned" || count != 1 {
		t.Fatalf("unstarted cleanup was not exactly once: %+v count=%d", record, count)
	}
}
