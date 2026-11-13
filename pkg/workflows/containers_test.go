package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func TestContainerAuthoringAndUnsupportedSharedJobs(t *testing.T) {
	for _, container := range []string{"ubuntu:24.04", "{image: 'ubuntu:24.04'}"} {
		w, err := Parse([]byte("jobs: {build: {runs-on: local, container: " + container + ", steps: [{run: 'echo ready'}, {uses: 'docker://ubuntu:24.04', with: {entrypoint: /bin/sh, args: '-c \"echo separate\"'}}]}}"))
		if err != nil {
			t.Fatal(err)
		}
		if err = Validate(w); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{
		"jobs: {build: {runs-on: ubuntu-latest, steps: [{argv: [echo, hello]}]}}",
		"jobs: {build: {container: {image: '${{ inputs.image }}'}, steps: [{run: echo}]}}",
		"jobs: {build: {container: ubuntu, uses: workflows/child.yml}}",
		"jobs: {build: {container: ubuntu, steps: [{uses: agent/codex, with: {prompt: hello}}]}}",
		"jobs: {build: {container: ubuntu, steps: [{parallel: [{argv: [echo, hello]}]}]}}",
		"jobs: {build: {container: ubuntu, steps: [{uses: input/wait, with: {prompt: hello, evidence-id: review}}]}}",
		"jobs: {build: {steps: [{uses: 'docker://ubuntu', with: {args: '${{ inputs.args }}'}}]}}",
	} {
		w, err := Parse([]byte(source))
		if err == nil {
			err = Validate(w)
		}
		if err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
}
func TestDockerEntrypointCmdAndQuotedArguments(t *testing.T) {
	image := containerapi.Image{Entrypoint: []string{"tool", "fixed"}, Cmd: []string{"default", ""}}
	for _, tc := range []struct {
		with map[string]any
		want []string
	}{
		{nil, []string{"tool", "fixed", "default", ""}},
		{map[string]any{"entrypoint": "alternate"}, []string{"alternate", "default", ""}},
		{map[string]any{"args": `"a b" '' "$HOME" '$(touch unsafe)'`}, []string{"tool", "fixed", "a b", "", "$HOME", "$(touch unsafe)"}},
		{map[string]any{"args": ""}, []string{"tool", "fixed"}},
	} {
		got, err := dockerCommand(Step{With: tc.with}, image)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%#v %v; want %#v", got, err, tc.want)
		}
	}
	for _, bad := range []string{`'unfinished`, `"unfinished`, `trailing\`} {
		if _, err := SplitContainerArgs(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestContainerDeclarationScalarNormalization(t *testing.T) {
	for _, tc := range []struct{ declaration, image string }{
		{"null", ""}, {"{image: 123}", "123"}, {"{image: 01}", "01"}, {"{image: true}", "true"},
	} {
		t.Run(tc.declaration, func(t *testing.T) {
			w, err := Parse([]byte("jobs: {build: {container: " + tc.declaration + ", steps: [{argv: [echo, ready]}]}}"))
			if err != nil {
				t.Fatal(err)
			}
			if err = Validate(w); err != nil {
				t.Fatal(err)
			}
			container := w.Jobs["build"].Container
			if tc.image == "" {
				if container != nil {
					t.Fatalf("null container became %+v", container)
				}
			} else if container == nil || container.Image != tc.image {
				t.Fatalf("image=%+v; want %q", container, tc.image)
			}
		})
	}
}

type fakeContainer struct {
	env                    []string
	id, name, dir, actions string
	taskStateDir           string
	workingDir             string
	labels                 map[string]string
	argv                   []string
	resident               bool
	running                bool
	started, done, removed chan struct{}
	code                   int
	startDelayInspections  int
	mu                     sync.Mutex
}
type fakeExec struct {
	env     []string
	id      string
	parent  *fakeContainer
	argv    []string
	dir     string
	running bool
	code    int
}
type workflowDocker struct {
	commandTimeout        time.Duration
	imageReferences       []string
	helper                func([]string, []byte, []string) ([]byte, []byte, int, bool)
	t                     *testing.T
	mu                    sync.Mutex
	containers            map[string]*fakeContainer
	execs                 map[string]*fakeExec
	root, workspace       string
	creates, executions   int
	removalStarted        chan struct{}
	removalGate           chan struct{}
	removalError          bool
	createResponseError   bool
	execAttachError       bool
	stallPhase            string
	stallRelease          chan struct{}
	stallOnce, stallUsed  bool
	startDelayInspections int
}

func (d *workflowDocker) command(c *fakeContainer, argv []string, input []byte, dir string, env []string) ([]byte, []byte, int) {
	if d.helper != nil {
		if out, logs, code, handled := d.helper(argv, input, env); handled {
			return out, logs, code
		}
	}
	if c.taskStateDir != "" && len(input) != 0 {
		// Model the fixed socket bind for helpers executed as host subprocesses.
		// Preserve business JSON bytes, which may contain literal path-looking data.
		var request map[string]json.RawMessage
		if json.Unmarshal(input, &request) == nil {
			var descriptor taskStateDescriptor
			if json.Unmarshal(request["task_state"], &descriptor) == nil && descriptor.Socket == taskStateMount+"/rpc.sock" {
				descriptor.Socket = filepath.Join(c.taskStateDir, "rpc.sock")
				request["task_state"], _ = json.Marshal(descriptor)
				input, _ = json.Marshal(request)
			}
		}
	}
	args := append([]string(nil), argv...)
	for i, a := range args {
		a = strings.ReplaceAll(a, "/tmp/", c.dir+"/")
		a = strings.ReplaceAll(a, "/workspace", d.workspace)
		if c.actions != "" {
			a = strings.ReplaceAll(a, "/actions", c.actions)
		}
		args[i] = a
	}
	if len(args) == 0 {
		return nil, []byte("no command"), 127
	}
	cmd := exec.Command(args[0], args[1:]...)
	if d.commandTimeout > 0 {
		ctx, cancel := context.WithTimeout(d.t.Context(), d.commandTimeout)
		defer cancel()
		cmd = exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.WaitDelay = time.Second
	}
	if dir == "" {
		dir = "/workspace"
	}
	if dir == "/workspace" || strings.HasPrefix(dir, "/workspace/") {
		dir = d.workspace + strings.TrimPrefix(dir, "/workspace")
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(string(input))
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		d.t.Logf("fake command failed: argv=%q dir=%q error=%v stderr=%s", args, cmd.Dir, err, stderr.String())
		code = 127
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
	}
	return []byte(stdout.String()), []byte(stderr.String()), code
}
func fakeStream(w http.ResponseWriter) (net.Conn, error) {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return nil, err
	}
	fmt.Fprint(buf, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	err = buf.Flush()
	return conn, err
}
func writeDockerOutput(conn net.Conn, stdout, stderr []byte) {
	for i, b := range [][]byte{stdout, stderr} {
		if len(b) == 0 {
			continue
		}
		header := make([]byte, 8)
		header[0] = byte(i + 1)
		binary.BigEndian.PutUint32(header[4:], uint32(len(b)))
		_, _ = conn.Write(header)
		_, _ = conn.Write(b)
	}
}
func (d *workflowDocker) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v1.56")
	if d.stallPhase != "" && (d.stallPhase == "create" && p == "/containers/create" || d.stallPhase != "create" && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/"+d.stallPhase)) {
		d.mu.Lock()
		stall := !d.stallOnce || !d.stallUsed
		d.stallUsed = true
		d.mu.Unlock()
		if stall {
			select {
			case <-r.Context().Done():
			case <-d.stallRelease:
			}
			return
		}
	}
	if p == "/_ping" {
		w.Header().Set("API-Version", "1.56")
		return
	}
	if p == "/info" {
		fmt.Fprint(w, `{"ID":"workflow-fixture"}`)
		return
	}
	if strings.HasPrefix(p, "/images/") {
		d.mu.Lock()
		d.imageReferences = append(d.imageReferences, p)
		d.mu.Unlock()
		fmt.Fprintf(w, `{"Id":%q,"Os":"linux","Architecture":"amd64","Config":{"Entrypoint":["/bin/sh"],"Cmd":["-c","printf baked"]}}`, "sha256:"+strings.Repeat("a", 64))
		return
	}
	if p == "/containers/create" {
		var q struct {
			WorkingDir string
			Cmd        []string
			Labels     map[string]string
			Env        []string
			HostConfig struct {
				Mounts []struct {
					Source string
					Target string
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			d.t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		argv := q.Cmd
		for i, a := range argv {
			if a == "--" {
				argv = argv[i+1:]
				break
			}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.creates++
		id := fmt.Sprintf("%064x", d.creates)
		dir := filepath.Join(d.root, id)
		if err := os.Mkdir(dir, 0700); err != nil {
			d.t.Error(err)
		}
		c := &fakeContainer{id: id, name: r.URL.Query().Get("name"), dir: dir, workingDir: q.WorkingDir, env: q.Env, labels: q.Labels, argv: argv, resident: len(argv) > 1 && argv[1] == "__container_idle_v1", started: make(chan struct{}), done: make(chan struct{}), removed: make(chan struct{}), startDelayInspections: d.startDelayInspections}
		if d.stallOnce && d.stallPhase == "running" {
			d.startDelayInspections = 0
		}
		for _, m := range q.HostConfig.Mounts {
			if m.Target == "/actions" {
				c.actions = m.Source
			}
			if m.Target == taskStateMount {
				c.taskStateDir = m.Source
			}

		}
		d.containers[id] = c
		d.containers[c.name] = c
		if d.createResponseError {
			http.Error(w, "container created but response failed", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"Id":%q}`, id)
		return
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 2 {
		http.Error(w, "unknown", 404)
		return
	}
	if parts[0] == "exec" {
		d.mu.Lock()
		e := d.execs[parts[1]]
		attachError := d.execAttachError
		d.mu.Unlock()
		if e == nil {
			http.Error(w, "no exec", 404)
			return
		}
		if parts[2] == "json" {
			d.mu.Lock()
			defer d.mu.Unlock()
			fmt.Fprintf(w, `{"ID":%q,"ContainerID":%q,"Running":%t,"ExitCode":%d}`, e.id, e.parent.id, e.running, e.code)
			return
		}
		if attachError {
			http.Error(w, "fixture exec attach transport failure", http.StatusBadGateway)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		conn, err := fakeStream(w)
		if err != nil {
			d.t.Error(err)
			return
		}
		defer conn.Close()
		d.mu.Lock()
		e.running = true
		d.mu.Unlock()
		input, _ := io.ReadAll(conn)
		stdout, stderr, code := d.command(e.parent, e.argv, input, e.dir, e.env)
		d.mu.Lock()
		e.running = false
		e.code = code
		d.mu.Unlock()
		writeDockerOutput(conn, stdout, stderr)
		return
	}
	d.mu.Lock()
	c := d.containers[parts[1]]
	d.mu.Unlock()
	if c == nil {
		http.Error(w, `{"message":"No such container"}`, 404)
		return
	}
	if r.Method == "DELETE" {
		if d.removalStarted != nil {
			select {
			case d.removalStarted <- struct{}{}:
			default:
			}
			<-d.removalGate
		}
		if d.removalError {
			http.Error(w, "container removal failed", http.StatusInternalServerError)
			return
		}
		d.mu.Lock()
		delete(d.containers, c.id)
		delete(d.containers, c.name)
		close(c.removed)
		d.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	switch parts[2] {
	case "json":
		c.mu.Lock()
		defer c.mu.Unlock()
		select {
		case <-c.started:
			if c.startDelayInspections > 0 {
				c.startDelayInspections--
				c.running = c.startDelayInspections == 0
			}
		default:
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": c.id, "Name": "/" + c.name, "Config": map[string]any{"Labels": c.labels}, "State": map[string]any{"Running": c.running}})
	case "start":
		c.mu.Lock()
		c.running = c.startDelayInspections == 0
		c.mu.Unlock()
		close(c.started)
		w.WriteHeader(204)
	case "attach":
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		conn, err := fakeStream(w)
		if err != nil {
			d.t.Error(err)
			return
		}
		defer conn.Close()
		input, _ := io.ReadAll(conn)
		select {
		case <-c.started:
		case <-c.removed:
			return
		}
		if c.resident {
			<-c.removed
			c.mu.Lock()
			c.running = false
			c.mu.Unlock()
			close(c.done)
			return
		}
		stdout, stderr, code := d.command(c, c.argv, input, c.workingDir, c.env)
		c.mu.Lock()
		c.running = false
		c.code = code
		c.mu.Unlock()
		writeDockerOutput(conn, stdout, stderr)
		close(c.done)
	case "wait":
		select {
		case <-c.done:
			c.mu.Lock()
			fmt.Fprintf(w, `{"StatusCode":%d}`, c.code)
			c.mu.Unlock()
		case <-r.Context().Done():
		}
	case "exec":
		c.mu.Lock()
		running := c.running
		c.mu.Unlock()
		if !running {
			http.Error(w, `{"message":"container is not running"}`, http.StatusConflict)
			return
		}
		var q struct {
			Cmd        []string
			WorkingDir string
			Env        []string
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			d.t.Error(err)
		}
		argv := q.Cmd[5:]
		d.mu.Lock()
		defer d.mu.Unlock()
		d.executions++
		id := fmt.Sprintf("exec-%d", d.executions)
		d.execs[id] = &fakeExec{env: q.Env, id: id, parent: c, argv: argv, dir: q.WorkingDir, running: false}
		fmt.Fprintf(w, `{"Id":%q}`, id)
	default:
		http.Error(w, "unexpected", 500)
		d.t.Errorf("unexpected %s %s", r.Method, p)
	}
}
func containerExecutor(t *testing.T) (*Executor, *SQLiteStore, *workflowDocker) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	d := &workflowDocker{t: t, root: root, workspace: workspace, containers: map[string]*fakeContainer{}, execs: map[string]*fakeExec{}}
	socket := filepath.Join(t.TempDir(), "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(d.serve)}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	t.Setenv("DOCKER_HOST", "unix://"+socket)
	t.Setenv("DOCKER_CONTEXT", "")
	directory := filepath.Join(root, "guardians")
	if err = os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 2)
	binary.LittleEndian.PutUint16(data[18:], 62)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	if err = os.WriteFile(filepath.Join(directory, "linux-amd64"), data, 0755); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	manifest, _ := json.Marshal(map[string]string{"linux-amd64": hex.EncodeToString(hash[:])})
	if err = os.WriteFile(filepath.Join(directory, "checksums.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	return &Executor{WorkspaceDir: workspace, Store: store, MaxParallel: 1, Slots: make(chan struct{}, 1), ContainerGuardianDirectory: directory}, store, d
}
func TestSharedJobStateSiblingIsolationCapacityAndCleanup(t *testing.T) {
	e, store, d := containerExecutor(t)
	source := `jobs:
  build:
    runs-on: local
    container: ubuntu:24.04
    steps:
      - id: prepare
        run: echo ready > /tmp/build-status
      - id: shared
        sequence:
          - id: read
            run: cat /tmp/build-status
      - id: sibling
        uses: docker://ubuntu:24.04
        with:
          entrypoint: /bin/sh
          args: '-c "test ! -e /tmp/build-status && echo isolated"'
      - id: again
        run: cat /tmp/build-status
`
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/test.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["build"].Steps["read"].Outputs["stdout"] != "ready\n" || run.Jobs["build"].Steps["sibling"].Outputs["stdout"] != "isolated\n" || run.Jobs["build"].Steps["again"].Outputs["stdout"] != "ready\n" {
		t.Fatalf("%+v", run)
	}
	if len(e.Slots) != 0 {
		t.Fatal("capacity lease leaked")
	}
	d.mu.Lock()
	if len(d.containers) != 0 || d.creates != 2 || d.executions != 3 {
		t.Errorf("containers=%d creates=%d execs=%d", len(d.containers), d.creates, d.executions)
	}
	d.mu.Unlock()
	records, err := store.Invocations(t.Context(), run.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.State != "cleaned" {
			t.Fatal(r)
		}
	}
}
func TestSharedJobPreflightIgnoresUnusedLegacyImage(t *testing.T) {
	for _, legacyJob := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-job-%t", legacyJob), func(t *testing.T) {
			e, _, _ := containerExecutor(t)
			e.CommandExecution = runtimeexec.Policy{Mode: "container", Runtime: "docker", Image: "unused-legacy-image"}
			source := "jobs: {build: {container: ubuntu:24.04, steps: [{id: command, run: echo ready}]}}"
			if legacyJob {
				source = "jobs: {build: {container: ubuntu:24.04, steps: [{run: echo ready}]}, legacy: {steps: [{argv: [echo, legacy]}]}}"
			}
			if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/test.yml"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
			if legacyJob {
				if err == nil || !strings.Contains(err.Error(), "container image must be pinned") {
					t.Fatalf("legacy command bypassed image admission: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if run.Jobs["build"].Steps["command"].Outputs["stdout"] != "ready\n" {
				t.Fatalf("shared command did not execute: %+v", run)
			}
		})
	}
}

func TestNestedReusableContainerCallsUseOwningDefinitionAndOutputs(t *testing.T) {
	e, _, d := containerExecutor(t)
	files := map[string]string{
		"entry.yml": `jobs: {call: {uses: ./workflows/wrapper.yml, with: {message: ready}}}`,
		"wrapper.yml": `on: {workflow_call: {inputs: {message: {type: string}}, outputs: {answer: {value: '${{ jobs.inner.outputs.answer }}'}}}}
jobs: {inner: {uses: ./workflows/helper.yml, with: {message: '${{ inputs.message }}'}}}`,
		"helper.yml": `on: {workflow_call: {inputs: {message: {type: string}}, outputs: {answer: {value: '${{ jobs.work.outputs.answer }}'}}}}
jobs: {work: {container: {image: ubuntu:24.04}, outputs: {answer: '${{ steps.echo.outputs.stdout }}'}, steps: [{id: echo, run: 'echo ${{ inputs.message }}'}]}}`,
	}
	for name, source := range files {
		if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows", name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/entry.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["call"].Outputs["answer"] != "ready\n" {
		t.Fatal(run)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creates != 1 || len(d.containers) != 0 {
		t.Fatal("nested container lifecycle", d.creates, len(d.containers))
	}
}

func TestUnfinishedSharedJobDoesNotReplayCompletedOrdinaryCommands(t *testing.T) {
	e, _, d := containerExecutor(t)
	source := `jobs: {build: {container: ubuntu:24.04, steps: [{id: completed, run: 'echo retained > /tmp/state'}, {id: failed, argv: [false]}]}}`
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/test.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err == nil || run == nil {
		t.Fatalf("%+v %v", run, err)
	}
	d.mu.Lock()
	creates := d.creates
	d.mu.Unlock()
	_, err = e.Resume(t.Context(), run.ID, nil, true)
	if err == nil || !strings.Contains(err.Error(), "lost transient container state") {
		t.Fatalf("unsafe recovery: %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creates != creates {
		t.Fatal("replayed ordinary commands in a new container")
	}
}

func TestSharedJobRefusesRecoveryAfterFailedCommands(t *testing.T) {
	for _, tc := range []struct {
		name, steps string
		wantExecs   int
	}{
		{"failed command", `[{id: failed, run: 'echo ready > /tmp/state; exit 1'}]`, 1},
		{"failed finalizer", `[{id: prepare, run: 'echo ready > /tmp/state', continue-on-error: true, finally: [{id: failed, run: exit 1}]}, {id: consume, run: 'cat /tmp/state; exit 1'}]`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, store, d := containerExecutor(t)
			writeWorkflow(t, e.WorkspaceDir, "test.yml", "jobs: {build: {container: ubuntu:24.04, steps: "+tc.steps+"}}")
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
			if err == nil || run == nil {
				t.Fatalf("command did not fail: run=%+v err=%v", run, err)
			}
			attempts, err := store.Attempts(t.Context(), run.ID)
			if err != nil || len(attempts) != tc.wantExecs {
				t.Fatalf("missing failed command receipts: attempts=%+v err=%v", attempts, err)
			}
			for _, a := range attempts {
				if a.Status != RunStatusFailed || a.FinishedAt.IsZero() {
					t.Fatalf("fixture must have only completed failed receipts: %+v", a)
				}
			}
			_, err = e.Resume(t.Context(), run.ID, nil, true)
			if err == nil || !strings.Contains(err.Error(), "lost transient container state") {
				t.Fatalf("resumed completed failed commands without transient state: %v", err)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.creates != 1 || d.executions != tc.wantExecs || len(d.containers) != 0 {
				t.Fatalf("recreated transient state: creates=%d execs=%d retained=%d", d.creates, d.executions, len(d.containers))
			}
		})
	}
}

func TestSharedContainerCleanupPrecedesJobCompletion(t *testing.T) {
	e, store, d := containerExecutor(t)
	d.removalStarted = make(chan struct{}, 1)
	d.removalGate = make(chan struct{})
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/test.yml"), []byte(`jobs: {build: {container: ubuntu:24.04, steps: [{run: 'echo ready'}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	var release sync.Once
	defer func() {
		cancel()
		release.Do(func() { close(d.removalGate) })
		select {
		case <-finished:
		case <-time.After(25 * time.Second):
			t.Error("container run did not stop before fixture teardown")
		}
	}()
	go func() {
		defer close(finished)
		_, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"})
		done <- err
	}()
	select {
	case <-d.removalStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not start")
	}
	var data []byte
	if err := store.db.QueryRow("SELECT record FROM runs WHERE status='running'").Scan(&data); err != nil {
		t.Fatal(err)
	}
	var current Run
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatal(err)
	}
	if _, terminal := current.Jobs["build"]; terminal {
		t.Error("published job completion before container absence")
	}
	release.Do(func() { close(d.removalGate) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish")
	}
}

func TestSharedContainerCleanupFailureCancelsQueuedJobsWithoutJournalFence(t *testing.T) {
	for _, mode := range []string{"parent", "sibling", "parent-launch", "sibling-launch", "standalone", "standalone-launch"} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%t", mode, nested), func(t *testing.T) {
				e, store, d := containerExecutor(t)
				d.removalError = true
				d.createResponseError = strings.HasSuffix(mode, "-launch")
				defer func() {
					d.mu.Lock()
					defer d.mu.Unlock()
					for _, c := range d.containers {
						select {
						case <-c.removed:
						default:
							close(c.removed)
						}
					}
				}()
				if _, err := store.db.Exec(`CREATE TRIGGER reject_uncertain BEFORE UPDATE OF state ON invocations WHEN NEW.state='uncertain' BEGIN SELECT RAISE(ABORT, 'cleanup checkpoint failed'); END`); err != nil {
					t.Fatal(err)
				}
				container, command := "container: ubuntu:24.04, ", "run: echo ready"
				if strings.HasPrefix(mode, "sibling") || strings.HasPrefix(mode, "standalone") {
					command = "uses: docker://ubuntu:24.04"
				}
				if strings.HasPrefix(mode, "standalone") {
					container = ""
				}
				job := "{" + container + "steps: [{" + command + "}]}\n"
				jobs := "  first: " + job + "  second: " + job
				if nested {
					writeWorkflow(t, e.WorkspaceDir, "child.yml", "on: {workflow_call: {}}\njobs:\n"+jobs)
					jobs = "  child: {uses: workflows/child.yml}\n"
				}
				writeWorkflow(t, e.WorkspaceDir, "test.yml", "jobs:\n"+jobs)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"})
				if err == nil || !strings.Contains(err.Error(), "cleanup checkpoint failed") || run == nil || run.Status != RunStatusNeedsAttention {
					t.Fatalf("cleanup and journal failure were not retained: run=%+v err=%v", run, err)
				}
				if ctx.Err() != nil {
					t.Fatalf("run required its outer deadline: %v", ctx.Err())
				}
				if len(e.Slots) != 0 {
					t.Fatal("canceled queued job retained capacity")
				}
				state := "running"
				if d.createResponseError {
					state = "intent"
				}
				var pending int
				if err := store.db.QueryRow("SELECT count(*) FROM invocations WHERE state=?", state).Scan(&pending); err != nil || pending != 1 {
					t.Fatalf("fixture did not retain the unfenced invocation: state=%s count=%d err=%v", state, pending, err)
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				wantExecs := 0
				if mode == "parent" {
					wantExecs = 1
				}
				if d.creates != 1 || d.executions != wantExecs {
					t.Fatalf("queued job executed after unconfirmed cleanup: creates=%d execs=%d", d.creates, d.executions)
				}
			})
		}
	}
}

func TestContainerGuardianFailureBlocksHandlersAndContinuation(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			e, store, d := containerExecutor(t)
			uncertain := false
			if shared {
				e.BeforeFinalize = func(ctx context.Context, id string) error {
					records, err := store.Invocations(ctx, id, false)
					for _, record := range records {
						if record.Kind == "container-exec" && record.State == "uncertain" && record.Error == "exec guardian cleanup is unconfirmed" {
							uncertain = true
						}
					}
					return err
				}
			}
			container, command, handlers := "", "uses: docker://ubuntu:24.04\n        with: {args: '-c \"exit 125\"'}", ""
			if shared {
				container, command = "    container: ubuntu:24.04\n", "run: exit 125"
				handlers = "        onError: [{id: handler, argv: [touch, handler-ran]}]\n"
			}
			source := "jobs:\n  build:\n" + container + `    steps:
      - id: unsafe
        ` + command + `
        continue-on-error: true
` + handlers + `
      - {id: next, argv: [touch, next-ran]}
`
			writeWorkflow(t, e.WorkspaceDir, "test.yml", source)
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
			if err == nil || run == nil || run.Status != RunStatusNeedsAttention {
				t.Fatalf("guardian failure was not hard: run=%+v err=%v", run, err)
			}
			if got := run.Jobs["build"].Steps["unsafe"].Error; strings.Count(got, "container guardian exited with code 125") != 1 {
				t.Fatalf("guardian failure diagnostic was duplicated or missing: %s", got)
			}
			for _, name := range []string{"handler-ran", "next-ran"} {
				if _, err := os.Stat(filepath.Join(e.WorkspaceDir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsafe continuation %s: %v", name, err)
				}
			}
			if shared {
				if !uncertain {
					t.Fatal("guardian cleanup uncertainty was discarded")
				}
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.containers) != 0 || d.creates != 1 || shared && d.executions != 1 {
				t.Fatalf("continued or leaked containers: creates=%d execs=%d retained=%d", d.creates, d.executions, len(d.containers))
			}
		})
	}
}

func TestSharedContainerStartupRespectsCommandDeadline(t *testing.T) {
	for _, phase := range []string{"create", "attach", "start", "running"} {
		t.Run(phase, func(t *testing.T) {
			e, _, d := containerExecutor(t)
			d.stallPhase, d.stallRelease = phase, make(chan struct{})
			if phase == "running" {
				d.startDelayInspections = -1
			}
			defer close(d.stallRelease)
			writeWorkflow(t, e.WorkspaceDir, "test.yml", `jobs: {build: {container: ubuntu:24.04, steps: [{id: first, timeout: 100ms, run: echo ready}]}}`)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			started := time.Now()
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"})
			if err == nil || run == nil || run.Jobs["build"].Steps["first"].FailureClass != "timeout" {
				t.Fatalf("startup was not timed out: run=%+v err=%v", run, err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second || ctx.Err() != nil {
				t.Fatalf("startup escaped step timeout: elapsed=%s outer=%v", elapsed, ctx.Err())
			}
			if len(e.Slots) != 0 {
				t.Fatal("startup failure leaked capacity")
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.containers) != 0 {
				t.Fatal("startup failure leaked container")
			}
		})
	}
}

func TestSharedContainerWaitsForRunningBeforeFirstExec(t *testing.T) {
	e, _, d := containerExecutor(t)
	d.startDelayInspections = 2
	writeWorkflow(t, e.WorkspaceDir, "test.yml", `jobs: {build: {container: ubuntu:24.04, steps: [{id: first, timeout: 3s, run: echo ready}]}}`)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"})
	if err != nil || run == nil || run.Jobs["build"].Steps["first"].Outputs["stdout"] != "ready\n" {
		t.Fatalf("exec raced parent readiness: run=%+v err=%v", run, err)
	}
	if len(e.Slots) != 0 {
		t.Fatal("capacity lease leaked")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.containers) != 0 || d.creates != 1 || d.executions != 1 {
		t.Fatalf("unexpected container lifecycle: creates=%d execs=%d retained=%d", d.creates, d.executions, len(d.containers))
	}
}

func TestSharedContainerStartupFailureAllowsCleanContinuation(t *testing.T) {
	for _, phase := range []string{"attach", "start", "running"} {
		for _, handler := range []string{"", "onError", "finally"} {
			t.Run(phase+"/"+handler, func(t *testing.T) {
				e, store, d := containerExecutor(t)
				d.stallPhase, d.stallRelease, d.stallOnce = phase, make(chan struct{}), true
				if phase == "running" {
					d.startDelayInspections = -1
				}
				defer close(d.stallRelease)
				first := "{id: first, timeout: 100ms, run: echo first, continue-on-error: true"
				wantExecs := 1
				if handler != "" {
					first += ", " + handler + ": [{id: recovered, run: echo recovered}]"
					wantExecs++
				}
				writeWorkflow(t, e.WorkspaceDir, "test.yml", "jobs: {build: {container: ubuntu:24.04, steps: ["+first+"}, {id: next, run: echo next}]}}")
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"})
				if err != nil || run == nil || run.Jobs["build"].Steps["next"].Outputs["stdout"] != "next\n" {
					t.Fatalf("startup failure poisoned continuation: run=%+v err=%v", run, err)
				}
				firstResult := run.Jobs["build"].Steps["first"]
				if handler == "onError" {
					if firstResult.Lifecycle == nil || firstResult.Lifecycle.OriginalClass != "timeout" {
						t.Fatalf("fixture missed startup timeout: %+v", firstResult)
					}
				} else if firstResult.FailureClass != "timeout" {
					t.Fatalf("fixture missed startup timeout: %+v", firstResult)
				}
				if handler != "" {
					if firstResult.Lifecycle == nil {
						t.Fatalf("missing handler lifecycle: %+v", firstResult)
					}
					results := firstResult.Lifecycle.Handlers[handler]
					if len(results) != 1 || results[0].Outputs["stdout"] != "recovered\n" {
						t.Fatalf("handler used an unready parent: %+v", firstResult)
					}
				}
				if len(e.Slots) != 0 {
					t.Fatal("startup recovery leaked capacity")
				}
				records, err := store.Invocations(t.Context(), run.ID, false)
				if err != nil {
					t.Fatal(err)
				}
				for _, record := range records {
					if record.State != "cleaned" {
						t.Fatalf("startup recovery left an unresolved invocation: %+v", record)
					}
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				if d.creates != 2 || d.executions != wantExecs || len(d.containers) != 0 {
					t.Fatalf("unexpected startup recovery: creates=%d execs=%d retained=%d", d.creates, d.executions, len(d.containers))
				}
			})
		}
	}
}

func TestSharedJobIgnoresSuccessfulReusableChildWithSameJobID(t *testing.T) {
	e, _, _ := containerExecutor(t)
	writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs: {build: {container: ubuntu:24.04, steps: [{run: echo child}]}}`)
	writeWorkflow(t, e.WorkspaceDir, "test.yml", `jobs:
  call: {uses: workflows/child.yml}
  build: {needs: call, container: ubuntu:24.04, steps: [{id: parent, run: echo parent}]}
`)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil || run == nil || run.Jobs["build"].Steps["parent"].Outputs["stdout"] != "parent\n" {
		t.Fatalf("child receipt blocked fresh parent: run=%+v err=%v", run, err)
	}
}

func TestSharedJobRefusesRecoveryAfterCompletedLifecycleTask(t *testing.T) {
	for _, status := range []string{RunStatusSucceeded, RunStatusFailed} {
		t.Run(status, func(t *testing.T) {
			command := "echo ready > /tmp/state"
			if status == RunStatusFailed {
				command += "; exit 1"
			}
			e, store, d := containerExecutor(t)
			writeWorkflow(t, e.WorkspaceDir, "test.yml", `jobs:
  build:
    container: ubuntu:24.04
    steps:
      - id: prepare
        run: `+command+`
        handlerTimeout: 5m
        finally: [{id: consume, run: cat /tmp/state}]
`)
			// Reject handler admission and final completion writes after the real task receipt.
			for _, statement := range []string{
				`CREATE TRIGGER stop_shared_handler BEFORE INSERT ON attempts WHEN NEW.step_id LIKE 'prepare/finally/%' BEGIN SELECT RAISE(ABORT, 'interrupted before shared handler'); END`,
				`CREATE TRIGGER stop_shared_completion BEFORE UPDATE OF record ON attempts WHEN json_extract(CAST(NEW.record AS TEXT),'$.phase')='lifecycle_completed' BEGIN SELECT RAISE(ABORT, 'interrupted before lifecycle completion'); END`,
			} {
				if _, err := store.db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
			if err == nil || run == nil {
				t.Fatalf("did not interrupt task receipt: run=%+v err=%v", run, err)
			}
			attempts, err := store.Attempts(t.Context(), run.ID)
			if err != nil || len(attempts) != 1 || attempts[0].Status != RunStatusRunning || attempts[0].Phase != "lifecycle_task_completed" || attempts[0].Result.Status != status {
				t.Fatalf("missing completed-task receipt: attempts=%+v err=%v", attempts, err)
			}
			for _, statement := range []string{"DROP TRIGGER stop_shared_handler", "DROP TRIGGER stop_shared_completion"} {
				if _, err := store.db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err = e.Resume(t.Context(), run.ID, nil, true)
			if err == nil || !strings.Contains(err.Error(), "lost transient container state") {
				t.Fatalf("resumed handlers without transient state: %v", err)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.creates != 1 || d.executions != 1 || len(d.containers) != 0 {
				t.Fatalf("recreated transient state: creates=%d execs=%d retained=%d", d.creates, d.executions, len(d.containers))
			}
		})
	}
}

func TestSharedCommandRendersWorkingDirectory(t *testing.T) {
	for _, dir := range []any{"subdir", 42} {
		t.Run(fmt.Sprint(dir), func(t *testing.T) {
			e, _, d := containerExecutor(t)
			if err := os.Mkdir(filepath.Join(e.WorkspaceDir, "subdir"), 0700); err != nil {
				t.Fatal(err)
			}
			inputType := "string"
			if dir != "subdir" {
				inputType = "number"
			}
			writeWorkflow(t, e.WorkspaceDir, "test.yml", `on: {workflow_call: {inputs: {dir: {type: `+inputType+`}}}}
jobs: {build: {container: ubuntu:24.04, steps: [{id: where, run: pwd, working-directory: '${{ inputs.dir }}'}]}}`)
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", Inputs: map[string]any{"dir": dir}})
			if dir == "subdir" {
				if err != nil || run == nil || run.Jobs["build"].Steps["where"].Outputs["stdout"] != filepath.Join(e.WorkspaceDir, "subdir")+"\n" {
					t.Fatalf("directory expression failed: run=%+v err=%v", run, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "working-directory must render to a string") {
					t.Fatalf("non-string directory accepted: run=%+v err=%v", run, err)
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				if d.creates != 0 {
					t.Fatal("invalid directory created a container")
				}
			}
		})
	}
}

func TestDockerCommandRendersWorkingDirectory(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, dir := range []any{"", "subdir", "/workspace/subdir", 42} {
			t.Run(fmt.Sprintf("shared=%t/dir=%v", shared, dir), func(t *testing.T) {
				e, _, d := containerExecutor(t)
				if err := os.Mkdir(filepath.Join(e.WorkspaceDir, "subdir"), 0700); err != nil {
					t.Fatal(err)
				}
				inputType, container := "string", ""
				if dir == 42 {
					inputType = "number"
				}
				if shared {
					container = "container: ubuntu:24.04, "
				}
				writeWorkflow(t, e.WorkspaceDir, "test.yml", `on: {workflow_call: {inputs: {dir: {type: `+inputType+`}}}}
jobs: {build: {`+container+`steps: [{id: where, uses: 'docker://ubuntu:24.04', working-directory: '${{ inputs.dir }}', with: {args: '-c pwd'}}]}}`)
				run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", Inputs: map[string]any{"dir": dir}})
				if dir == 42 {
					if err == nil || !strings.Contains(err.Error(), "working-directory must render to a string") {
						t.Fatalf("non-string directory accepted: run=%+v err=%v", run, err)
					}
				} else {
					want := e.WorkspaceDir
					if dir != "" {
						want = filepath.Join(want, "subdir")
					}
					if err != nil || run == nil || run.Jobs["build"].Steps["where"].Outputs["stdout"] != want+"\n" {
						t.Fatalf("Docker directory expression failed: run=%+v err=%v", run, err)
					}
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				if len(d.containers) != 0 || dir == 42 && d.creates != 0 {
					t.Fatalf("unexpected container effects: creates=%d retained=%d", d.creates, len(d.containers))
				}
			})
		}
	}
}
