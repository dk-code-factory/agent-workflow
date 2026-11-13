package containerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPodmanCLILifecycleAndWorkloadExitStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	script := filepath.Join(root, "podman")
	encoded, _ := json.Marshal(root)
	source := `#!` + python + `
import json, pathlib, sys
root = pathlib.Path(` + string(encoded) + `)
args = sys.argv[1:]
with (root / "calls.jsonl").open("a") as stream:
    stream.write(json.dumps(args) + "\n")
record = root / "object.json"
if args[0] == "info":
    print(json.dumps({"host":{"hostname":"fixture"},"store":{"graphRoot":str(root)}}))
elif args[:2] == ["image", "inspect"]:
    print(json.dumps([{"Id":"sha256:"+"a"*64,"Os":"linux","Architecture":"amd64","Config":{"Entrypoint":["tool"],"Cmd":["default"]}}]))
elif args[0] == "create":
    labels = dict(args[i+1].split("=",1) for i,a in enumerate(args) if a == "--label")
    record.write_text(json.dumps({"Id":"b"*64,"Name":args[args.index("--name")+1],"Config":{"Labels":labels},"State":{"Running":False}}))
    print("b"*64)
elif args[0] in ["start", "exec"]:
    data=sys.stdin.read()
    print("output:"+data)
    print("diagnostic",file=sys.stderr)
    sys.exit(int((root / "exit-code").read_text()) if (root / "exit-code").exists() else 7)
elif args[0] == "wait":
    print(7)
elif args[:2] == ["container","inspect"]:
    if record.exists(): print("["+record.read_text()+"]")
    else: sys.exit(125)
elif args[0] == "ps":
    if record.exists():
        obj=json.loads(record.read_text());print(obj["Id"]+" "+obj["Name"])
elif args[0] == "rm":
    record.unlink()
else:
    sys.exit(125)
`
	if err = os.WriteFile(script, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	selection := Selection{Transport: PodmanCLI, Executable: script, Environment: []string{"PATH=" + os.Getenv("PATH")}}
	runtime, err := Open(selection)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	selection.Identity, err = runtime.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	image, err := runtime.ResolveImage(ctx, "local-tag")
	if err != nil {
		t.Fatal(err)
	}
	owner := Owner{Selection: &selection, Name: "fixture", Labels: map[string]string{"owned": "yes"}}
	id, err := runtime.Create(ctx, CreateRequest{Name: owner.Name, Image: image.ID, User: "1000:1000", PrivatePaths: []string{"/workspace/.agent-workflow"}, Labels: owner.Labels, Mounts: []Mount{{Source: root, Target: "/actions", ReadOnly: true}}, Entrypoint: []string{"tool"}, Command: []string{"", "a b"}})
	if err != nil {
		t.Fatal(err)
	}
	owner.ID = id
	a, err := runtime.Attach(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var out, logs bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- a.Copy(Streams{Stdin: strings.NewReader("request"), Stdout: &out, Stderr: &logs}) }()
	if err = runtime.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal("workload exit became transport failure", err)
		}
	case <-ctx.Done():
		t.Fatal("attached workload did not complete", ctx.Err())
	}
	code, err := runtime.Wait(ctx, id)
	if err != nil || code != 7 || out.String() != "output:request\n" || logs.String() != "diagnostic\n" {
		t.Fatal(code, err, out.String(), logs.String())
	}
	execID, err := runtime.ExecCreate(ctx, id, ExecRequest{Command: []string{"tool", ""}, WorkingDir: "/workspace", Env: []string{"EXPLICIT=value"}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := runtime.ExecInspect(ctx, execID)
	if err != nil || !status.Running {
		t.Fatal(status, err)
	}
	attached, err := runtime.ExecAttach(ctx, execID)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	execDone := make(chan error, 1)
	go func() { execDone <- attached.Copy(Streams{Stdout: io.Discard, Stderr: io.Discard}) }()
	select {
	case err = <-execDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("attached exec did not complete", ctx.Err())
	}
	status, err = runtime.ExecInspect(ctx, execID)
	if err != nil || status.Running || status.ExitCode != 7 || status.ContainerID != id {
		t.Fatal(status, err)
	}
	for _, exitCode := range []int{124, 125, 126, 127, 128, 200, 255} {
		t.Run(fmt.Sprintf("exit-%d", exitCode), func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(root, "exit-code"), []byte(fmt.Sprint(exitCode)), 0600); err != nil {
				t.Fatal(err)
			}
			execID, err := runtime.ExecCreate(ctx, id, ExecRequest{Command: []string{"tool"}})
			if err != nil {
				t.Fatal(err)
			}
			attached, err := runtime.ExecAttach(ctx, execID)
			if err != nil {
				t.Fatal(err)
			}
			defer attached.Close()
			done := make(chan error, 1)
			go func() { done <- attached.Copy(Streams{Stdout: io.Discard, Stderr: io.Discard}) }()
			select {
			case err = <-done:
			case <-ctx.Done():
				t.Fatal("attached exec did not complete", ctx.Err())
			}
			reserved := exitCode >= 125 && exitCode <= 127
			if (err != nil) != reserved {
				t.Fatalf("exit %d: transport error=%v, want reserved=%t", exitCode, err, reserved)
			}
			status, err := runtime.ExecInspect(ctx, execID)
			if err != nil || status.Running || status.ExitCode != exitCode || status.ContainerID != id {
				t.Fatal(status, err)
			}
		})
	}
	if err = Remove(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := runtime.Inspect(ctx, owner.Name); err != nil || exists {
		t.Fatal(exists, err)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"--pull=never", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "ro=true", "a b", "/workspace/.agent-workflow:ro,nosuid,nodev,noexec,size=4096,mode=000"} {
		if !bytes.Contains(calls, []byte(required)) {
			t.Fatal("missing typed policy/argument", required, string(calls))
		}
	}
}
