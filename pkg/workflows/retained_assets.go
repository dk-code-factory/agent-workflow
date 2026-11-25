package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const AssetBundleVersion = 1

// AssetInput is already captured source data. Mode contains the original
// permission bits, plus fs.ModeDir for a directory. Directories have no Data.
// CaptureAssetBundle never reopens an installation or another source path.
type AssetInput struct {
	Path string
	Mode fs.FileMode
	Data []byte
}

// AssetEntry retains original source metadata separately from materialized
// permissions. Files are stored as 0444, or 0555 when any execute bit was set;
// all materialized directories are 0555, including inferred parents.
type AssetEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// AssetBundle is a versioned retained tree. Its identity includes original
// modes, declared directories and exact file content, but excludes the owner.
// It is separate from the historical HelperBundle format and identity scheme.
type AssetBundle struct {
	Version  int          `json:"version"`
	OriginID string       `json:"origin_run_id"`
	ID       string       `json:"id"`
	Entries  []AssetEntry `json:"entries"`
}

func assetBundleID(entries []AssetEntry) string {
	return actionHash(struct {
		Version int          `json:"version"`
		Entries []AssetEntry `json:"entries"`
	}{AssetBundleVersion, entries})
}

func assetDirectories(entries []AssetEntry) (map[string]bool, error) {
	if len(entries) > MaxHelperFiles {
		return nil, fmt.Errorf("retained assets exceed 4096 entries")
	}
	directories := map[string]bool{".": true}
	byPath := make(map[string]AssetEntry, len(entries))
	var total int64
	previous := ""
	for _, entry := range entries {
		name, err := assetPath(entry.Path)
		if err != nil || name == "." || name != entry.Path || !utf8.ValidString(name) || len(name) > 4096 || entry.Mode > 0777 || entry.Path <= previous {
			return nil, fmt.Errorf("invalid or unsorted retained asset path/mode %q", entry.Path)
		}
		previous = entry.Path
		switch entry.Kind {
		case "directory":
			if entry.Size != 0 || entry.SHA256 != "" {
				return nil, fmt.Errorf("retained directory has file metadata: %s", name)
			}
			directories[name] = true
		case "file":
			if entry.Size < 0 || entry.Size > MaxHelperBytes-total || len(entry.SHA256) != 64 || !hexID(entry.SHA256) {
				return nil, fmt.Errorf("invalid retained asset file metadata or byte limit: %s", name)
			}
			total += entry.Size
		default:
			return nil, fmt.Errorf("unsupported retained asset kind %q", entry.Kind)
		}
		byPath[name] = entry
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			directories[parent] = true
		}
	}
	for name := range directories {
		if entry, exists := byPath[name]; exists && entry.Kind != "directory" {
			return nil, fmt.Errorf("retained asset file/directory collision: %s", name)
		}
	}
	return directories, nil
}

func materializedAssetMode(entry AssetEntry) fs.FileMode {
	if entry.Kind == "directory" || entry.Mode&0111 != 0 {
		return 0555
	}
	return 0444
}

func (s *SQLiteStore) assetBundleDirectory(bundle *AssetBundle) (string, error) {
	if bundle == nil {
		return "", fmt.Errorf("missing retained asset bundle")
	}
	return s.bundleDirectory(&HelperBundle{OriginID: bundle.OriginID, ID: bundle.ID})
}

