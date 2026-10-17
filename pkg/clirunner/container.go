package clirunner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dkropachev/agent-workflow/pkg/process"
)

// containerCommand exposes only an independent repository, private provider
// state, the pinned bridge, and explicit provider authentication. Worktree .git
// indirection is rejected because it would expose shared Git administration.
func containerCommand(cmd *exec.Cmd, a Agent, workspace, state string, bridge bool) ([]string, error) {
	info, err := os.Lstat(filepath.Join(workspace, ".git"))
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("container workers require an independent repository (.git directory), not a host worktree")
	}
	runtime := a.Execution.Runtime
	if runtime == "" {
		runtime = "podman"
	}
	runtimePath, err := exec.LookPath(runtime)
	if err != nil {
		return nil, err
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return nil, err
	}
	name := "agent-workflow-" + hex.EncodeToString(token[:])
	args := []string{"run", "--rm", "--name", name, "--label", "agent-workflow.managed=true", "--pull=never", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--memory=4g", "--cpus=2", "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m", "--workdir", "/workspace"}
	if a.Execution.runID != "" {
		args = append(args, "--label", "agent-workflow.run="+a.Execution.runID)
	}
	if a.Execution.invocationID != "" {
		args = append(args, "--label", "agent-workflow.invocation="+a.Execution.invocationID)
	}
	if a.Execution.ownerRoot != "" {
		root, err := filepath.Abs(a.Execution.ownerRoot)
		if err != nil {
			return nil, err
		}
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		hash := sha256.Sum256([]byte(root))
		args = append(args, "--label", "agent-workflow.workspace="+hex.EncodeToString(hash[:]))
	}
	if runtime == "podman" {
		args = append(args, "--userns=keep-id")
	}
	args = append(args, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	bind := func(source, target string, readonly bool) error {
		info, statErr := os.Stat(source)
		if statErr != nil {
			return statErr
		}
		if info.IsDir() {
			real, err := filepath.EvalSymlinks(source)
			if err != nil {
				return err
			}
			real, err = filepath.Abs(real)
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err == nil {
				home, err = filepath.EvalSymlinks(home)
				if err == nil {
					relative, err := filepath.Rel(real, home)
					if err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
						return fmt.Errorf("host home directories and their ancestors cannot be mounted")
					}
				}
			}
		}

		if strings.ContainsAny(source, ",\n\r") {
			return fmt.Errorf("container bind path contains unsupported characters")
		}
		mount := "type=bind,src=" + source + ",dst=" + target
		if readonly {
			mount += ",ro=true"
		}
		args = append(args, "--mount", mount)
		return nil
	}
	if err = bind(workspace, "/workspace", !a.Execution.Writable); err != nil {
		return nil, err
	}
	if err = bind(state, "/state", false); err != nil {
		return nil, err
	}
	childArgs := append([]string{}, cmd.Args...)
	if bridge {
		if err = bind(filepath.Dir(a.Bridge), "/bridge", true); err != nil {
			return nil, err
		}
		childArgs[1] = "/bridge/" + filepath.Base(a.Bridge)
	}
	if a.Authentication.Mode == "native" {
		file := a.Authentication.CredentialFile
		if file == "" {
			return nil, fmt.Errorf("container native auth requires explicit credential-file")
		}
		file, err = filepath.EvalSymlinks(file)
		if err != nil {
			return nil, err
		}
		info, err = os.Stat(file)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("native credential reference must be a regular file")
		}
		if err = bind(file, "/state/auth.json", true); err != nil {
			return nil, err
		}
	}
	allowed := map[string]bool{"ANTHROPIC_BASE_URL": true, "ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true}
	if a.Authentication.CredentialEnv != "" {
		allowed[a.Authentication.CredentialEnv] = true
	}
	for _, assignment := range cmd.Env {
		k, _, _ := strings.Cut(assignment, "=")
		if allowed[k] {
			args = append(args, "--env", k)
		}
	}
	if bridge {
		args = append(args, "--env", "CLAUDE_CONFIG_DIR=/state", "--env", "MAX_STRUCTURED_OUTPUT_RETRIES=0", "--env", "ENABLE_CLAUDEAI_MCP_SERVERS=false")
	} else {
		args = append(args, "--env", "CODEX_HOME=/state")
	}
	args = append(args, "--entrypoint", childArgs[0], a.Execution.Image)
	args = append(args, childArgs[1:]...)
	providerEnv := cmd.Env
	cmd.Env = append(os.Environ(), providerEnv...)
	cmd.Path = runtimePath
	cmd.Err = nil
	cmd.Args = append([]string{runtime}, args...)
	// The lifetime guardian removes the named container even if the controller is
	// SIGKILLed. No Docker socket or host home directory is mounted in the worker.
	return []string{"__container_cleanup_v1", runtimePath, name}, nil
}

func superviseContainer(ctx context.Context, cmd *exec.Cmd, a Agent, workspace, state string, bridge bool) (func(), error) {
	if a.Execution.Mode != "container" {
		return process.Supervise(cmd)
	}
	cleanup, err := containerCommand(cmd, a, workspace, state, bridge)
	if err != nil {
		return nil, err
	}
	return process.SuperviseWithCleanup(cmd, cleanup)
}
