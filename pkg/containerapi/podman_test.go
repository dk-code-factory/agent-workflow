package containerapi

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPodmanCLILifecycleAndWorkloadExitStatus(t *testing.T) {
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
    sys.exit(7)
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
	selection.Identity, err = runtime.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	image, err := runtime.ResolveImage(t.Context(), "local-tag")
	if err != nil {
		t.Fatal(err)
	}
	owner := Owner{Selection: &selection, Name: "fixture", Labels: map[string]string{"owned": "yes"}}
	id, err := runtime.Create(t.Context(), CreateRequest{Name: owner.Name, Image: image.ID, User: "1000:1000", PrivatePaths: []string{"/workspace/.agent-workflow"}, Labels: owner.Labels, Mounts: []Mount{{Source: root, Target: "/actions", ReadOnly: true}}, Entrypoint: []string{"tool"}, Command: []string{"", "a b"}})
	if err != nil {
		t.Fatal(err)
	}
	owner.ID = id
	a, err := runtime.Attach(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out, logs bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- a.Copy(Streams{Stdin: strings.NewReader("request"), Stdout: &out, Stderr: &logs}) }()
	if err = runtime.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal("workload exit became transport failure", err)
	}
	code, err := runtime.Wait(t.Context(), id)
	if err != nil || code != 7 || out.String() != "output:request\n" || logs.String() != "diagnostic\n" {
		t.Fatal(code, err, out.String(), logs.String())
	}
	execID, err := runtime.ExecCreate(t.Context(), id, ExecRequest{Command: []string{"tool", ""}, WorkingDir: "/workspace", Env: []string{"EXPLICIT=value"}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := runtime.ExecInspect(t.Context(), execID)
	if err != nil || !status.Running {
		t.Fatal(status, err)
	}
	attached, err := runtime.ExecAttach(t.Context(), execID)
	if err != nil {
		t.Fatal(err)
	}
	if err = attached.Copy(Streams{Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	status, err = runtime.ExecInspect(t.Context(), execID)
	if err != nil || status.Running || status.ExitCode != 7 || status.ContainerID != id {
		t.Fatal(status, err)
	}
	if err = Remove(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := runtime.Inspect(t.Context(), owner.Name); err != nil || exists {
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
