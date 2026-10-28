package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"os/exec"
)

// containerCommand is the effect-free inspection/build compatibility helper.
func containerCommand(cmd *exec.Cmd, a Agent, workspace, state string, bridge bool) ([]string, error) {
	runtime, err := runtimeexec.RuntimePath(a.Execution.policy())
	if err != nil {
		return nil, err
	}
	owner := containerapi.Owner{Runtime: runtime, Name: "agent-workflow-" + runtimeexec.NewID(), Labels: map[string]string{"agent-workflow.managed": "true", "agent-workflow.token": runtimeexec.NewID()}}
	args, err := runtimeexec.BuildContainer(cmd, a.Execution.policy(), runtimeexec.ContainerOptions{Workspace: workspace, State: state, Bridge: bridge, CredentialEnv: a.Authentication.CredentialEnv, CredentialFile: a.Authentication.CredentialFile}, owner, state)
	if err != nil {
		return nil, err
	}
	cmd.Path = runtime
	cmd.Err = nil
	cmd.Args = append([]string{runtime}, args...)
	data, _ := json.Marshal(owner)
	return []string{"__container_cleanup_v2", string(data)}, nil
}
func superviseContainer(ctx context.Context, cmd *exec.Cmd, a Agent, workspace, state string, bridge bool) (func(), error) {
	if a.Execution.Mode != "container" {
		return runtimeexec.SupervisePolicy(ctx, cmd, a.Execution.policy())
	}
	if a.Execution.ownerRoot == "" {
		return nil, fmt.Errorf("container execution requires an originating workspace owner")
	}
	return runtimeexec.PrepareContainer(ctx, cmd, a.Execution.policy(), runtimeexec.ContainerOptions{Workspace: workspace, State: state, Root: a.Execution.ownerRoot, Bridge: bridge, CredentialEnv: a.Authentication.CredentialEnv, CredentialFile: a.Authentication.CredentialFile})
}
