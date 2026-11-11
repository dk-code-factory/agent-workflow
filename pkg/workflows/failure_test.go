package workflows

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func TestFilesystemFailureClassificationDoesNotInterpretResourceNames(t *testing.T) {
	for _, path := range []string{"/tmp/1333810401/missing-input", "/tmp/403-repository/file", "/work/credentials/authentication-401.sqlite", "/work/storage/disk-limit"} {
		ordinary := &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		for _, err := range []error{ordinary, fmt.Errorf("input: %w", ordinary), errors.Join(errors.New("input rejected"), ordinary)} {
			if got := classifyFailure(err); got != "execution" {
				t.Fatalf("resource path became %s: %v", got, err)
			}
			if got := unsafeWorkerFailureClass(err); got != "" {
				t.Fatalf("resource path became unsafe %s: %v", got, err)
			}
		}
		storage := &runtimeexec.StorageError{Err: ordinary}
		if got := unsafeWorkerFailureClass(storage); got != "storage" {
			t.Fatalf("typed storage identity was lost: %s", got)
		}
		authentication := fmt.Errorf("authentication failed: %w", ordinary)
		if got := unsafeWorkerFailureClass(authentication); got != "authentication" {
			t.Fatalf("wrapper authentication diagnostic was lost: %s", got)
		}
		permission := &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
		if got := unsafeWorkerFailureClass(permission); got != "authentication" {
			t.Fatalf("real permission denial was lost: %s", got)
		}
	}
}
