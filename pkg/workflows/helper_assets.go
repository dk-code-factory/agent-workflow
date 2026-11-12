package workflows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

const MaxHelperBytes int64 = 32 << 20
const MaxHelperFiles = 4096

type HelperFile struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type HelperBundle struct {
	OriginID string       `json:"origin_run_id"`
	ID       string       `json:"id"`
	Files    []HelperFile `json:"files"`
}
type assetStageOwner struct {
	OriginID string `json:"origin_run_id"`
	StageID  string `json:"stage_id"`
}

func assetPath(name string) (string, error) {
	if name == "" || strings.Contains(name, "${{") || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) {
		return "", fmt.Errorf("helper path must be a relative file/directory")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("helper path traversal is unsupported")
		}
	}
	return path.Clean(name), nil
}
func digestBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func (s *SQLiteStore) bundleDirectory(b *HelperBundle) (string, error) {
	if b == nil || len(b.OriginID) != 32 || !hexID(b.OriginID) || len(b.ID) != 64 || !hexID(b.ID) {
		return "", fmt.Errorf("invalid helper bundle ownership/identity")
	}
	directory := filepath.Join(s.Dir, "assets", b.OriginID, b.ID)
	if err := s.checkAssetAncestors(filepath.Dir(directory)); err != nil {
		return "", err
	}
	return directory, nil
}
func noSymlinkComponents(root *os.Root, name string) error {
	components := strings.Split(filepath.ToSlash(name), "/")
	current := "."
	for _, part := range components {
		if part == "." || part == "" {
			continue
		}
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("helper symlinks are unsupported: %s", current)
		}
	}
	return nil
}
func writeAssetFile(name string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// CaptureHelperBundle stages only declared regular files. OpenRoot prevents a
// concurrent workspace symlink replacement from escaping the owning package.
func (s *SQLiteStore) CaptureHelperBundle(ctx context.Context, origin, base string, declarations []string) (bundle *HelperBundle, returned error) {
	if len(origin) != 32 || !hexID(origin) {
		return nil, fmt.Errorf("helper capture requires an originating run identity")
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err = s.checkAssetAncestors(filepath.Join(s.Dir, "assets", "staging")); err != nil {
		return nil, err
	}
	stageID := newID()
	stage := filepath.Join(s.Dir, "assets", "staging", stageID)
	if err = os.MkdirAll(stage, 0700); err != nil {
		return nil, err
	}
	defer func() {
		if returned != nil {
			_ = os.RemoveAll(stage)
		}
	}()
	marker, _ := json.Marshal(assetStageOwner{OriginID: origin, StageID: stageID})
	if err = writeAssetFile(filepath.Join(stage, "owner.json"), marker, 0400); err != nil {
		return nil, err
	}
	if err = os.Mkdir(filepath.Join(stage, "files"), 0755); err != nil {
		return nil, err
	}
	files := map[string]HelperFile{}
	var total int64
	capture := func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := noSymlinkComponents(root, name); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("helper must be a regular file: %s", name)
		}
		if info.Size() > MaxHelperBytes {
			return fmt.Errorf("helper bundle exceeds 32 MiB")
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if !os.SameFile(info, opened) {
			f.Close()
			return fmt.Errorf("helper changed during capture: %s", name)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, MaxHelperBytes+1))
		after, statErr := f.Stat()
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if statErr != nil {
			return statErr
		}
		if closeErr != nil {
			return closeErr
		}
		if int64(len(data)) > MaxHelperBytes || int64(len(data)) != info.Size() || !info.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("helper changed or exceeds capture limits: %s", name)
		}
		if err = noSymlinkComponents(root, name); err != nil {
			return err
		}
		mode := uint32(0444)
		if info.Mode()&0111 != 0 {
			mode = 0555
		}
		entry := HelperFile{Path: filepath.ToSlash(name), Mode: mode, Size: int64(len(data)), SHA256: digestBytes(data)}
		if old, exists := files[entry.Path]; exists {
			if old != entry {
				return fmt.Errorf("conflicting helper destination %s", entry.Path)
			}
			return nil
		}
		if len(files) >= MaxHelperFiles || total+entry.Size > MaxHelperBytes {
			return fmt.Errorf("helper bundle exceeds 4096 files or 32 MiB")
		}
		if err = writeAssetFile(filepath.Join(stage, "files", filepath.FromSlash(entry.Path)), data, fs.FileMode(mode)); err != nil {
			return err
		}
		files[entry.Path] = entry
		total += entry.Size
		return nil
	}
	for _, declaration := range declarations {
		name, err := assetPath(declaration)
		if err != nil {
			return nil, err
		}
		if err = noSymlinkComponents(root, name); err != nil {
			return nil, err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			err = fs.WalkDir(root.FS(), name, func(name string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("helper symlinks are unsupported: %s", name)
				}
				if entry.IsDir() {
					return nil
				}
				return capture(name)
			})
		} else {
			err = capture(name)
		}
		if err != nil {
			return nil, err
		}
	}
	bundle = &HelperBundle{OriginID: origin, Files: make([]HelperFile, 0, len(files))}
	for _, entry := range files {
		bundle.Files = append(bundle.Files, entry)
	}
	sort.Slice(bundle.Files, func(i, j int) bool { return bundle.Files[i].Path < bundle.Files[j].Path })
	bundle.ID = actionHash(bundle.Files)
	manifest, _ := json.Marshal(bundle)
	if err = writeAssetFile(filepath.Join(stage, "bundle.json"), manifest, 0400); err != nil {
		return nil, err
	}
	// Verify the staged bytes, then fsync directory entries before final rename.
	if err = verifyBundleDirectory(stage, bundle); err != nil {
		return nil, err
	}
	err = filepath.WalkDir(stage, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		dir, err := os.Open(name)
		if err != nil {
			return err
		}
		err = dir.Sync()
		_ = dir.Close()
		return err
	})
	if err != nil {
		return nil, err
	}
	destination, err := s.bundleDirectory(bundle)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return nil, err
	}
	if _, err = os.Lstat(destination); err == nil {
		if err = verifyBundleDirectory(destination, bundle); err != nil {
			return nil, err
		}
		_ = os.RemoveAll(stage)
		return bundle, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err = os.Rename(stage, destination); err != nil {
		return nil, err
	}
	for parent := filepath.Dir(destination); ; parent = filepath.Dir(parent) {
		directory, err := os.Open(parent)
		if err != nil {
			return nil, err
		}
		err = directory.Sync()
		_ = directory.Close()
		if err != nil {
			return nil, err
		}
		if parent == filepath.Clean(s.Dir) {
			break
		}
		if filepath.Dir(parent) == parent {
			return nil, fmt.Errorf("asset durability path escaped owned state")
		}
	}
	return bundle, nil
}
func verifyBundleDirectory(directory string, bundle *HelperBundle) error {
	if len(bundle.Files) > MaxHelperFiles || bundle.ID != actionHash(bundle.Files) {
		return fmt.Errorf("helper manifest identity mismatch")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("helper asset directory is not owned regular storage")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = noSymlinkComponents(root, "bundle.json"); err != nil {
		return err
	}
	raw, err := root.ReadFile("bundle.json")
	if err != nil {
		return err
	}
	expected, _ := json.Marshal(bundle)
	if !bytes.Equal(raw, expected) {
		return fmt.Errorf("retained helper manifest differs from admitted manifest")
	}
	expectedFiles := map[string]HelperFile{}
	var total int64
	for _, entry := range bundle.Files {
		name, err := assetPath(entry.Path)
		if err != nil || name != entry.Path || name == "." || entry.Size < 0 || entry.Mode != 0444 && entry.Mode != 0555 || len(entry.SHA256) != 64 || !hexID(entry.SHA256) {
			return fmt.Errorf("invalid retained helper file metadata")
		}
		if _, duplicate := expectedFiles[name]; duplicate {
			return fmt.Errorf("duplicate retained helper destination")
		}
		total += entry.Size
		if total > MaxHelperBytes {
			return fmt.Errorf("retained helper bundle exceeds 32 MiB")
		}
		expectedFiles[name] = entry
		if err = noSymlinkComponents(root, path.Join("files", name)); err != nil {
			return err
		}
		f, err := root.Open(path.Join("files", name))
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != fs.FileMode(entry.Mode) || info.Size() != entry.Size {
			f.Close()
			return fmt.Errorf("retained helper mode/size mismatch: %s", name)
		}
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(f, entry.Size+1))
		_ = f.Close()
		if err != nil || n != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("retained helper checksum mismatch: %s", name)
		}
	}
	return fs.WalkDir(root.FS(), "files", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("retained helper symlink detected")
		}
		if entry.IsDir() {
			return nil
		}
		if _, ok := expectedFiles[strings.TrimPrefix(name, "files/")]; !ok {
			return fmt.Errorf("undeclared retained helper file: %s", name)
		}
		return nil
	})
}
func (s *SQLiteStore) VerifyHelperBundle(bundle *HelperBundle) error {
	directory, err := s.bundleDirectory(bundle)
	if err != nil {
		return err
	}
	if err = verifyBundleDirectory(directory, bundle); err != nil {
		return fmt.Errorf("retained helper assets unavailable: %w", err)
	}
	return nil
}
func (s *SQLiteStore) HelperBundlePath(bundle *HelperBundle) (string, error) {
	if err := s.VerifyHelperBundle(bundle); err != nil {
		return "", err
	}
	directory, _ := s.bundleDirectory(bundle)
	return filepath.Join(directory, "files"), nil
}
func helperBundles(all map[string]*Workflow) map[string]*HelperBundle {
	out := map[string]*HelperBundle{}
	add := func(p *ContainerImplementation) {
		if p != nil && p.Bundle != nil {
			out[p.Bundle.OriginID+"/"+p.Bundle.ID] = p.Bundle
		}
	}
	for _, w := range all {
		for _, j := range w.Jobs {
			if j.Container != nil {
				add(j.Container.Implementation)
			}
			for _, s := range lifecycleSteps(j.Steps) {
				add(s.ContainerImplementation)
			}
		}
	}
	return out
}
func (s *SQLiteStore) linkHelperAssets(ctx context.Context, tx *sql.Tx, r *Run, snap Snapshot) error {
	origin := originID(r, snap)
	for _, bundle := range helperBundles(snap.Definitions) {
		if bundle.OriginID != origin {
			return fmt.Errorf("helper bundle originating owner changed")
		}
		if err := s.VerifyHelperBundle(bundle); err != nil {
			return err
		}
		raw, _ := json.Marshal(bundle)
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO helper_assets(origin_id,id,manifest) VALUES(?,?,?)", origin, bundle.ID, raw); err != nil {
			return err
		}
		var retained []byte
		if err := tx.QueryRowContext(ctx, "SELECT manifest FROM helper_assets WHERE origin_id=? AND id=?", origin, bundle.ID).Scan(&retained); err != nil {
			return err
		}
		if !bytes.Equal(raw, retained) {
			return fmt.Errorf("immutable helper asset manifest changed")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO run_helper_assets(run_id,asset_origin,asset_id) VALUES(?,?,?)", r.ID, origin, bundle.ID); err != nil {
			return err
		}
	}
	return nil
}
func (s *SQLiteStore) verifyRunHelperAssets(ctx context.Context, run string, snap Snapshot) error {
	for _, bundle := range helperBundles(snap.Definitions) {
		var raw []byte
		if err := s.db.QueryRowContext(ctx, `SELECT a.manifest FROM helper_assets a JOIN run_helper_assets r ON r.asset_origin=a.origin_id AND r.asset_id=a.id WHERE r.run_id=? AND a.origin_id=? AND a.id=?`, run, bundle.OriginID, bundle.ID).Scan(&raw); err != nil {
			return fmt.Errorf("retained helper asset reference missing: %w", err)
		}
		expected, _ := json.Marshal(bundle)
		if !bytes.Equal(raw, expected) {
			return fmt.Errorf("retained helper asset reference changed")
		}
		// Reusable children admit the same frozen closure and must retain its references too.
		var missing string
		err := s.db.QueryRowContext(ctx, `SELECT r.id FROM runs r WHERE (r.id=? OR r.origin_id=?) AND NOT EXISTS (SELECT 1 FROM run_helper_assets a WHERE a.run_id=r.id AND a.asset_origin=? AND a.asset_id=?) LIMIT 1`, run, run, bundle.OriginID, bundle.ID).Scan(&missing)
		if err == nil {
			return fmt.Errorf("retained helper asset reference missing for run %s", missing)
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err := s.VerifyHelperBundle(bundle); err != nil {
			return err
		}
	}
	return nil
}

// CleanupHelperStaging is a recovery operation before accepting admissions.
// It requires exclusive workspace ownership and removes only marked staging or
// unreferenced final bundle directories; another origin's committed assets stay.
func (s *SQLiteStore) CleanupHelperStaging(ctx context.Context, owner *ownership.Lock) error {
	if !owner.Holds(s.Dir) {
		return fmt.Errorf("helper staging cleanup requires exclusive ownership")
	}
	assets := filepath.Join(s.Dir, "assets")
	if err := s.checkAssetAncestors(assets); err != nil {
		return err
	}
	entries, err := os.ReadDir(assets)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, group := range entries {
		if group.Type()&os.ModeSymlink != 0 || !group.IsDir() {
			continue
		}
		name := group.Name()
		if name != "staging" && (len(name) != 32 || !hexID(name)) {
			continue
		}
		groupPath := filepath.Join(assets, name)
		children, err := os.ReadDir(groupPath)
		if err != nil {
			return err
		}
		for _, entry := range children {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				continue
			}
			directory := filepath.Join(groupPath, entry.Name())
			raw, err := os.ReadFile(filepath.Join(directory, "owner.json"))
			if err != nil {
				continue
			}
			var marker assetStageOwner
			if len(raw) > 4096 || json.Unmarshal(raw, &marker) != nil || len(marker.StageID) != 32 || !hexID(marker.StageID) || len(marker.OriginID) != 32 || !hexID(marker.OriginID) {
				continue
			}
			if name == "staging" {
				if entry.Name() != marker.StageID {
					continue
				}
			} else {
				if name != marker.OriginID || len(entry.Name()) != 64 || !hexID(entry.Name()) {
					continue
				}
				var n int
				if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM helper_assets WHERE origin_id=? AND id=?", name, entry.Name()).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					continue
				}
			}
			if err = os.RemoveAll(directory); err != nil {
				return err
			}
		}
	}
	return nil
}

