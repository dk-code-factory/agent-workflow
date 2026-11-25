package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

func retainedAssetStore(t *testing.T) *SQLiteStore {
	t.Helper()
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
	default:
		t.Skip("retained asset verification requires a supported Unix host")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Fixtures can contain deliberately corrupted, read-only directories. Never
	// follow their links when preparing this disposable tree for test cleanup.
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	store, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func retainedAssetInputs() []AssetInput {
	return []AssetInput{
		{Path: "nested/script.sh", Mode: 0751, Data: []byte("#!/bin/sh\necho captured\n")},
		{Path: "empty", Mode: fs.ModeDir | 0710},
		{Path: "data/raw.bin", Mode: 0640, Data: []byte{0, 255, 10}},
		{Path: "nested", Mode: fs.ModeDir | 0750},
		{Path: "files", Mode: 0644, Data: []byte("a source file named files")},
	}
}

func TestAssetBundleRetainsMemoryAndOriginalMetadata(t *testing.T) {
	store := retainedAssetStore(t)
	inputs := retainedAssetInputs()
	bundle, err := store.CaptureAssetBundle(t.Context(), randomID(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	inputs[0].Data[0] = 'x'
	root, err := store.AssetBundlePath(bundle)
	if err != nil {
		t.Fatal(err)
	}
	wantInputs := retainedAssetInputs()
	slices.SortFunc(wantInputs, func(a, b AssetInput) int { return bytes.Compare([]byte(a.Path), []byte(b.Path)) })
	if bundle.Version != 1 || len(bundle.Entries) != len(wantInputs) {
		t.Fatalf("changed retained manifest: %+v", bundle)
	}
	for i, input := range wantInputs {
		entry := bundle.Entries[i]
		if entry.Path != input.Path || entry.Mode != uint32(input.Mode.Perm()) {
			t.Fatalf("lost source metadata: %+v", entry)
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(input.Path)))
		if err != nil || info.Mode().Perm() != materializedAssetMode(entry) {
			t.Fatalf("materialized permissions: %+v, %v", info, err)
		}
		if input.Mode.IsDir() {
			if !info.IsDir() || entry.Kind != "directory" || entry.Size != 0 || entry.SHA256 != "" {
				t.Fatalf("lost empty directory: %+v", entry)
			}
		} else {
			got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(input.Path)))
			if err != nil || !bytes.Equal(got, input.Data) || entry.Size != int64(len(input.Data)) || entry.SHA256 != digestBytes(input.Data) {
				t.Fatalf("changed retained bytes: %q, %v", got, err)
			}
		}
	}
	encoded, _ := json.Marshal(bundle)
	var retained AssetBundle
	if err := json.Unmarshal(encoded, &retained); err != nil {
		t.Fatal(err)
	}
	// No source installation or transient input map is required to reopen it.
	reopened := &SQLiteStore{Dir: store.Dir}
	if err := reopened.VerifyAssetBundle(&retained); err != nil {
		t.Fatal(err)
	}
	again, err := store.CaptureAssetBundle(t.Context(), bundle.OriginID, wantInputs)
	if err != nil || !reflect.DeepEqual(bundle, again) {
		t.Fatalf("same bytes/modes changed identity: %+v, %v", again, err)
	}
	other, err := store.CaptureAssetBundle(t.Context(), randomID(), wantInputs)
	if err != nil || other.ID != bundle.ID || other.OriginID == bundle.OriginID {
		t.Fatalf("lost per-origin ownership: %+v, %v", other, err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Dir, "assets/staging"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("retained duplicate staging: %v, %v", entries, err)
	}
}

func TestAssetBundleRejectsUnsafeInputsBeforeStaging(t *testing.T) {
	cases := map[string][]AssetInput{}
	for _, name := range []string{"", ".", "../outside", "/absolute", "a/../b", "./a", "a//b", "a/", "a\\b", "${{ input }}", "a\x00", "a\xff"} {
		cases[fmt.Sprintf("path-%q", name)] = []AssetInput{{Path: name, Mode: 0644}}
	}
	cases["duplicate"] = []AssetInput{{Path: "x"}, {Path: "x"}}
	cases["file/directory collision"] = []AssetInput{{Path: "x"}, {Path: "x", Mode: fs.ModeDir}}
	cases["file ancestor"] = []AssetInput{{Path: "x"}, {Path: "x/y"}}
	cases["special file"] = []AssetInput{{Path: "link", Mode: fs.ModeSymlink}}
	cases["special permissions"] = []AssetInput{{Path: "suid", Mode: fs.ModeSetuid | 0755}}
	cases["directory bytes"] = []AssetInput{{Path: "dir", Mode: fs.ModeDir, Data: []byte("not a directory")}}
	cases["entry limit"] = make([]AssetInput, MaxHelperFiles+1)
	cases["byte limit"] = []AssetInput{{Path: "large", Data: make([]byte, MaxHelperBytes+1)}}
	for name, inputs := range cases {
		t.Run(name, func(t *testing.T) {
			store := retainedAssetStore(t)
			if bundle, err := store.CaptureAssetBundle(t.Context(), randomID(), inputs); err == nil || bundle != nil {
				t.Fatalf("accepted invalid assets: %+v, %v", bundle, err)
			}
			if _, err := os.Lstat(filepath.Join(store.Dir, "assets")); !os.IsNotExist(err) {
				t.Fatalf("invalid input created storage: %v", err)
			}
		})
	}
	store := retainedAssetStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.CaptureAssetBundle(ctx, randomID(), retainedAssetInputs()); err != context.Canceled {
		t.Fatalf("canceled capture: %v", err)
	}
	if _, err := store.CaptureAssetBundle(t.Context(), "../escape", retainedAssetInputs()); err == nil {
		t.Fatal("accepted invalid owner")
	}
}

func TestAssetBundleRejectsRetainedCorruption(t *testing.T) {
	for _, kind := range []string{"missing file", "missing empty directory", "changed bytes", "changed file mode", "changed directory mode", "extra file", "extra directory", "symlink file", "symlink directory", "manifest", "owner"} {
		t.Run(kind, func(t *testing.T) {
			store := retainedAssetStore(t)
			bundle, err := store.CaptureAssetBundle(t.Context(), randomID(), retainedAssetInputs())
			if err != nil {
				t.Fatal(err)
			}
			root, _ := store.AssetBundlePath(bundle)
			directory, _ := store.assetBundleDirectory(bundle)
			file := filepath.Join(root, "nested/script.sh")
			target := t.TempDir()
			putHelper(t, target, "outside", "untouched", 0644)
			for _, dir := range []string{root, filepath.Join(root, "nested")} {
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "missing file", "symlink file":
				err = os.Remove(file)
				if err == nil && kind == "symlink file" {
					err = os.Symlink(filepath.Join(target, "outside"), file)
				}
			case "missing empty directory", "symlink directory":
				err = os.Remove(filepath.Join(root, "empty"))
				if err == nil && kind == "symlink directory" {
					err = os.Symlink(target, filepath.Join(root, "empty"))
				}
			case "changed bytes":
				err = os.Chmod(file, 0755)
				if err == nil {
					err = os.WriteFile(file, bytes.Repeat([]byte("x"), len(retainedAssetInputs()[0].Data)), 0755)
				}
				if err == nil {
					err = os.Chmod(file, 0555)
				}
			case "changed file mode":
				err = os.Chmod(file, 0755)
			case "extra file":
				err = os.WriteFile(filepath.Join(root, "extra"), []byte("extra"), 0444)
			case "extra directory":
				err = os.Mkdir(filepath.Join(root, "extra"), 0555)
			case "manifest", "owner":
				name := "bundle.json"
				if kind == "owner" {
					name = "owner.json"
				}
				err = os.Remove(filepath.Join(directory, name))
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{root, filepath.Join(root, "nested")} {
				if err := os.Chmod(dir, 0555); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "changed directory mode" {
				if err := os.Chmod(filepath.Join(root, "empty"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.AssetBundlePath(bundle); err == nil {
				t.Fatal("accepted corrupted retained assets")
			}
			got, err := os.ReadFile(filepath.Join(target, "outside"))
			if err != nil || string(got) != "untouched" {
				t.Fatalf("modified symlink target: %q, %v", got, err)
			}
		})
	}
}

func TestAssetBundleOwnedCleanupAndLegacyCompatibility(t *testing.T) {
	store := retainedAssetStore(t)
	bundle, err := store.CaptureAssetBundle(t.Context(), randomID(), retainedAssetInputs())
	if err != nil {
		t.Fatal(err)
	}
	directory, _ := store.assetBundleDirectory(bundle)
	base := t.TempDir()
	putHelper(t, base, "helper", "historical helper", 0644)
	legacy, err := store.CaptureHelperBundle(t.Context(), randomID(), base, []string{"helper"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(legacy)
	want := fmt.Sprintf(`{"origin_run_id":%q,"id":%q,"files":[{"path":"helper","mode":292,"size":17,"sha256":%q}]}`, legacy.OriginID, actionHash(legacy.Files), digestBytes([]byte("historical helper")))
	if string(raw) != want {
		t.Fatalf("changed historical format or identity: %s", raw)
	}
	if err := store.VerifyHelperBundle(legacy); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(store.Dir, "assets/staging/unknown")
	putHelper(t, unknown, "keep", "unowned", 0600)
	lock, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := store.CleanupHelperStaging(t.Context(), lock); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("did not remove unreferenced read-only assets: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(unknown, "keep")); err != nil || string(got) != "unowned" {
		t.Fatalf("removed unmarked storage: %q, %v", got, err)
	}
}

func TestOwnedAssetCleanupRejectsReplacedRootAndDoesNotFollowLinks(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink", "child symlink"} {
		t.Run(replacement, func(t *testing.T) {
			store := retainedAssetStore(t)
			bundle, err := store.CaptureAssetBundle(t.Context(), randomID(), retainedAssetInputs())
			if err != nil {
				t.Fatal(err)
			}
			directory, _ := store.assetBundleDirectory(bundle)
			expected, err := os.Lstat(directory)
			if err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			putHelper(t, target, "keep", "unrelated", 0644)
			if err := os.Chmod(target, 0750); err != nil {
				t.Fatal(err)
			}
			if replacement == "child symlink" {
				if err := os.Symlink(target, filepath.Join(directory, "outside")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(directory, directory+"-original"); err != nil {
					t.Fatal(err)
				}
				if replacement == "directory" {
					putHelper(t, directory, "keep", "replacement", 0644)
				} else if err := os.Symlink(target, directory); err != nil {
					t.Fatal(err)
				}
			}
			err = removeOwnedAssetDirectory(directory, expected)
			if replacement == "child symlink" && err != nil || replacement != "child symlink" && err == nil {
				t.Fatalf("cleanup replacement result: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(target, "keep"))
			if err != nil || string(got) != "unrelated" {
				t.Fatalf("followed cleanup link: %q, %v", got, err)
			}
			if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0750 {
				t.Fatalf("changed cleanup link target permissions: %+v, %v", info, err)
			}
			if replacement == "directory" {
				if got, err := os.ReadFile(filepath.Join(directory, "keep")); err != nil || string(got) != "replacement" {
					t.Fatalf("modified replacement: %q, %v", got, err)
				}
			}
		})
	}
}

func TestAssetBundleRejectsStorageAncestorSymlink(t *testing.T) {
	store := retainedAssetStore(t)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(store.Dir, "assets")); err != nil {
		t.Fatal(err)
	}
	if bundle, err := store.CaptureAssetBundle(t.Context(), randomID(), retainedAssetInputs()); err == nil || bundle != nil {
		t.Fatalf("captured through redirected storage: %+v, %v", bundle, err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("wrote through storage link: %v, %v", entries, err)
	}
}

func TestAssetBundleBoundsInferredDirectoriesBeforeStaging(t *testing.T) {
	// Sixteen small manifests imply 4,112 paths. Exercise only preflight;
	// no tree is materialized and no file payload allocation is needed.
	entries := make([]AssetEntry, 16)
	for i := range entries {
		entries[i] = AssetEntry{Path: fmt.Sprintf("%02d/%sfile", i, strings.Repeat("p/", 255)), Kind: "file", Mode: 0644, SHA256: digestBytes(nil)}
	}
	if _, err := assetDirectories(entries); err == nil || !strings.Contains(err.Error(), "materialized path limit of 4096") {
		t.Fatalf("accepted excessive inferred parents: %v", err)
	}
	if dirs, err := assetDirectories(entries[:15]); err != nil || len(dirs)-1+15 != 3855 {
		t.Fatalf("rejected bounded inferred parents: %d, %v", len(dirs), err)
	}
}

func TestAssetBundlePortableUnicodePathCollisions(t *testing.T) {
	for _, paths := range [][]string{
		{"Readme", "README"},
		{"caf\u00e9/data", "cafe\u0301/other"},
		{"Stra\u00dfe/a", "STRASSE/b"},
		{"\u03a3/a", "\u03c2/b"},
		{"A", "a/child"},
	} {
		entries := make([]AssetEntry, 0, len(paths))
		for _, name := range paths {
			entries = append(entries, AssetEntry{Path: name, Kind: "file", Mode: 0644, SHA256: digestBytes(nil)})
		}
		slices.SortFunc(entries, func(a, b AssetEntry) int { return strings.Compare(a.Path, b.Path) })
		if err := ValidateAssetEntries(entries); err == nil || !strings.Contains(err.Error(), "portable asset path collision") {
			t.Fatalf("accepted casefold/canonical collision %v: %v", paths, err)
		}
	}
	// Same-spelled inferred parents are shared, and textual prefix overlap alone
	// does not collide. Keep Unicode spelling intact instead of rewriting paths.
	entries := []AssetEntry{
		{Path: "caf\u00e9/a", Kind: "file", Mode: 0644, SHA256: digestBytes(nil)},
		{Path: "caf\u00e9/b", Kind: "file", Mode: 0644, SHA256: digestBytes(nil)},
		{Path: "caf\u00e9-extra/a", Kind: "file", Mode: 0644, SHA256: digestBytes(nil)},
	}
	slices.SortFunc(entries, func(a, b AssetEntry) int { return strings.Compare(a.Path, b.Path) })
	if err := ValidateAssetEntries(entries); err != nil {
		t.Fatalf("rejected legitimate shared parents: %v", err)
	}
	if entries[1].Path != "caf\u00e9/a" {
		t.Fatal("rewrote original Unicode spelling")
	}
}
