package development

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestPackageRetentionRetriesReadonlyTreesWithoutFollowingLinks(t *testing.T) {
	for _, boundary := range []string{"interrupted", "replaced"} {
		t.Run(boundary, func(t *testing.T) {
			r, _ := fixture(t)
			id := strings.Repeat("a", 32)
			group := filepath.Join(r.Store.Dir, "assets", id)
			t.Cleanup(func() {
				_ = filepath.WalkDir(group, func(name string, entry fs.DirEntry, err error) error {
					if err == nil && entry.IsDir() {
						return os.Chmod(name, 0700)
					}
					return err
				})
			})
			bundle, err := r.Store.CaptureAssetBundle(t.Context(), id, []workflows.AssetInput{
				{Path: "nested/script", Mode: 0750, Data: []byte("captured")},
				{Path: "nested/empty", Mode: fs.ModeDir | 0710},
			})
			if err != nil {
				t.Fatal(err)
			}
			run := &workflows.Run{Version: 1, ID: id, OriginID: id, WorkflowRef: "workflows/main.yml", Status: workflows.RunStatusSucceeded}
			snap := workflows.Snapshot{Version: 1, Metadata: map[string]any{"origin_run_id": id}, Package: &workflows.PackageBinding{Version: 1, Name: "fixture", PackageVersion: "1.0.0", EngineVersion: "0.2.2", Skill: "task", Workflow: run.WorkflowRef, Digest: bundle.ID, Assets: bundle}}
			if err := r.Store.Admit(t.Context(), run, snap); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.Store.PackageAssets(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			foreign := t.TempDir()
			if err := os.Chmod(foreign, 0710); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(foreign, "keep")
			if err := os.WriteFile(keep, []byte("unrelated"), 0400); err != nil {
				t.Fatal(err)
			}
			// Child symlinks can be removed, but never chmod their targets.
			if err := os.Symlink(foreign, filepath.Join(group, "foreign-link")); err != nil {
				t.Fatal(err)
			}
			r.retentionHook = func(name, state string) error {
				if name != group || state != "before" {
					return nil
				}
				if boundary == "interrupted" {
					return errors.New("interrupted at asset boundary")
				}
				if err := os.Rename(group, group+".saved"); err != nil {
					return err
				}
				return os.Symlink(foreign, group)
			}
			if err := r.Cleanup(t.Context(), id); err == nil {
				t.Fatal("interruption/replacement was ignored")
			}
			if boundary == "replaced" {
				if err := os.Remove(group); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(group+".saved", group); err != nil {
					t.Fatal(err)
				}
			}
			r.retentionHook = nil
			if err := r.Cleanup(t.Context(), id); err != nil {
				t.Fatal("readonly retention could not resume", err)
			}
			if _, err := os.Lstat(group); !os.IsNotExist(err) {
				t.Fatal("owned package assets retained", err)
			}
			info, err := os.Stat(foreign)
			if err != nil || info.Mode().Perm() != 0710 {
				t.Fatal("foreign directory permissions changed", info, err)
			}
			info, err = os.Stat(keep)
			data, readErr := os.ReadFile(keep)
			if err != nil || readErr != nil || info.Mode().Perm() != 0400 || string(data) != "unrelated" {
				t.Fatal("foreign bytes or mode changed", info, err, readErr)
			}
		})
	}
}
