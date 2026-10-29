//go:build linux

package network_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

type directoryObservation struct {
	CWD        string
	PWD        string
	Credential string
	Argument   string
	Executable string
}

func TestDirectoryObservationFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] != "directory-observation" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(126)
	}
	if err := json.NewEncoder(os.Stdout).Encode(directoryObservation{CWD: hex.EncodeToString([]byte(cwd)), PWD: hex.EncodeToString([]byte(os.Getenv("PWD"))), Credential: hex.EncodeToString([]byte(os.Getenv("FAKE_PROVIDER_CREDENTIAL"))), Argument: hex.EncodeToString([]byte(os.Args[len(os.Args)-2])), Executable: hex.EncodeToString([]byte(os.Args[0]))}); err != nil {
		os.Exit(126)
	}
	os.Exit(0)
}

func TestWrapPreservesNativeDirectoryAndEffectiveEnvironment(t *testing.T) {
	if os.Getenv("AW_DIRECTORY_CALLER") == "1" {
		runDirectoryCases(t, os.Getenv("AW_DIRECTORY_SERVICE"))
		return
	}
	root := t.TempDir()
	caller, service := filepath.Join(root, "caller"), filepath.Join(root, "service")
	for _, dir := range []string{caller, service, filepath.Join(root, "cwd-\xfd"), filepath.Join(caller, "project"), filepath.Join(caller, "child"), filepath.Join(root, "elsewhere", "target"), filepath.Join(root, "elsewhere", "child")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere", "target"), filepath.Join(caller, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.Args[0], filepath.Join(root, "executable-\xfc")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWrapPreservesNativeDirectoryAndEffectiveEnvironment$")
	child.Dir = caller
	child.WaitDelay = 2 * time.Second
	// The inherited PWD is deliberately unrelated to the caller's real cwd.
	// Empty Dir must retain it; nonempty Dir uses exec.Cmd's native adjustment.
	child.Env = []string{"PATH=/usr/bin:/bin", "AW_DIRECTORY_CALLER=1", "AW_DIRECTORY_SERVICE=" + service, "PWD=/original-inherited-pwd", "FAKE_PROVIDER_CREDENTIAL=inherited-credential", "DOCKER_HOST=unix:///stale-host", "AGENT_WORKFLOW_NETWORK_TERMINAL=1"}
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("isolated caller: %s %v", out, err)
	}
}