func containerStateOrigin(origin, fallback string) string {
	if origin != "" {
		return origin
	}
	return fallback
}
func (e *Executor) pinHelperBundles(ctx context.Context, all map[string]*Workflow) error {
	store := invocationStore(e.Store)
	for ref, w := range all {
		var err error
		capture := func(declarations []string, p *ContainerImplementation) error {
			if p == nil {
				return fmt.Errorf("helper files require an admitted container")
			}
			if store == nil {
				return fmt.Errorf("helper files require durable owned storage")
			}
			if p.Bundle != nil {
				return store.VerifyHelperBundle(p.Bundle)
			}
			resolved, err := (Resolver{WorkspaceDir: e.WorkspaceDir, DefinitionsDir: e.DefinitionsDir}).ResolveLocal(ref)
			if err != nil {
				return err
			}
			bundle, err := store.CaptureHelperBundle(ctx, e.bundleOrigin, filepath.Dir(resolved.Path), declarations)
			if err != nil {
				return err
			}
			p.Bundle = bundle
			return nil
		}
		for id, j := range w.Jobs {
			if j.Container != nil {
				p := j.Container.Implementation
				if p != nil && p.Bundle != nil || j.Container.Files != nil {
					if err = capture(j.Container.Files, p); err != nil {
						return fmt.Errorf("%s job %s: %w", ref, id, err)
					}
				}
			}
			j.Steps, err = mapSteps(j.Steps, "", func(step Step) (Step, error) {
				if !isDockerStep(step) {
					return step, nil
				}
				p := step.ContainerImplementation
				if p != nil && p.Bundle != nil || step.Files != nil {
					if err := capture(step.Files, p); err != nil {
						return step, err
					}
				} else if p != nil && j.Container != nil && j.Container.Implementation != nil {
					p.Bundle = j.Container.Implementation.Bundle
				}
				return step, nil
			})
			if err != nil {
				return err
			}
			w.Jobs[id] = j
		}
	}
	return nil
}

func (s *SQLiteStore) checkAssetAncestors(directory string) error {
	relative, err := filepath.Rel(s.Dir, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid asset directory")
	}
	current := s.Dir
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("helper storage ancestor is not an owned directory")
		}
	}
	return nil
}
