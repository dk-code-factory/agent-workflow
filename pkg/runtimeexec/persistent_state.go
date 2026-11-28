package runtimeexec

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// PersistentStateMount is resolved by the engine, never from workflow host paths.
// It is launch-only authority and must not be retained in a workflow snapshot.
type PersistentStateMount struct{ Source, Target string }

func pathsOverlap(a, b string) bool {
	inside := func(root, child string) bool {
		rel, err := filepath.Rel(root, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return inside(a, b) || inside(b, a)
}

func ValidatePersistentStateTarget(target string) error {
	if !strings.HasPrefix(target, "/") || target == "/" || path.Clean(target) != target || strings.ContainsAny(target, "\\\x00\r\n") || strings.Contains(target, "${{") {
		return fmt.Errorf("persistent-state mount-target must be a clean absolute container path")
	}
	for _, reserved := range []string{"/workspace", "/state", "/actions", "/engine-lifetime", "/engine-task-state", "/engine-guardian", "/tmp", "/proc", "/sys", "/dev", "/home", "/root"} {
		if pathsOverlap(target, reserved) {
			return fmt.Errorf("persistent-state mount-target overlaps reserved container path %s", reserved)
		}
	}
	return nil
}

// ValidatePersistentStateLocation rejects symlinks in every existing component,
// including ancestors. Missing directories are permitted only before creation.
func ValidatePersistentStateLocation(source string, allowMissing bool, protected ...string) error {
	if !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return fmt.Errorf("persistent-state requires an absolute directory")
	}
	current := source
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("persistent-state requires real directories without symlinks")
			}
		} else if !allowMissing || !os.IsNotExist(err) {
			return fmt.Errorf("persistent-state directory unavailable: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	rel, err := filepath.Rel(source, home)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("host home directories and ancestors cannot be persistent state")
	}
	for _, directory := range protected {
		if directory == "" {
			continue
		}
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return err
		}
		if real, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = real
		} else if !os.IsNotExist(err) {
			return err
		}
		if pathsOverlap(source, absolute) {
			return fmt.Errorf("persistent-state directory must be disjoint from workspace and engine mounts")
		}
	}
	return nil
}

func ValidatePersistentStateMounts(mounts []PersistentStateMount, protected ...string) error {
	for i, mount := range mounts {
		if err := ValidatePersistentStateTarget(mount.Target); err != nil {
			return err
		}
		if err := ValidatePersistentStateLocation(mount.Source, false, protected...); err != nil {
			return err
		}
		for _, previous := range mounts[:i] {
			if pathsOverlap(mount.Source, previous.Source) || pathsOverlap(mount.Target, previous.Target) {
				return fmt.Errorf("persistent-state mounts must be disjoint")
			}
		}
	}
	return nil
}
