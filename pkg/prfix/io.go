package prfix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

const maxOutput = 16 << 20

type boundedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > maxOutput {
		p = p[:maxOutput-b.Len()]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func runCommand(ctx context.Context, root string, input []byte, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = root
	cmd.Stdin = bytes.NewReader(input)
	var out, stderr boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	cleanup, e := process.Supervise(cmd)
	if e != nil {
		return nil, e
	}
	defer cleanup()
	e = cmd.Run()
	if out.overflow || stderr.overflow {
		return nil, fmt.Errorf("%s output exceeds 16 MiB", command)
	}
	if e != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", command, strings.Join(args, " "), e, stderr.String())
	}
	return out.Bytes(), nil
}
func writeImmutable(path string, b []byte) error {
	if err := safeParent(path); err != nil {
		return err
	}
	if st, e := os.Lstat(path); e == nil {
		if !st.Mode().IsRegular() {
			return fmt.Errorf("application artifact must be a regular file: %s", path)
		}
		old, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(old, b) {
			return fmt.Errorf("immutable artifact conflicts: %s", path)
		}
		return syncArtifactDirectory(path)
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	// Publish only fully written, synced bytes. A same-directory hard link is an
	// atomic no-replace operation; an interrupted writer leaves only a temp file.
	f, e := os.CreateTemp(filepath.Dir(path), ".prfix-artifact-*")
	if e != nil {
		return e
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	_, w := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if e = errors.Join(w, syncErr, closeErr); e != nil {
		return e
	}
	if e = os.Link(temporary, path); errors.Is(e, os.ErrExist) {
		st, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("application artifact must be a regular file: %s", path)
		}
		old, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(old, b) {
			return fmt.Errorf("immutable artifact conflicts: %s", path)
		}
	} else if e != nil {
		return e
	}
	return syncArtifactDirectory(path)
}
func syncArtifactDirectory(path string) error {
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func artifact(root string, v any) (string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	path := filepath.Join(root, ".agent-workflow", "pr-fix", "artifacts", digest(v)+".json")
	return path, writeImmutable(path, b)
}
func readJSON(path string, v any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, maxOutput+1))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("one JSON document required")
	}
	return nil
}

func safeParent(path string) error {
	abs, e := filepath.Abs(filepath.Dir(path))
	if e != nil {
		return e
	}
	missing := []string{}
	for probe := abs; ; probe = filepath.Dir(probe) {
		st, e := os.Lstat(probe)
		if os.IsNotExist(e) {
			missing = append(missing, probe)
		} else if e != nil {
			return e
		} else if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return fmt.Errorf("application directory must be a real directory: %s", probe)
		}
		if filepath.Dir(probe) == probe {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if e = os.Mkdir(missing[i], 0700); e != nil && !os.IsExist(e) {
			return e
		}
	}
	return nil
}
