package operational

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemThresholdsAreIndependent(t *testing.T) {
	path := t.TempDir()
	state, err := StatFilesystem(path)
	if err != nil {
		t.Fatal(err)
	}
	tooManyBytes := state.FreeBytes + 1
	zero := uint64(0)
	if _, err = (Config{MinFreeBytes: &tooManyBytes, MinFreeInodes: &zero}).Check(path); err == nil {
		t.Fatal("byte exhaustion not detected")
	}
	tooManyInodes := state.FreeInodes + 1
	if _, err = (Config{MinFreeBytes: &zero, MinFreeInodes: &tooManyInodes}).Check(path); err == nil {
		t.Fatal("inode exhaustion not detected")
	}
	// This uses observed thresholds, never fills the developer's filesystem.
	if _, err = (Config{MinFreeBytes: &zero, MinFreeInodes: &zero}).Check(path); err != nil {
		t.Fatal(err)
	}
}

func TestControllerLogsRotateAndRetainThreeFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.log")
	log := &RotatingLog{Path: path, MaxBytes: 32, Backups: 3}
	for i := 0; i < 8; i++ {
		if _, err := log.Write(bytes.Repeat([]byte{byte('a' + i)}, 24)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + ".1", path + ".2", path + ".3"} {
		info, err := os.Stat(name)
		if err != nil || info.Size() != 24 || info.Mode().Perm() != 0600 {
			t.Fatal(name, info, err)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatal("unbounded controller log history")
	}
}
