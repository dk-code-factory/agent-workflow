package runtimeexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
)

func TestPersistentStateBuildRequestReadWriteAndRejectsUnsafeBinds(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	home := directory("home")
	t.Setenv("HOME", home)
	opts := LaunchOptions{Policy: Policy{Mode: "container", Image: "fixture"}, Image: containerapi.Image{ID: "sha256:" + strings.Repeat("a", 64), OS: "linux", Architecture: "amd64"}, Guardian: fixtureGuardian(t, "amd64"), Workspace: directory("workspace"), State: directory("state"), EngineState: directory("engine"), Actions: directory("actions")}
	lifetime := directory("lifetime")
	source := directory("plugin")
	owner := containerapi.Owner{Name: "owned", Labels: map[string]string{"agent-workflow.token": "token"}}
	opts.PersistentState = []PersistentStateMount{{Source: source, Target: "/cf-workflows-state"}}
	q, err := BuildRequest(opts, owner, lifetime)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range q.Mounts {
		if m.Target == "/cf-workflows-state" {
			found = true
			if m.Source != source || m.ReadOnly {
				t.Fatalf("unsafe persistent mount: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("persistent mount missing")
	}
	for _, source := range []string{source, directory("état")} {
		opts.PersistentState[0].Source = source
		for _, target := range []string{"/cf-state", "/cf-état"} {
			opts.PersistentState[0].Target = target
			request, err := BuildRequest(opts, owner, lifetime)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var restored containerapi.CreateRequest
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, mount := range restored.Mounts {
				if mount.Source == source {
					found = true
					if mount.Target != target || mount.ReadOnly {
						t.Fatalf("persistent mount changed during JSON serialization: %+v", mount)
					}
				}
			}
			if !found {
				t.Fatal("serialized persistent mount missing")
			}
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(root, "ancestor")
	if err := os.Symlink(root, ancestorLink); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{home, root, opts.Workspace, directory("workspace/plugin"), opts.EngineState, directory("engine/plugin"), opts.State, lifetime, opts.Actions, link, filepath.Join(ancestorLink, "plugin"), file, filepath.Join(root, "missing"), directory("plugin-\xff"), directory("plugin-\xfe")} {
		opts.PersistentState[0].Source = bad
		if _, err := BuildRequest(opts, owner, lifetime); err == nil {
			t.Errorf("accepted unsafe source %q", bad)
		}
	}
	opts.PersistentState[0].Source = source
	for _, bad := range []string{"/", "relative", "/workspace/private", "/state", "/engine-task-state/child", "/engine-guardian", "/actions", "/tmp/state", "/proc/x", "/x/../safe", "/x\\y", "/${{ inputs.path }}", "/cf-\xff", "/cf-\xfe"} {
		opts.PersistentState[0].Target = bad
		if _, err := BuildRequest(opts, owner, lifetime); err == nil {
			t.Errorf("accepted unsafe target %q", bad)
		}
	}
	opts.PersistentState = []PersistentStateMount{{Source: source, Target: "/durable"}, {Source: directory("other"), Target: "/durable/nested"}}
	if _, err := BuildRequest(opts, owner, lifetime); err == nil {
		t.Fatal("overlapping targets accepted")
	}
	opts.PersistentState[1] = PersistentStateMount{Source: directory("plugin/nested"), Target: "/other"}
	if _, err := BuildRequest(opts, owner, lifetime); err == nil {
		t.Fatal("overlapping sources accepted")
	}
}
