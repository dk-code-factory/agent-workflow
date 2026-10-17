package development

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestInterruptedRetentionResumesWithoutLosingOwnership(t *testing.T) {
	for _, boundary := range []string{"after", "database"} {
		t.Run(boundary, func(t *testing.T) {
			r, run := fixture(t)
			writer := create(t, r, run, "writer")
			ctx := context.Background()
			record, _, err := r.Store.Load(ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			record.Status = workflows.RunStatusFailed
			if err = r.Store.Save(ctx, record); err != nil {
				t.Fatal(err)
			}
			provider := filepath.Join(r.Root, ".agent-workflow", "providers", "codex", run, "owner")
			if err = os.MkdirAll(provider, 0700); err != nil {
				t.Fatal(err)
			}
			native := filepath.Join(t.TempDir(), "native-auth.json")
			if err = os.WriteFile(native, []byte("provider-owned"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(native, filepath.Join(provider, "auth.json")); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(r.Root, ".agent-workflow", "providers", "codex", strings.Repeat("f", 32))
			if err = os.MkdirAll(other, 0700); err != nil {
				t.Fatal(err)
			}
			interrupted := false
			r.retentionHook = func(_ string, state string) error {
				if state == boundary && !interrupted {
					interrupted = true
					return errors.New("fixture owner interrupted after removal boundary")
				}
				return nil
			}
			if err = r.Cleanup(ctx, run); err == nil {
				t.Fatal("interruption was ignored")
			}
			if _, _, err = r.Store.Load(ctx, run); err != nil {
				t.Fatal("database history deleted before removal completion", err)
			}
			if _, err = r.Store.QueueResume(ctx, run, "", true, 128); err == nil {
				t.Fatal("retiring run became executable")
			}
			r.retentionHook = nil
			if err = r.Cleanup(ctx, run); err != nil {
				t.Fatal("cleanup was not resumable", err)
			}
			if err = r.Cleanup(ctx, run); err != nil {
				t.Fatal("completed cleanup was not idempotent", err)
			}
			if _, err = os.Stat(str(writer, "path")); !os.IsNotExist(err) {
				t.Fatal("owned worktree retained")
			}
			if _, err = os.Stat(provider); !os.IsNotExist(err) {
				t.Fatal("engine provider state retained")
			}
			if data, err := os.ReadFile(native); err != nil || string(data) != "provider-owned" {
				t.Fatal("native authentication was removed", err)
			}
			if _, err = os.Stat(other); err != nil {
				t.Fatal("another origin's state was removed", err)
			}
			plan, err := r.Store.Retention(ctx, run)
			if err != nil || plan.State != "complete" {
				t.Fatal("ownership tombstone not retained", plan, err)
			}
		})
	}
}

func TestRetentionRejectsSymlinkedOwnershipRoot(t *testing.T) {
	r, run := fixture(t)
	ctx := context.Background()
	record, _, _ := r.Store.Load(ctx, run)
	record.Status = workflows.RunStatusSucceeded
	if err := r.Store.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(r.Root, ".agent-workflow", "providers", "claude")
	if err := os.MkdirAll(provider, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "keep"), []byte("caller"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, filepath.Join(provider, run)); err != nil {
		t.Fatal(err)
	}
	if err := r.Cleanup(ctx, run); err == nil {
		t.Fatal("symlinked origin ownership accepted")
	}
	if _, err := os.Stat(filepath.Join(foreign, "keep")); err != nil {
		t.Fatal("foreign path touched", err)
	}
}
