package runtimeexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

const GuardianProtocol = "1"
const ContainerGuardian = "/usr/local/bin/agent-workflow-guardian"
const ContainerBridge = "/opt/agent-workflow/bridge/bridge.mjs"

type ContainerOptions struct {
	Workspace, State, Root string
	Bridge                 bool
	CredentialFile         string
	CredentialEnv          string
}

func RuntimePath(policy Policy) (string, error) {
	name := policy.Runtime
	if name == "" {
		name = "podman"
	}
	if name != "podman" && name != "docker" {
		return "", fmt.Errorf("runtime must be docker or podman")
	}
	return exec.LookPath(name)
}

func CheckImage(ctx context.Context, policy Policy, bridge bool, requiredLabels ...map[string]string) error {
	parts := strings.Split(policy.Image, "@sha256:")
	digest := ""
	if len(parts) == 2 {
		digest = parts[1]
	} else if strings.HasPrefix(policy.Image, "sha256:") {
		digest = strings.TrimPrefix(policy.Image, "sha256:")
	}
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return fmt.Errorf("container image must be pinned by a full sha256 digest")
	}
	runtime, err := RuntimePath(policy)
	if err != nil {
		return err
	}
	data, err := containerapi.Output(ctx, runtime, "image", "inspect", policy.Image)
	if err != nil {
		return err
	}
	var images []struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if json.Unmarshal(data, &images) != nil || len(images) != 1 {
		return fmt.Errorf("invalid image metadata")
	}
	labels := images[0].Config.Labels
	for _, expected := range requiredLabels {
		for name, value := range expected {
			if labels[name] != value {
				return fmt.Errorf("image provider version label %s is incompatible", name)
			}
		}
	}
	if labels["agent-workflow.guardian-protocol"] != GuardianProtocol {
		return fmt.Errorf("image lacks the required container lifetime guardian")
	}
	if bridge && (labels["agent-workflow.bridge-protocol"] != "2" || labels["agent-workflow.claude-sdk"] != "0.3.311") {
		return fmt.Errorf("image bridge/SDK versions are incompatible")
	}
	return nil
}