// CaptureAssetBundle writes only the supplied in-memory entries into owned
// immutable storage. Duplicate paths, special files and file/parent collisions
// are rejected before staging. The returned manifest must later be linked by
// durable admission; until then, normal owned orphan cleanup may remove it.
func (s *SQLiteStore) CaptureAssetBundle(ctx context.Context, origin string, inputs []AssetInput) (bundle *AssetBundle, returned error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(origin) != 32 || !hexID(origin) || len(inputs) > MaxHelperFiles {
		return nil, fmt.Errorf("retained asset capture requires an originating identity and at most 4096 entries")
	}
	bundle = &AssetBundle{Version: AssetBundleVersion, OriginID: origin, Entries: make([]AssetEntry, 0, len(inputs))}
	data := make(map[string][]byte, len(inputs))
	var total int64
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if input.Mode & ^(fs.ModeDir|0777) != 0 || int64(len(input.Data)) > MaxHelperBytes-total {
			return nil, fmt.Errorf("unsupported source mode or retained asset byte limit: %s", input.Path)
		}
		total += int64(len(input.Data))
		entry := AssetEntry{Path: input.Path, Kind: "file", Mode: uint32(input.Mode.Perm()), Size: int64(len(input.Data))}
		if input.Mode.IsDir() {
			if len(input.Data) != 0 {
				return nil, fmt.Errorf("retained directory has source bytes: %s", input.Path)
			}
			entry.Kind = "directory"
		} else {
			data[input.Path] = bytes.Clone(input.Data)
			entry.SHA256 = digestBytes(data[input.Path])
		}
		bundle.Entries = append(bundle.Entries, entry)
	}
	sort.Slice(bundle.Entries, func(i, j int) bool { return bundle.Entries[i].Path < bundle.Entries[j].Path })
	directories, err := assetDirectories(bundle.Entries)
	if err != nil {
		return nil, err
	}
	bundle.ID = assetBundleID(bundle.Entries)
	destination, err := s.assetBundleDirectory(bundle)
	if err != nil {
		return nil, err
	}
	if err = s.checkAssetAncestors(filepath.Join(s.Dir, "assets", "staging")); err != nil {
		return nil, err
	}
	stageID := newID()
	stage := filepath.Join(s.Dir, "assets", "staging", stageID)
	if err = os.MkdirAll(filepath.Dir(stage), 0700); err != nil {
		return nil, err
	}
	if err = os.Mkdir(stage, 0700); err != nil {
		return nil, err
	}
	stageInfo, err := os.Lstat(stage)
	if err != nil {
		return nil, err
	}
	defer func() {
		if returned != nil {
			returned = errors.Join(returned, removeOwnedAssetDirectory(stage, stageInfo))
		}
	}()
	marker, _ := json.Marshal(assetStageOwner{OriginID: origin, StageID: stageID})
	if err = writeAssetFile(filepath.Join(stage, "owner.json"), marker, 0400); err != nil {
		return nil, err
	}
	for name := range directories {
		if err = os.MkdirAll(filepath.Join(stage, "files", filepath.FromSlash(name)), 0755); err != nil {
			return nil, err
		}
	}
	for _, entry := range bundle.Entries {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Kind == "file" {
			if err = writeAssetFile(filepath.Join(stage, "files", filepath.FromSlash(entry.Path)), data[entry.Path], materializedAssetMode(entry)); err != nil {
				return nil, err
			}
		}
	}
	manifest, _ := json.Marshal(bundle)
	if err = writeAssetFile(filepath.Join(stage, "bundle.json"), manifest, 0400); err != nil {
		return nil, err
	}
	for name := range directories {
		if err = os.Chmod(filepath.Join(stage, "files", filepath.FromSlash(name)), 0555); err != nil {
			return nil, err
		}
	}
	if err = verifyAssetBundleDirectory(stage, bundle); err != nil {
		return nil, err
	}
	if err = syncAssetDirectories(stage); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return nil, err
	}
	if _, err = os.Lstat(destination); err == nil {
		if err = verifyAssetBundleDirectory(destination, bundle); err != nil {
			return nil, err
		}
		if err = removeOwnedAssetDirectory(stage, stageInfo); err != nil {
			return nil, err
		}
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
		err = errors.Join(directory.Sync(), directory.Close())
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

func verifyAssetBundleDirectory(directory string, bundle *AssetBundle) error {
	if bundle.Version != AssetBundleVersion || bundle.ID != assetBundleID(bundle.Entries) {
		return fmt.Errorf("retained asset manifest identity mismatch")
	}
	directories, err := assetDirectories(bundle.Entries)
	if err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("retained asset storage is not an owned directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range []string{"owner.json", "bundle.json", "files"} {
		if err = noSymlinkComponents(root, name); err != nil {
			return err
		}
	}
	marker, err := readRegularAsset(root, "owner.json", 4097)
	var owner assetStageOwner
	if err != nil || len(marker) > 4096 || json.Unmarshal(marker, &owner) != nil || owner.OriginID != bundle.OriginID || len(owner.StageID) != 32 || !hexID(owner.StageID) {
		return fmt.Errorf("retained asset ownership marker unavailable or invalid")
	}
	expected, _ := json.Marshal(bundle)
	raw, err := readRegularAsset(root, "bundle.json", int64(len(expected))+1)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, expected) {
		return fmt.Errorf("retained asset manifest differs from admitted manifest")
	}
	files := make(map[string]AssetEntry, len(bundle.Entries))
	for name := range directories {
		if err = noSymlinkComponents(root, path.Join("files", name)); err != nil {
			return err
		}
		info, err := root.Lstat(path.Join("files", name))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm() != 0555 {
			return fmt.Errorf("retained asset directory mode/type mismatch: %s", name)
		}
	}
	for _, entry := range bundle.Entries {
		if entry.Kind == "directory" {
			continue
		}
		files[entry.Path] = entry
		name := path.Join("files", entry.Path)
		if err = noSymlinkComponents(root, name); err != nil {
			return err
		}
		file, err := openRegularAsset(root, name)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		if statErr != nil || info.Size() != entry.Size || info.Mode().Perm() != materializedAssetMode(entry) {
			file.Close()
			return fmt.Errorf("retained asset mode/size mismatch: %s", entry.Path)
		}
		contents, readErr := io.ReadAll(io.LimitReader(file, entry.Size+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(contents)) != entry.Size || digestBytes(contents) != entry.SHA256 {
			return fmt.Errorf("retained asset checksum mismatch: %s", entry.Path)
		}
	}
	return fs.WalkDir(root.FS(), "files", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "files" {
			name = "."
		} else {
			name = strings.TrimPrefix(name, "files/")
		}
		if entry.IsDir() && directories[name] {
			return nil
		}
		if _, exists := files[name]; !entry.IsDir() && entry.Type().IsRegular() && exists {
			return nil
		}
		return fmt.Errorf("undeclared or unsafe retained asset: %s", name)
	})
}