func runDirectoryCases(t *testing.T, service string) {
	caller, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, dir      string
		path, argument string
		env            []string
	}{
		{name: "empty/inherited"},
		{name: "relative/inherited", dir: "project"},
		{name: "absolute/inherited", dir: filepath.Join(caller, "project")},
		{name: "relative/explicit-pwd", dir: "project", env: []string{"PWD=/explicit-pwd", "FAKE_PROVIDER_CREDENTIAL=explicit-credential", "AGENT_WORKFLOW_NETWORK_TERMINAL=1"}},
		{name: "empty/explicit-empty", env: []string{}},
		{name: "relative/explicit-no-pwd", dir: "project", env: []string{"FAKE_PROVIDER_CREDENTIAL=explicit-credential"}},
		{name: "relative/duplicates", dir: "project", env: []string{"PWD=/first-pwd", "PWD=/last-pwd", "FAKE_PROVIDER_CREDENTIAL=first", "FAKE_PROVIDER_CREDENTIAL=last", "AGENT_WORKFLOW_NETWORK_TERMINAL=1", "AGENT_WORKFLOW_NETWORK_TERMINAL=0"}},
		{name: "relative/symlink-parent", dir: "link/../child"},
		{name: "absolute/symlink-parent", dir: caller + "/link/../child"},
		{name: "opaque-native-bytes", dir: filepath.Join(filepath.Dir(caller), "cwd-\xfd"), path: filepath.Join(filepath.Dir(caller), "executable-\xfc"), argument: "arg-\xff-\xc3(", env: []string{"FAKE_PROVIDER_CREDENTIAL=env-\xfe-\xed\xa0\x80"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := func() *exec.Cmd {
				path := tc.path
				if path == "" {
					path = os.Args[0]
				}
				cmd := exec.CommandContext(ctx, path, "-test.run=^TestDirectoryObservationFixture$", "--", tc.argument, "directory-observation")
				cmd.Dir = tc.dir
				cmd.Env = tc.env
				cmd.WaitDelay = 2 * time.Second
				return cmd
			}
			native := command()
			originalEnv := native.Environ()
			nativeOut, err := native.CombinedOutput()
			if err != nil {
				t.Fatalf("native launch: %s %v", nativeOut, err)
			}
			var want directoryObservation
			if err := json.Unmarshal(nativeOut, &want); err != nil {
				t.Fatalf("native output %q: %v", nativeOut, err)
			}
			wrapped := command()
			cleanup, err := network.Wrap(wrapped, fixtureReference(), strings.Repeat("d", 32))
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			payload := wrapped.ExtraFiles[0]
			info, err := payload.Stat()
			if err != nil {
				t.Fatal(err)
			}
			var req network.Request
			if err := json.NewDecoder(io.NewSectionReader(payload, 0, info.Size())).Decode(&req); err != nil {
				t.Fatal(err)
			}
			wantDir := tc.dir
			if wantDir == "" {
				wantDir = caller
			} else if !filepath.IsAbs(wantDir) {
				wantDir = caller + "/" + wantDir
			}
			if req.Dir != wantDir || wrapped.Dir != wantDir {
				t.Fatalf("directory resolution: payload=%q helper=%q want=%q", req.Dir, wrapped.Dir, wantDir)
			}
			wantEnv := network.Environment(originalEnv, fixtureReference())
			if !reflect.DeepEqual(req.Env, wantEnv) || !reflect.DeepEqual(wrapped.Env, wantEnv) {
				t.Fatal("payload/helper environment differs from the original command effective environment")
			}
			wantTerminal := false
			for _, value := range originalEnv {
				if value == "AGENT_WORKFLOW_NETWORK_TERMINAL=1" {
					wantTerminal = true
				}
			}
			if req.Terminal != wantTerminal {
				t.Fatalf("terminal marker=%t want=%t", req.Terminal, wantTerminal)
			}
			// Execute the production workload entry point from a different service cwd.
			// It consumes fd4 and performs its own Chdir before the real guardian starts
			// the same observation command used by the independent native launch above.
			req.Terminal = false // This fixture has pipes, not an invocation PTY.
			data, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			request, err := os.CreateTemp(t.TempDir(), "request")
			if err != nil {
				t.Fatal(err)
			}
			defer request.Close()
			if _, err := request.Write(data); err != nil {
				t.Fatal(err)
			}
			if _, err := request.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			lifeR, lifeW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer lifeR.Close()
			defer lifeW.Close()
			ackR, ackW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer ackR.Close()
			defer ackW.Close()
			if err := ackR.SetReadDeadline(time.Now().Add(12 * time.Second)); err != nil {
				t.Fatal(err)
			}
			dummy, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer dummy.Close()
			daemon := exec.CommandContext(ctx, os.Args[0], "__network_runtime_workload_v1", "directory-fixture")
			daemon.Dir = service
			daemon.Env = []string{}
			daemon.WaitDelay = 2 * time.Second
			daemon.ExtraFiles = []*os.File{lifeR, request, dummy, dummy, ackW}
			out, err := daemon.CombinedOutput()
			ackW.Close()
			if err != nil {
				t.Fatalf("workload from service cwd: %s %v", out, err)
			}
			var got directoryObservation
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("workload output %q: %v", out, err)
			}
			if got != want {
				t.Fatalf("native=%+v contained=%+v", want, got)
			}
			ack, err := io.ReadAll(ackR)
			if err != nil || string(ack) != "\x01" {
				t.Fatalf("guardian cleanup=%v: %v", ack, err)
			}
		})
	}
}