// BuildContainer contains no effects. It is shared by command and agent roles.
func BuildContainer(cmd *exec.Cmd, policy Policy, options ContainerOptions, owner containerapi.Owner, lifetimeDir string) ([]string, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	workspace, err := filepath.Abs(options.Workspace)
	if err != nil {
		return nil, err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(filepath.Join(workspace, ".git")); err == nil && !info.IsDir() {
		return nil, fmt.Errorf("containers require independent Git administration, not a host worktree")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	args := []string{"create", "--interactive", "--name", owner.Name, "--pull=never", "--restart=no", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--memory=4g", "--cpus=2", "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m", "--workdir", "/workspace"}
	keys := make([]string, 0, len(owner.Labels))
	for key := range owner.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label", key+"="+owner.Labels[key])
	}
	if policy.Runtime == "" || policy.Runtime == "podman" {
		args = append(args, "--userns=keep-id")
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid = 65532
	}
	if gid == 0 {
		gid = 65532
	}
	if policy.UID != nil {
		uid = *policy.UID
	}
	if policy.GID != nil {
		gid = *policy.GID
	}
	args = append(args, "--user", strconv.Itoa(uid)+":"+strconv.Itoa(gid))
	bind := func(source, target string, readonly bool) error {
		source, err = filepath.Abs(source)
		if err != nil {
			return err
		}
		source, err = filepath.EvalSymlinks(source)
		if err != nil {
			return err
		}
		if strings.ContainsAny(source, ",\n\r") {
			return fmt.Errorf("unsupported container bind path")
		}
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if info.IsDir() {
			home, homeErr := os.UserHomeDir()
			if homeErr == nil {
				home, _ = filepath.EvalSymlinks(home)
				relative, relErr := filepath.Rel(source, home)
				if relErr == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
					return fmt.Errorf("host home directories and their ancestors cannot be mounted")
				}
			}
		}
		mount := "type=bind,src=" + source + ",dst=" + target
		if readonly {
			mount += ",ro=true"
		}
		args = append(args, "--mount", mount)
		return nil
	}
	if err = bind(workspace, "/workspace", !policy.Writable); err != nil {
		return nil, err
	}
	if err = bind(options.State, "/state", false); err != nil {
		return nil, err
	}
	if err = bind(lifetimeDir, "/engine-lifetime", true); err != nil {
		return nil, err
	}
	if options.CredentialFile != "" {
		if err = bind(options.CredentialFile, "/state/auth.json", true); err != nil {
			return nil, err
		}
	}
	allowed := map[string]bool{"ANTHROPIC_BASE_URL": true, "ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true}
	if options.CredentialEnv != "" {
		allowed[options.CredentialEnv] = true
	}
	for _, assignment := range cmd.Env {
		key, _, _ := strings.Cut(assignment, "=")
		if allowed[key] {
			args = append(args, "--env", key)
		}
	}
	args = append(args, "--env", "HOME=/state/home", "--env", "TMPDIR=/tmp")
	args = append(args, "--env", "AGENT_WORKFLOW_WORKSPACE_WRITABLE="+strconv.FormatBool(policy.Writable))
	if options.Bridge {
		args = append(args, "--env", "CLAUDE_CONFIG_DIR=/state", "--env", "MAX_STRUCTURED_OUTPUT_RETRIES=0", "--env", "ENABLE_CLAUDEAI_MCP_SERVERS=false")
	} else {
		args = append(args, "--env", "CODEX_HOME=/state")
	}
	if options.CredentialFile != "" {
		args = append(args, "--env", "AGENT_WORKFLOW_AUTH_FILE=/state/auth.json")
	}
	child := append([]string(nil), cmd.Args...)
	if options.Bridge {
		if len(child) < 2 {
			return nil, fmt.Errorf("bridge command missing script")
		}
		child[1] = ContainerBridge
	}
	for index, arg := range child {
		if filepath.IsAbs(arg) {
			relative, err := filepath.Rel(workspace, arg)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				child[index] = filepath.ToSlash(filepath.Join("/workspace", relative))
			}
		}
	}
	args = append(args, "--entrypoint", ContainerGuardian, policy.Image, "__container_workload_guardian_v1", "/engine-lifetime/lease", owner.Labels["agent-workflow.token"], "--")
	return append(args, child...), nil
}

func PrepareContainer(ctx context.Context, cmd *exec.Cmd, policy Policy, options ContainerOptions) (func(), error) {
	if err := CheckImage(ctx, policy, options.Bridge); err != nil {
		return nil, err
	}
	runtime, err := RuntimePath(policy)
	if err != nil {
		return nil, err
	}
	identity, err := containerapi.Identity(ctx, runtime)
	if err != nil {
		return nil, err
	}
	i, err := Admit(ctx, "container")
	if err != nil {
		return nil, err
	}
	if i == nil {
		return nil, fmt.Errorf("container execution requires a durable originating invocation")
	}
	root, err := filepath.EvalSymlinks(options.Root)
	if err != nil {
		_ = i.Save(context.WithoutCancel(ctx), "cleaned")
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		_ = i.Save(context.WithoutCancel(ctx), "cleaned")
		return nil, err
	}
	hash := sha256.Sum256([]byte(root))
	owner := containerapi.Owner{Runtime: runtime, Identity: identity, Name: "agent-workflow-" + i.Record.ID, Labels: map[string]string{"agent-workflow.managed": "true", "agent-workflow.run": i.Record.RunID, "agent-workflow.generation": strconv.FormatInt(i.Record.Generation, 10), "agent-workflow.invocation": i.Record.ID, "agent-workflow.workspace": hex.EncodeToString(hash[:]), "agent-workflow.token": i.Record.Token}}
	i.Record.Container = &owner
	if err = i.Save(ctx, "intent"); err != nil {
		return nil, err
	}
	args, err := BuildContainer(cmd, policy, options, owner, i.Directory())
	if err != nil {
		_ = i.Save(context.WithoutCancel(ctx), "cleaned")
		return nil, err
	}
	if err = i.StartHeartbeat(); err != nil {
		_ = i.Save(context.WithoutCancel(ctx), "cleaned")
		return nil, err
	}
	// The runtime creates an inert object first. Its identity is durable before
	// the attached start can launch a provider or command.
	create := exec.CommandContext(ctx, runtime, args...)
	create.Env = runtimeEnvironment(cmd.Env, options.CredentialEnv)
	var out limitedCreateOutput
	create.Stdout = &out
	create.Stderr = io.Discard
	stopCreate, err := process.Supervise(create)
	if err == nil {
		err = create.Run()
		stopCreate()
	}
	if err != nil {
		i.StopHeartbeat()
		i.Record.Error = "container creation outcome requires reconciliation"
		_ = i.Save(context.WithoutCancel(ctx), "uncertain")
		return nil, fmt.Errorf("%w: container create: %v", process.ErrCleanup, err)
	}
	owner.ID = strings.TrimSpace(out.String())
	if len(owner.ID) != 64 || strings.Trim(owner.ID, "0123456789abcdef") != "" {
		i.StopHeartbeat()
		_ = i.Save(context.WithoutCancel(ctx), "uncertain")
		return nil, fmt.Errorf("%w: runtime returned an invalid container identity", process.ErrCleanup)
	}
	if err = i.Save(ctx, "created"); err != nil {
		i.StopHeartbeat()
		return nil, err
	}
	if _, exists, err := containerapi.Inspect(ctx, owner); err != nil || !exists {
		i.StopHeartbeat()
		_ = i.Save(context.WithoutCancel(ctx), "uncertain")
		return nil, fmt.Errorf("%w: created container ownership is unconfirmed", process.ErrCleanup)
	}
	cmd.Path = runtime
	cmd.Err = nil
	cmd.Args = []string{filepath.Base(runtime), "start", "--attach", "--interactive", owner.ID}
	cmd.Env = runtimeEnvironment(cmd.Env, options.CredentialEnv)
	encoded, _ := json.Marshal(owner)
	stop, err := i.Supervise(ctx, cmd, []string{"__container_cleanup_v2", string(encoded)})
	if err != nil {
		i.StopHeartbeat()
		return nil, err
	}
	return stop, nil
}

func runtimeEnvironment(provider []string, credentialEnv string) []string {
	// HOME/CODEX_HOME and other provider state must not select another host
	// runtime context or rootless storage namespace. Only explicit credential
	// values are added for the runtime's --env NAME forwarding mechanism.
	allowed := map[string]bool{"ANTHROPIC_BASE_URL": true, "ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true}
	if credentialEnv != "" {
		allowed[credentialEnv] = true
	}
	out := append([]string(nil), os.Environ()...)
	for _, assignment := range provider {
		key, _, _ := strings.Cut(assignment, "=")
		if allowed[key] {
			out = append(out, assignment)
		}
	}
	return out
}

type limitedCreateOutput struct{ data []byte }

func (b *limitedCreateOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 4096 {
		return 0, fmt.Errorf("container create output exceeds limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func (b *limitedCreateOutput) String() string { return string(b.data) }

// Probe contexts bound runtime prerequisite checks independently of turn length.
func ProbeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 15*time.Second)
}
