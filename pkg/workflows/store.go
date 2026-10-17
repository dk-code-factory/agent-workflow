package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FileStore writes one private, atomically replaced JSON checkpoint per run.
// Records are observational: interrupted runs are never automatically replayed.
type FileStore struct{ Dir string }

func (s FileStore) Save(ctx context.Context, run *Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(run.ID) != 32 || !hexID(run.ID) {
		return fmt.Errorf("invalid run id")
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".run-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(s.Dir, run.ID+".json"))
}

func hexID(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
