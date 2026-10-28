//go:build linux

package network

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The wrapper directs setpriv's child to this fixture, rather than the package's
// production init dispatcher. Credentials are measured inside the executed child.
func TestRoutingDiscoveryCredentialsFixture(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "__network_runtime_routing_plan_v1" {
		return
	}
	var input routingDiscovery
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		os.Exit(125)
	}
	groups, err := os.Getgroups()
	if err != nil {
		os.Exit(125)
	}
	data, _ := json.Marshal(struct {
		UID, EUID, GID, EGID int
		Groups               []int
	}{os.Getuid(), os.Geteuid(), os.Getgid(), os.Getegid(), groups})
	if json.NewEncoder(os.Stdout).Encode(routingPlan{Files: []routingFile{{Path: "credentials", Data: data}}}) != nil {
		os.Exit(125)
	}
	os.Exit(0)
}

func TestRoutingPlanningFixture(t *testing.T) {
	if os.Getenv("AW_ROUTING_PLAN_FIXTURE") != "1" {
		return
	}
	var input routingDiscovery
	var systemDirs []string
	if json.NewDecoder(os.Stdin).Decode(&input) != nil || json.Unmarshal([]byte(os.Getenv("AW_ROUTING_SYSTEM_DIRS")), &systemDirs) != nil {
		os.Exit(125)
	}
	plan, err := planContainerRoutingPaths(input.Request.Env, input.Request.Reference, uint32(os.Getuid()), systemDirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	if json.NewEncoder(os.Stdout).Encode(plan) != nil {
		os.Exit(125)
	}
	os.Exit(0)
}

func TestRoutingDiscoveryExecutesAsAuthenticatedCaller(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("setpriv needs an already-root supervisor to set supplementary groups; no privilege escalation in this test")
	}
	if _, err := os.Stat("/usr/bin/setpriv"); err != nil {
		t.Fatal(err)
	}
	// A separate accessible fixture directory lets the dropped child execute the
	// test binary without changing permissions on Go's build output or the host.
	dir, err := os.MkdirTemp("", "routing-credentials-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "fixture")
	if err := os.WriteFile(child, data, 0755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "launcher")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+child+"' -test.run=^TestRoutingDiscoveryCredentialsFixture$ -- \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	plan, err := discoverRouting(memberLaunch{UID: 65534, GID: 65534, Groups: []uint32{65533, 65534}, Binary: wrapper}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != "credentials" {
		t.Fatalf("missing executed-child credentials: %+v", plan)
	}
	var got struct {
		UID, EUID, GID, EGID int
		Groups               []int
	}
	if err := json.Unmarshal(plan.Files[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got.Groups)
	if got.UID != 65534 || got.EUID != 65534 || got.GID != 65534 || got.EGID != 65534 || !reflect.DeepEqual(got.Groups, []int{65533, 65534}) {
		t.Fatalf("discovery retained supervisor authority: %+v", got)
	}
}

func TestRoutingDiscoveryRejectsMalformedLaunchBeforeWorkload(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ name, data string }{
		{"toml", "[engine\nremote=false"},
		{"typed-routing", "[engine]\nremote='false'\n"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			f := newRoutingFixture(t)
			conf := filepath.Join(f.home, "malformed.conf")
			writeRoutingFixture(t, conf, fixture.data)
			marker := filepath.Join(f.home, "workload-ran")
			request := routingDiscovery{Request: Request{Reference: f.ref, Env: append(f.env, "CONTAINERS_CONF="+conf), Path: "/bin/sh", Args: []string{"-c", "touch \"$1\"", "fixture", marker}}}
			input, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			// The subprocess consumes the private discovery protocol and runs the
			// production planner with fixture system roots. Host /etc container
			// configuration cannot change this deterministic rejection result.
			systemDirs, err := json.Marshal(f.system)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^TestRoutingPlanningFixture$")
			cmd.Stdin = bytes.NewReader(input)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + f.home, "AW_ROUTING_PLAN_FIXTURE=1", "AW_ROUTING_SYSTEM_DIRS=" + string(systemDirs)}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err == nil {
				t.Fatal("malformed launch produced a routing projection")
			}
			if stdout.Len() != 0 {
				t.Fatalf("rejected launch emitted usable plan: %s", &stdout)
			}
			if !strings.Contains(stderr.String(), "invalid containers TOML") && !strings.Contains(stderr.String(), "engine.remote must be boolean") {
				t.Fatalf("launch failed before exercising malformed config: %s", &stderr)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("workload effects before routing validation: %v", err)
			}
		})
	}
}

func TestRoutingViewRejectsIncompleteOrWritableProjection(t *testing.T) {
	for _, name := range []string{"missing-inventory", "unexpected-inventory", "missing-file", "writable-file", "writable-directory", "writable-empty-directory"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			data := []byte(`{"projected":true}`)
			if err := os.WriteFile(path, data, 0400); err != nil {
				t.Fatal(err)
			}
			visible := routingPlan{Files: []routingFile{{Path: path, Data: data}}}
			expected := routingPlan{Files: []routingFile{{Path: path}}}
			want := "inventory changed"
			switch name {
			case "missing-inventory":
				visible.Files = nil
			case "unexpected-inventory":
				visible.Files[0].Path = filepath.Join(dir, "new-config.json")
			case "missing-file":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				want = "no such file"
			case "writable-file":
				want = "incomplete or writable"
			case "writable-directory", "writable-empty-directory":
				visible.Files, expected.Files = nil, nil
				if name == "writable-directory" {
					expected.ReadOnlyDirs = []string{dir}
				} else {
					expected.EmptyDirs = []string{dir}
				}
				want = "directory is writable"
			}
			if err := verifyRoutingView(visible, expected, Reference{}); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s was not rejected at projection verification: %v", name, err)
			}
			if name != "missing-file" {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("verification changed caller file: %q, %v", got, err)
				}
			}
		})
	}
}

func TestRoutingViewPinsEverySocketAliasToSelectedRuntime(t *testing.T) {
	dir := t.TempDir()
	selected := filepath.Join(dir, "selected.sock")
	outside := filepath.Join(dir, "outside.sock")
	for _, path := range []string{selected, outside} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
	}
	ref := Reference{Endpoint: "unix://" + selected}
	if err := verifyRoutingView(routingPlan{Sockets: []string{selected}}, routingPlan{Sockets: []string{selected}}, ref); err != nil {
		t.Fatalf("selected socket rejected: %v", err)
	}
	for _, expectedAlias := range []bool{false, true} {
		t.Run(fmt.Sprintf("expected=%v", expectedAlias), func(t *testing.T) {
			visible, expected := routingPlan{}, routingPlan{}
			if expectedAlias {
				expected.Sockets = []string{outside}
			} else {
				visible.Sockets = []string{outside}
			}
			if err := verifyRoutingView(visible, expected, ref); err == nil || !strings.Contains(err.Error(), "does not match pinned runtime") {
				t.Fatalf("outside socket accepted: %v", err)
			}
		})
	}
}
