package runtimeexec

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

func TestContainerNetworkPolicyRejectsUnsupportedAndConflictingModes(t *testing.T) {
	for _, mode := range []string{"", "none", "bridge"} {
		p := Policy{Mode: "container", Image: "fixture", NetworkMode: mode}
		if err := p.ValidateFor("linux"); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var retained Policy
		if err = json.Unmarshal(data, &retained); err != nil || retained.NetworkMode != mode {
			t.Fatalf("network selection was not retained: %+v %v", retained, err)
		}
	}
	for _, p := range []Policy{
		{Mode: "container", Image: "fixture", NetworkMode: "host"},
		{Mode: "container", Image: "fixture", NetworkMode: "container:other"},
		{Mode: "container", Image: "fixture", NetworkMode: "custom-network"},
		{Mode: "local", NetworkMode: "bridge"},
		{NetworkMode: "none"},
		{Mode: "container", Image: "fixture", NetworkMode: "bridge", NetworkRuntime: "team"},
		{Mode: "container", Image: "fixture", NetworkMode: "none", NetworkRuntime: "team"},
		{Mode: "container", Image: "fixture", NetworkMode: "bridge", Network: &network.Reference{}},
	} {
		if err := p.ValidateFor("linux"); err == nil {
			t.Fatalf("accepted unsupported/conflicting policy: %+v", p)
		}
	}
}

func TestContainerBuildersHonorExplicitNetworkSelection(t *testing.T) {
	for _, mode := range []string{"", "none", "bridge"} {
		t.Run("mode="+mode, func(t *testing.T) {
			policy := Policy{Mode: "container", Image: "fixture", NetworkMode: mode}
			options := LaunchOptions{
				Policy:   policy,
				Image:    containerapi.Image{ID: "sha256:" + strings.Repeat("a", 64), OS: "linux", Architecture: "amd64"},
				Guardian: fixtureGuardian(t, "amd64"), Workspace: t.TempDir(), State: t.TempDir(),
			}
			owner := containerapi.Owner{Name: "owned", Labels: map[string]string{"agent-workflow.token": "token"}}
			request, err := BuildRequest(options, owner, t.TempDir())
			if err != nil || request.Network != mode {
				t.Fatalf("typed request network=%q; want %q: %v", request.Network, mode, err)
			}
			args, err := BuildContainer(exec.Command("echo", "hello"), policy, ContainerOptions{Workspace: options.Workspace, State: options.State}, owner, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for i, arg := range args {
				if arg == "--network" {
					found = true
					if i+1 >= len(args) || args[i+1] != mode {
						t.Fatalf("incorrect legacy network flag: %q", args)
					}
				}
			}
			if found != (mode != "") {
				t.Fatalf("legacy default/selection changed: %q", args)
			}
		})
	}
}
