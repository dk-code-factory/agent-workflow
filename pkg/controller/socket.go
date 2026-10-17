package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Unix socket addresses have a small platform-dependent byte limit (104 on
// macOS). A workspace locator keeps clients independent of the runtime path.
func socketAddress(dir string) (string, error) {
	if data, err := os.ReadFile(filepath.Join(dir, "controller.address")); err == nil {
		address := strings.TrimSpace(string(data))
		if filepath.IsAbs(address) && len(address) < 100 {
			return address, nil
		}
		return "", fmt.Errorf("invalid controller socket locator")
	}
	address := filepath.Join(dir, "controller.sock")
	if len(address) >= 100 {
		return "", fmt.Errorf("controller socket address is unavailable")
	}
	return address, nil
}
func newSocketAddress(dir string) (string, func(), error) {
	preferred := filepath.Join(dir, "controller.sock")
	if len(preferred) < 100 {
		return preferred, func() {}, nil
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", nil, err
	}
	root = filepath.Join(root, "agent-workflow", "sockets")
	hash := sha256.Sum256([]byte(dir))
	candidate := filepath.Join(root, hex.EncodeToString(hash[:12])+".sock")
	if len(candidate) < 100 {
		if err = os.MkdirAll(root, 0700); err != nil {
			return "", nil, err
		}
		return candidate, func() {}, nil
	}
	// A random, private short directory is safe even when cache/TMPDIR paths are
	// themselves long. It is removed when the owner stops.
	root, err = os.MkdirTemp("/var/tmp", "aw-socket-*")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(root, "controller.sock"), func() { _ = os.Remove(root) }, nil
}
func writeSocketAddress(dir, address string) error {
	file, err := os.CreateTemp(dir, ".socket-address-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.WriteString(address + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, "controller.address"))
}
