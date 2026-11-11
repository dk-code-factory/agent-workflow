package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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

type fakeContainer struct {
	id, name, dir          string
	labels                 map[string]string
	argv                   []string
	resident               bool
	running                bool
	started, done, removed chan struct{}
	code                   int
	mu                     sync.Mutex
}
type fakeExec struct {
	id      string
	parent  *fakeContainer
	argv    []string
	dir     string
	running bool
	code    int
}
type workflowDocker struct {
	t                   *testing.T
	mu                  sync.Mutex
	containers          map[string]*fakeContainer
	execs               map[string]*fakeExec
	root, workspace     string
	creates, executions int
	removalStarted      chan struct{}
	removalGate         chan struct{}
}

func (d *workflowDocker) command(c *fakeContainer, argv []string, input []byte, dir string) ([]byte, []byte, int) {
	args := append([]string(nil), argv...)
	for i, a := range args {
		a = strings.ReplaceAll(a, "/tmp/", c.dir+"/")
		a = strings.ReplaceAll(a, "/workspace", d.workspace)
		args[i] = a
	}
	if len(args) == 0 {
		return nil, []byte("no command"), 127
	}
	cmd := exec.Command(args[0], args[1:]...)
	if dir == "" {
		dir = "/workspace"
	}
	if dir == "/workspace" || strings.HasPrefix(dir, "/workspace/") {
		dir = d.workspace + strings.TrimPrefix(dir, "/workspace")
	}
	cmd.Dir = dir
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
	if p == "/_ping" {
		w.Header().Set("API-Version", "1.56")
		return
	}
	if p == "/info" {
		fmt.Fprint(w, `{"ID":"workflow-fixture"}`)
		return
	}
	if strings.HasPrefix(p, "/images/") {
		fmt.Fprintf(w, `{"Id":%q,"Os":"linux","Architecture":"amd64","Config":{"Entrypoint":["/bin/sh"],"Cmd":["-c","printf baked"]}}`, "sha256:"+strings.Repeat("a", 64))
		return
	}
	if p == "/containers/create" {
		var q struct {
			Cmd    []string
			Labels map[string]string
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
		c := &fakeContainer{id: id, name: r.URL.Query().Get("name"), dir: dir, labels: q.Labels, argv: argv, resident: len(argv) > 1 && argv[1] == "__container_idle_v1", started: make(chan struct{}), done: make(chan struct{}), removed: make(chan struct{})}
		d.containers[id] = c
		d.containers[c.name] = c
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
		conn, err := fakeStream(w)
		if err != nil {
			d.t.Error(err)
			return
		}
		defer conn.Close()
		input, _ := io.ReadAll(conn)
		stdout, stderr, code := d.command(e.parent, e.argv, input, e.dir)
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
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": c.id, "Name": "/" + c.name, "Config": map[string]any{"Labels": c.labels}, "State": map[string]any{"Running": c.running}})
	case "start":
		c.mu.Lock()
		c.running = true
		c.mu.Unlock()
		close(c.started)
		w.WriteHeader(204)
	case "attach":
		conn, err := fakeStream(w)
		if err != nil {
			d.t.Error(err)
			return
		}
		defer conn.Close()
		input, _ := io.ReadAll(conn)
		<-c.started
		if c.resident {
			<-c.removed
			c.mu.Lock()
			c.running = false
			c.mu.Unlock()
			close(c.done)
			return
		}
		stdout, stderr, code := d.command(c, c.argv, input, "")
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
		var q struct {
			Cmd        []string
			WorkingDir string
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			d.t.Error(err)
		}
		argv := q.Cmd[5:]
		d.mu.Lock()
		defer d.mu.Unlock()
		d.executions++
		id := fmt.Sprintf("exec-%d", d.executions)
		d.execs[id] = &fakeExec{id: id, parent: c, argv: argv, dir: q.WorkingDir, running: false}
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

func TestSharedContainerCleanupPrecedesJobCompletion(t *testing.T) {
	e, store, d := containerExecutor(t)
	d.removalStarted = make(chan struct{}, 1)
	d.removalGate = make(chan struct{})
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/test.yml"), []byte(`jobs: {build: {container: ubuntu:24.04, steps: [{run: 'echo ready'}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/test.yml"}); done <- err }()
	select {
	case <-d.removalStarted:
	case <-time.After(5 * time.Second):
		close(d.removalGate)
		t.Fatal("cleanup did not start")
	}
	var data []byte
	if err := store.db.QueryRow("SELECT record FROM runs WHERE status='running'").Scan(&data); err != nil {
		close(d.removalGate)
		t.Fatal(err)
	}
	var current Run
	if err := json.Unmarshal(data, &current); err != nil {
		close(d.removalGate)
		t.Fatal(err)
	}
	if _, terminal := current.Jobs["build"]; terminal {
		t.Error("published job completion before container absence")
	}
	close(d.removalGate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish")
	}
}
