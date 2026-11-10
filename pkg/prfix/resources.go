package prfix

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Pin directory entries, file contents, modes and symlink targets. Resource
// symlinks may refer outside the checkout, but cycles and unbounded input block
// preparation instead of silently leaving part of a selected resource unpinned.
func resourceDigest(path string) (string, error) {
	return resourceDigestWithOpen(path, 64<<20, os.Open)
}

// Pin repository instructions for the workspace and every changed-path ancestor.
// Other backend or external guidance remains an explicitly declared resource.
func repositoryInstructions(root string, paths []string) (map[string]string, error) {
	directories := map[string]bool{".": true}
	for _, path := range paths {
		if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("repository instruction scope must be repository-relative")
		}
		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			directories[dir] = true
		}
	}
	names := make([]string, 0, len(directories))
	for dir := range directories {
		names = append(names, filepath.Join(dir, "AGENTS.md"))
	}
	sort.Strings(names)
	resources := map[string]string{}
	for _, name := range names {
		path := filepath.Join(root, name)
		_, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("repository instructions must be a regular file: %s", name)
		}
		hash, err := resourceDigest(path)
		if err != nil {
			return nil, err
		}
		resources[name] = hash
	}
	return resources, nil
}

func resourceDigestWithOpen(path string, maxBytes int64, open func(string) (*os.File, error)) (string, error) {
	count := 0
	var size int64
	visiting := map[string]bool{}
	var walk func(string) (string, error)
	walk = func(path string) (string, error) {
		count++
		if count > 10000 {
			return "", fmt.Errorf("resource exceeds 10000 entries")
		}
		st, e := os.Lstat(path)
		if e != nil {
			return "", e
		}
		target := ""
		if st.Mode()&os.ModeSymlink != 0 {
			target, e = os.Readlink(path)
			if e != nil {
				return "", e
			}
			resolved, e := filepath.EvalSymlinks(path)
			if e != nil {
				return "", e
			}
			if visiting[resolved] {
				return "", fmt.Errorf("cyclic resource symlink")
			}
			hash, e := walk(resolved)
			return digest([]any{st.Mode().String(), target, hash}), e
		}
		if st.IsDir() {
			resolved, e := filepath.Abs(path)
			if e != nil {
				return "", e
			}
			if visiting[resolved] {
				return "", fmt.Errorf("cyclic resource directory")
			}
			visiting[resolved] = true
			defer delete(visiting, resolved)
			entries, e := os.ReadDir(path)
			if e != nil {
				return "", e
			}
			children := map[string]string{}
			for _, entry := range entries {
				hash, e := walk(filepath.Join(path, entry.Name()))
				if e != nil {
					return "", e
				}
				children[entry.Name()] = hash
			}
			return digest([]any{st.Mode().String(), children}), nil
		}
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("resource must be a file or directory")
		}
		remaining := maxBytes - size
		if st.Size() > remaining {
			return "", fmt.Errorf("resource exceeds byte limit (%d bytes)", maxBytes)
		}
		file, e := open(path)
		if e != nil {
			return "", e
		}
		actual, e := file.Stat()
		if e != nil || !actual.Mode().IsRegular() || !os.SameFile(st, actual) {
			_ = file.Close()
			return "", fmt.Errorf("resource changed before reading: %s", path)
		}
		b, e := io.ReadAll(io.LimitReader(file, remaining+1))
		closeErr := file.Close()
		if e != nil {
			return "", e
		}
		if closeErr != nil {
			return "", closeErr
		}
		if int64(len(b)) > remaining {
			return "", fmt.Errorf("resource exceeds byte limit (%d bytes)", maxBytes)
		}
		size += int64(len(b))
		return digest([]any{st.Mode().String(), rawHash(b)}), nil
	}
	return walk(path)
}
