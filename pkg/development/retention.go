package development

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dkropachev/agent-workflow/internal/ownership"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func (r *Runtime) Cleanup(ctx context.Context, id string) error {
	owner, err := ownership.Own(r.Store.Dir)
	if err != nil {
		return err
	}
	defer owner.Close()
	if err = r.Store.Migrate(ctx, owner); err != nil {
		return err
	}
	return r.CleanupOwned(ctx, id, owner)
}

// CleanupOwned journals each filesystem/effect boundary before removal. The
// tombstone remains after the run records are deleted, making retries idempotent.
func (r *Runtime) CleanupOwned(ctx context.Context, id string, owner *ownership.Lock) error {
	if !owner.Holds(r.Store.Dir) {
		return fmt.Errorf("cleanup requires exclusive workspace ownership")
	}
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return fmt.Errorf("invalid run id")
	}
	root, err := r.canonicalRoot()
	if err != nil {
		return err
	}
	state, err := filepath.Abs(r.Store.Dir)
	if err != nil {
		return err
	}
	state, err = filepath.EvalSymlinks(state)
	if err != nil {
		return err
	}
	if state != filepath.Join(root, ".agent-workflow") {
		return fmt.Errorf("retention store/workspace identity mismatch")
	}
	plan, err := r.Store.Retention(ctx, id)
	if err == nil {
		if plan.Root != root {
			return fmt.Errorf("retention workspace identity changed")
		}
		if plan.State == "complete" {
			return nil
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err = r.Store.OriginRuns(ctx, id); err != nil {
			return err
		}
		plan = workflows.Retention{OriginID: id, Root: root}
		group := filepath.Join(state, "workspaces", id)
		if _, statErr := os.Lstat(group); statErr == nil {
			if err = validateOwnedPath(state, group); err != nil {
				return err
			}
			registered, listErr := r.registeredWorktrees(ctx)
			if listErr != nil {
				return listErr
			}
			for _, path := range registered {
				if inside(group, path) {
					plan.Removals = append(plan.Removals, workflows.Removal{Kind: "worktree", Path: path, State: "planned"})
				}
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		plan.Removals = append(plan.Removals, workflows.Removal{Kind: "directory", Path: group, State: "planned"})
		for _, provider := range []string{"claude", "codex", "commands"} {
			plan.Removals = append(plan.Removals, workflows.Removal{Kind: "directory", Path: filepath.Join(state, "providers", provider, id), State: "planned"})
		}
		invocations, listErr := r.Store.InvocationDirectories(ctx, id)
		if listErr != nil {
			return listErr
		}
		for _, path := range invocations {
			plan.Removals = append(plan.Removals, workflows.Removal{Kind: "directory", Path: path, State: "planned"})
		}
		plan, err = r.Store.BeginRetention(ctx, plan, owner)
		if err != nil {
			return err
		}
	} else {
		return err
	}
	if _, err = r.Store.OriginRuns(ctx, id); err != nil {
		return err
	}
	for index := range plan.Removals {
		removal := &plan.Removals[index]
		if removal.State == "removed" {
			continue
		}
		if !validRemoval(state, id, *removal) {
			return fmt.Errorf("retention path is outside admitted ownership")
		}
		if err = validateOwnedPath(state, removal.Path); err != nil {
			return err
		}
		removal.State = "removing"
		plan.State = "removing"
		if err = r.Store.SaveRetention(ctx, plan, owner); err != nil {
			return err
		}
		if r.retentionHook != nil {
			if err = r.retentionHook(removal.Path, "before"); err != nil {
				return err
			}
		}
		if removal.Kind == "worktree" {
			registered, listErr := r.registeredWorktrees(ctx)
			if listErr != nil {
				return listErr
			}
			for _, path := range registered {
				if path == removal.Path {
					if _, err = r.git(ctx, root, "worktree", "remove", "--force", "--", path); err != nil {
						return err
					}
					break
				}
			}
		} else if err = os.RemoveAll(removal.Path); err != nil {
			return err
		}
		if r.retentionHook != nil {
			if err = r.retentionHook(removal.Path, "after"); err != nil {
				return err
			}
		}
		removal.State = "removed"
		if err = r.Store.SaveRetention(ctx, plan, owner); err != nil {
			return err
		}
	}
	plan.State = "database"
	if err = r.Store.SaveRetention(ctx, plan, owner); err != nil {
		return err
	}
	if r.retentionHook != nil {
		if err = r.retentionHook(id, "database"); err != nil {
			return err
		}
	}
	return r.Store.FinishRetention(ctx, plan, owner)
}

func (r *Runtime) registeredWorktrees(ctx context.Context) ([]string, error) {
	output, err := r.git(ctx, r.Root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, field := range strings.Split(output, "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			paths = append(paths, filepath.Clean(strings.TrimPrefix(field, "worktree ")))
		}
	}
	return paths, nil
}
func inside(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func validRemoval(state, id string, removal workflows.Removal) bool {
	group := filepath.Join(state, "workspaces", id)
	if removal.Kind == "worktree" {
		return inside(group, removal.Path)
	}
	if removal.Kind != "directory" {
		return false
	}
	if removal.Path == group {
		return true
	}
	for _, provider := range []string{"claude", "codex", "commands"} {
		if removal.Path == filepath.Join(state, "providers", provider, id) {
			return true
		}
	}
	parent := filepath.Join(state, "invocations")
	name := filepath.Base(removal.Path)
	return filepath.Dir(removal.Path) == parent && len(name) == 32 && strings.Trim(name, "0123456789abcdef") == ""
}
func validateOwnedPath(state, path string) error {
	if !inside(state, path) || path == state {
		return fmt.Errorf("refusing removal outside engine ownership")
	}
	relative, _ := filepath.Rel(state, path)
	current := state
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed ownership path is a symlink")
		}
	}
	return nil
}
