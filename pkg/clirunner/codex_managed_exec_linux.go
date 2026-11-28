//go:build linux

package clirunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// O_NONBLOCK opens a FIFO without waiting for a writer. captureManagedBuild
// then rejects nonregular descriptors before reading them.
func openManagedManifest(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "managed-manifest"), nil
}

// The held descriptor chooses the executed image without reopening its path.
// Verification observes the guardian's actual child before any protocol input.
// This binds an image; it does not provide filesystem isolation.
type managedExecutable struct {
	file   *os.File
	info   os.FileInfo
	sha256 string
}

func openManagedExecutable(identity *ManagedCodexBuildIdentity) (*managedExecutable, error) {
	if identity == nil || !managedHex(identity.BinarySHA256, 64) || identity.BinaryBytes <= 0 || identity.BinaryBytes > 1<<30 {
		return nil, fmt.Errorf("managed executable identity is invalid")
	}
	fd, err := unix.Open(identity.BinaryPath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "managed-executable")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != identity.BinaryBytes || info.Mode().Perm()&0111 == 0 {
		file.Close()
		return nil, fmt.Errorf("managed executable is not the retained regular executable")
	}
	digest, err := managedExecutableDigest(file)
	if err != nil || digest != identity.BinarySHA256 {
		file.Close()
		return nil, fmt.Errorf("managed executable digest differs from retained identity")
	}
	return &managedExecutable{file: file, info: info, sha256: digest}, nil
}

func managedExecutableDigest(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(file, (1<<30)+1))
	if err != nil || n > 1<<30 {
		return "", fmt.Errorf("managed executable hash exceeds bound or cannot be read")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (image *managedExecutable) command(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", argv...)
	cmd.ExtraFiles = []*os.File{image.file}
	return cmd
}

func (image *managedExecutable) verifyGuardianChild(ctx context.Context, guardian *os.Process) (int, error) {
	if guardian == nil {
		return 0, fmt.Errorf("managed guardian has not started")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lastObservation := "no child observed"
	for {
		children, err := managedGuardianChildren(guardian.Pid)
		if err != nil {
			return 0, fmt.Errorf("managed guardian children unavailable: %w", err)
		}
		if len(children) > 1 {
			return 0, fmt.Errorf("managed guardian owns an unexpected child set")
		}
		if len(children) == 1 {
			lastObservation = "child " + children[0]
			pid, err := strconv.Atoi(children[0])
			if err != nil || pid <= 0 {
				return 0, fmt.Errorf("managed child identity is invalid")
			}
			file, err := os.Open(fmt.Sprintf("/proc/%d/exe", pid))
			if err != nil {
				lastObservation += ": " + err.Error()
			}
			if err == nil {
				info, statErr := file.Stat()
				lastObservation += fmt.Sprintf(" image=%v stat=%v same=%v", info, statErr, statErr == nil && os.SameFile(image.info, info))
				if statErr == nil && os.SameFile(image.info, info) {
					digest, hashErr := managedExecutableDigest(file)
					file.Close()
					if hashErr != nil || digest != image.sha256 {
						return 0, fmt.Errorf("actual managed child executable digest differs")
					}
					after, readErr := managedGuardianChildren(guardian.Pid)
					if readErr != nil || len(after) != 1 || after[0] != children[0] {
						return 0, fmt.Errorf("managed child changed during verification")
					}
					return pid, nil
				}
				file.Close()
			}
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("actual managed child image was not verified (%s): %w", lastObservation, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Linux records children under their creating thread, which may differ from
// the Go guardian's leader thread. Inspect only this owned guardian's tasks.
func managedGuardianChildren(pid int) ([]string, error) {
	root := fmt.Sprintf("/proc/%d/task", pid)
	tasks, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if len(tasks) > 1024 {
		return nil, fmt.Errorf("guardian task count exceeds bound")
	}
	children := map[string]bool{}
	for _, task := range tasks {
		raw, err := os.ReadFile(filepath.Join(root, task.Name(), "children"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, child := range strings.Fields(string(raw)) {
			children[child] = true
		}
	}
	result := make([]string, 0, len(children))
	for child := range children {
		result = append(result, child)
	}
	sort.Strings(result)
	return result, nil
}