// VerifyAssetBundle validates both the retained manifest and its complete tree.
func (s *SQLiteStore) VerifyAssetBundle(bundle *AssetBundle) error {
	directory, err := s.assetBundleDirectory(bundle)
	if err != nil {
		return err
	}
	return verifyAssetBundleDirectory(directory, bundle)
}

// AssetBundlePath returns only a verified engine-owned materialization.
func (s *SQLiteStore) AssetBundlePath(bundle *AssetBundle) (string, error) {
	if err := s.VerifyAssetBundle(bundle); err != nil {
		return "", err
	}
	directory, _ := s.assetBundleDirectory(bundle)
	return filepath.Join(directory, "files"), nil
}

func syncAssetDirectories(directory string) error {
	return filepath.WalkDir(directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.IsDir() {
			return walkErr
		}
		dir, err := os.Open(name)
		if err != nil {
			return err
		}
		return errors.Join(dir.Sync(), dir.Close())
	})
}

// Call only for a newly created private stage, or after the recovery cleanup's
// ownership marker and database-reference checks. Never follow directory links.
func removeOwnedAssetDirectory(directory string, expected fs.FileInfo) error {
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if expected == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, expected) {
		return fmt.Errorf("owned asset cleanup directory identity changed")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, expected) {
		return fmt.Errorf("owned asset cleanup directory changed while opening")
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.IsDir() {
			return walkErr
		}
		before, err := entry.Info()
		if err != nil {
			return err
		}
		dir, err := openAssetFile(root, name)
		if err != nil {
			return err
		}
		info, statErr := dir.Stat()
		if statErr != nil || !info.IsDir() || !os.SameFile(info, before) {
			dir.Close()
			return fmt.Errorf("owned asset cleanup child directory changed")
		}
		return errors.Join(dir.Chmod(0700), dir.Close())
	})
	if err != nil {
		return err
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	current, err := os.Lstat(directory)
	if err != nil || !os.SameFile(current, expected) {
		return fmt.Errorf("owned asset cleanup directory changed before removal")
	}
	// Remove only the empty outer directory by pathname; recursive removal is
	// anchored to the original descriptor above, even if that path is replaced.
	return os.Remove(directory)
}
