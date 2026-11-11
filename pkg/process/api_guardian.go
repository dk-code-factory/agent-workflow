//go:build linux || darwin

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
)

func init() {
	if len(os.Args) != 3 || os.Args[1] != "__container_api_guardian_v1" {
		return
	}
	var owner containerapi.Owner
	if json.Unmarshal([]byte(os.Args[2]), &owner) != nil {
		os.Exit(125)
	}
	lifetime := os.NewFile(3, "controller-lifetime")
	if lifetime == nil {
		os.Exit(125)
	}
	_, err := io.Copy(io.Discard, lifetime)
	_ = lifetime.Close()
	if err != nil {
		os.Exit(125)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if containerapi.Remove(ctx, owner) != nil {
		os.Exit(125)
	}
	os.Exit(0)
}

// OwnContainer starts an independent host guardian before the create request.
// Closing its private pipe (also on controller death) triggers verified cleanup.
// The workload guardian additionally prevents a delayed start after owner loss.
func OwnContainer(owner containerapi.Owner) (func() error, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(owner)
	if err != nil {
		read.Close()
		write.Close()
		return nil, err
	}
	cmd := exec.Command(binary, "__container_api_guardian_v1", string(encoded))
	cmd.ExtraFiles = []*os.File{read}
	if err = cmd.Start(); err != nil {
		read.Close()
		write.Close()
		return nil, err
	}
	read.Close()
	return func() error {
		_ = write.Close()
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("host container guardian: %w", err)
		}
		return nil
	}, nil
}
