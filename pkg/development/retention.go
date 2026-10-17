package development

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Cleanup requires the caller to hold exclusive workspace ownership. Filesystem
// removal precedes record deletion, so a failed removal retains its audit trail.
func (r *Runtime) Cleanup(ctx context.Context, id string) error {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return fmt.Errorf("invalid run id")
	}
	if _, err := r.Store.OriginRuns(ctx, id); err != nil {
		return err
	}
	root, err := r.canonicalRoot()
	if err != nil {
		return err
	}
	group := filepath.Join(root, ".agent-workflow", "workspaces", id)
	if _, err := os.Lstat(group); os.IsNotExist(err) {
		return r.Store.DeleteOrigin(ctx, id)
	} else if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(group)
	if err != nil {
		return err
	}
	if real != group {
		return fmt.Errorf("managed workspace ownership path is a symlink")
	}
	list, err := r.git(ctx, r.Root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	for _, field := range strings.Split(list, "\x00") {
		if !strings.HasPrefix(field, "worktree ") {
			continue
		}
		path := strings.TrimPrefix(field, "worktree ")
		relative, err := filepath.Rel(group, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if path == r.Root {
			return fmt.Errorf("refusing caller workspace removal")
		}
		if _, err = r.git(ctx, r.Root, "worktree", "remove", "--force", "--", path); err != nil {
			return err
		}
	}
	if err = os.RemoveAll(group); err != nil {
		return err
	}
	return r.Store.DeleteOrigin(ctx, id)
}
