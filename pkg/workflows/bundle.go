package workflows

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

// RecoveryBundle exports only engine-owned state. Provider-owned authentication
// stores stay outside the bundle, including engine state links to native auth.
func (s *SQLiteStore) RecoveryBundle(ctx context.Context, destination string, owner *ownership.Lock) error {
	if !owner.Holds(s.Dir) {
		return fmt.Errorf("recovery bundle requires exclusive workspace ownership")
	}
	if err := s.CheckInvocationCleanup(ctx, ""); err != nil {
		return err
	}
	dbCopy := filepath.Join(s.Dir, "bundle-database-"+newID()+".db")
	if err := s.Backup(ctx, dbCopy, owner); err != nil {
		return err
	}
	defer os.Remove(dbCopy)
	destination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	for _, part := range []string{"workspaces", "providers", "invocations"} {
		root, err := filepath.Abs(filepath.Join(s.Dir, part))
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, destination)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("bundle destination cannot be inside exported state")
		}
	}
	if _, err = os.Lstat(destination); err == nil {
		return fmt.Errorf("bundle destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".workflow-bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	gzipWriter := gzip.NewWriter(file)
	archive := tar.NewWriter(gzipWriter)
	manifest, _ := json.Marshal(map[string]any{"format": 1, "scope": "engine-owned recovery state", "schema": schemaVersion, "workspace_state_path": s.Dir, "native_credentials_included": false, "restore": "restore files at matching managed paths, then restore state.db through the CLI; caller Git administration is not included"})
	if err = archive.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err = archive.Write(manifest); err != nil {
		return err
	}
	add := func(path, name string, info fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		if err = archive.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		_, err = io.CopyN(archive, input, info.Size())
		return err
	}
	info, err := os.Stat(dbCopy)
	if err != nil {
		return err
	}
	if err = add(dbCopy, "state.db", info); err != nil {
		return err
	}
	for _, directory := range []string{"workspaces", "providers", "invocations"} {
		root := filepath.Join(s.Dir, directory)
		if _, err = os.Lstat(root); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(s.Dir, path)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(relative)
			if entry.Type()&os.ModeSymlink != 0 {
				if directory != "workspaces" {
					return nil
				}
				link, err := os.Readlink(path)
				if err != nil {
					return err
				}
				target := link
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(path), target)
				}
				rel, err := filepath.Rel(root, filepath.Clean(target))
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return nil
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				header, err := tar.FileInfoHeader(info, link)
				if err != nil {
					return err
				}
				header.Name = name
				return archive.WriteHeader(header)
			} // never traverse or export provider-owned references
			if directory == "providers" {
				parts := strings.Split(name, "/")
				if len(parts) >= 3 && (len(parts[2]) != 32 || strings.Trim(parts[2], "0123456789abcdef") != "") {
					if entry.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if entry.Name() == "auth.json" || entry.Name() == ".credentials.json" || entry.Name() == "credentials.json" {
					return nil
				}
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() && !info.IsDir() {
				return nil
			}
			return add(path, name, info)
		})
		if err != nil {
			return err
		}
	}
	if err = archive.Close(); err != nil {
		return err
	}
	if err = gzipWriter.Close(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Link(file.Name(), destination); err != nil {
		return err
	}
	return syncFileAndDirectory(destination)
}
