package containerapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type podmanRuntime struct {
	selection  Selection
	mu         sync.Mutex
	attached   map[string]*cliAttachment
	executions map[string]*cliAttachment
}

func openPodman(s Selection) (*podmanRuntime, error) {
	if s.Executable == "" || len(s.Environment) == 0 {
		return nil, fmt.Errorf("missing retained Podman executable/environment")
	}
	return &podmanRuntime{selection: s, attached: map[string]*cliAttachment{}, executions: map[string]*cliAttachment{}}, nil
}
func (r *podmanRuntime) Close() error { return nil }
func (r *podmanRuntime) command(ctx context.Context, args ...string) *exec.Cmd {
	if r.selection.Endpoint != "" {
		args = append([]string{"--remote", "--url", r.selection.Endpoint}, args...)
	}
	c := exec.CommandContext(ctx, r.selection.Executable, args...)
	c.Env = r.selection.Environment
	return c
}
func (r *podmanRuntime) output(ctx context.Context, args ...string) ([]byte, error) {
	var out limitedBuffer
	c := r.command(ctx, args...)
	c.Stdout = &out
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("Podman %s: %w", args[0], err)
	}
	return out.Bytes(), nil
}
func (r *podmanRuntime) Identity(ctx context.Context) (string, error) {
	b, err := r.output(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return "", err
	}
	var v struct {
		Host struct {
			Hostname string `json:"hostname"`
		} `json:"host"`
		Store struct {
			GraphRoot string `json:"graphRoot"`
		} `json:"store"`
	}
	if json.Unmarshal(b, &v) != nil || v.Host.Hostname == "" || v.Store.GraphRoot == "" {
		return "", fmt.Errorf("invalid Podman identity")
	}
	b, _ = json.Marshal(v)
	return string(b), nil
}
func (r *podmanRuntime) ResolveImage(ctx context.Context, ref string) (Image, error) {
	b, err := r.output(ctx, "image", "inspect", ref)
	if err != nil {
		return Image{}, err
	}
	var v []struct {
		ID           string `json:"Id"`
		OS           string `json:"Os"`
		Architecture string
		Config       struct {
			Entrypoint, Cmd []string
			Labels          map[string]string
		}
	}
	if json.Unmarshal(b, &v) != nil || len(v) != 1 {
		return Image{}, fmt.Errorf("invalid Podman image metadata")
	}
	i := Image{ID: v[0].ID, OS: v[0].OS, Architecture: v[0].Architecture, Entrypoint: v[0].Config.Entrypoint, Cmd: v[0].Config.Cmd, Labels: v[0].Config.Labels}
	return i, ValidateImage(i)
}
func (r *podmanRuntime) Create(ctx context.Context, q CreateRequest) (string, error) {
	if !validContentID(q.Image) || q.User == "" || strings.Split(q.User, ":")[0] == "0" {
		return "", fmt.Errorf("immutable image and non-root user required")
	}
	args := []string{"create", "--interactive", "--name", q.Name, "--pull=never", "--restart=no", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--memory=4g", "--cpus=2", "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m", "--user", q.User, "--workdir", q.WorkingDir}
	if q.KeepID {
		args = append(args, "--userns=keep-id")
	}
	net := q.Network
	if net == "" {
		net = "none"
	}
	args = append(args, "--network", net)
	keys := make([]string, 0, len(q.Labels))
	for k := range q.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+q.Labels[k])
	}
	for _, target := range q.PrivatePaths {
		args = append(args, "--tmpfs", target+":ro,nosuid,nodev,noexec,size=4096,mode=000")
	}
	for _, m := range q.Mounts {
		if strings.ContainsAny(m.Source+m.Target, ",\n\r") {
			return "", fmt.Errorf("unsupported bind path")
		}
		v := "type=bind,src=" + m.Source + ",dst=" + m.Target
		if m.ReadOnly {
			v += ",ro=true"
		}
		args = append(args, "--mount", v)
	}
	for _, env := range q.Env {
		args = append(args, "--env", env)
	}
	if q.Entrypoint != nil {
		b, _ := json.Marshal(q.Entrypoint)
		args = append(args, "--entrypoint", string(b))
	}
	args = append(args, q.Image)
	args = append(args, q.Command...)
	b, err := r.output(ctx, args...)
	return strings.TrimSpace(string(b)), err
}

type cliAttachment struct {
	mu        sync.Mutex
	command   *exec.Cmd
	container string
	started   chan struct{}
	done      chan struct{}
	code      int
	err       error
	once      sync.Once
}

func (a *cliAttachment) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.command.Process != nil {
		return a.command.Process.Kill()
	}
	return nil
}
func (a *cliAttachment) Copy(s Streams) error {
	a.command.Stdin = s.Stdin
	a.command.Stdout = s.Stdout
	a.command.Stderr = s.Stderr
	a.once.Do(func() {
		a.mu.Lock()
		a.err = a.command.Start()
		close(a.started)
		a.mu.Unlock()
		if a.err == nil {
			a.err = a.command.Wait()
			a.code = a.command.ProcessState.ExitCode()
			// Podman forwards workload exits through its process status. Reserved
			// CLI/launch statuses (125-127) and signals remain errors.
			var exit *exec.ExitError
			if errors.As(a.err, &exit) && a.code >= 0 && (a.code < 125 || a.code > 127) {
				a.err = nil
			}
		}
		close(a.done)
	})
	<-a.done
	return a.err
}
func (r *podmanRuntime) Attach(ctx context.Context, id string) (Attachment, error) {
	a := &cliAttachment{command: r.command(ctx, "start", "--attach", "--interactive", id), container: id, started: make(chan struct{}), done: make(chan struct{})}
	r.mu.Lock()
	r.attached[id] = a
	r.mu.Unlock()
	return a, nil
}
func (r *podmanRuntime) Start(ctx context.Context, id string) error {
	r.mu.Lock()
	a := r.attached[id]
	r.mu.Unlock()
	if a == nil {
		_, err := r.output(ctx, "start", id)
		return err
	}
	select {
	case <-a.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *podmanRuntime) Wait(ctx context.Context, id string) (int, error) {
	b, err := r.output(ctx, "wait", id)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
func (r *podmanRuntime) ExecCreate(ctx context.Context, id string, q ExecRequest) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	key := hex.EncodeToString(token[:])
	args := []string{"exec", "--interactive"}
	if q.User != "" {
		args = append(args, "--user", q.User)
	}
	if q.WorkingDir != "" {
		args = append(args, "--workdir", q.WorkingDir)
	}
	for _, env := range q.Env {
		args = append(args, "--env", env)
	}
	args = append(args, id)
	args = append(args, q.Command...)
	a := &cliAttachment{command: r.command(ctx, args...), container: id, started: make(chan struct{}), done: make(chan struct{})}
	r.mu.Lock()
	r.executions[key] = a
	r.mu.Unlock()
	return key, nil
}
func (r *podmanRuntime) ExecAttach(ctx context.Context, id string) (Attachment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.executions[id]
	if a == nil {
		return nil, fmt.Errorf("unknown retained Podman exec; parent cleanup required")
	}
	return a, nil
}
func (r *podmanRuntime) ExecInspect(ctx context.Context, id string) (ExecStatus, error) {
	r.mu.Lock()
	a := r.executions[id]
	r.mu.Unlock()
	if a == nil {
		return ExecStatus{}, fmt.Errorf("unknown Podman exec")
	}
	s := ExecStatus{ID: id, ContainerID: a.container, Running: true}
	select {
	case <-a.done:
		s.Running = false
		s.ExitCode = a.code
	default:
	}
	return s, nil
}
func (r *podmanRuntime) Inspect(ctx context.Context, id string) (Object, bool, error) {
	b, err := r.output(ctx, "container", "inspect", id)
	if err != nil {
		// A successful complete listing can establish absence. Failed commands cannot.
		list, e := r.output(ctx, "ps", "--all", "--no-trunc", "--format", "{{.ID}} {{.Names}}")
		if e != nil {
			return Object{}, false, e
		}
		for _, line := range strings.Split(string(list), "\n") {
			for _, part := range strings.Fields(line) {
				for _, name := range strings.Split(part, ",") {
					if name == id {
						return Object{}, true, err
					}
				}
			}
		}
		return Object{}, false, nil
	}
	var v []struct {
		ID     string `json:"Id"`
		Name   string
		Config struct{ Labels map[string]string }
		State  struct{ Running bool }
	}
	if json.Unmarshal(b, &v) != nil || len(v) != 1 || v[0].ID == "" {
		return Object{}, true, fmt.Errorf("invalid Podman inspection")
	}
	return Object{ID: v[0].ID, Name: v[0].Name, Labels: v[0].Config.Labels, Running: v[0].State.Running}, true, nil
}
func (r *podmanRuntime) Remove(ctx context.Context, id string) error {
	_, err := r.output(ctx, "rm", "--force", id)
	return err
}

// Resolve selects a local endpoint once. No image pull, login or daemon resource
// creation is performed. Podman's CLI environment is retained without credentials.
func Resolve(ctx context.Context, name string, endpoint string) (Selection, error) {
	s := Selection{}
	switch name {
	case "docker":
		s.Transport = DockerSDK
		s.Endpoint = endpoint
		if s.Endpoint == "" {
			s.Endpoint = os.Getenv("DOCKER_HOST")
		}
		if s.Endpoint == "" || os.Getenv("DOCKER_CONTEXT") != "" && endpoint == "" {
			b, err := Output(ctx, "docker", "context", "inspect")
			if err != nil {
				return s, err
			}
			var v []struct {
				Endpoints map[string]struct{ Host string }
			}
			if json.Unmarshal(b, &v) != nil || len(v) != 1 {
				return s, fmt.Errorf("invalid Docker context")
			}
			s.Endpoint = v[0].Endpoints["docker"].Host
		}
	case "", "podman":
		s.Transport = PodmanCLI
		s.Endpoint = endpoint
		var err error
		s.Executable, err = exec.LookPath("podman")
		if err != nil {
			return s, err
		}
		// Pin all variables used by local Podman to select a store/connection. Remove
		// controller/provider credentials and all ambient connection selectors.
		for _, key := range []string{"PATH", "HOME", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "TMPDIR"} {
			if v, ok := os.LookupEnv(key); ok {
				s.Environment = append(s.Environment, key+"="+v)
			}
		}
		if s.Endpoint == "" {
			s.Endpoint = os.Getenv("CONTAINER_HOST")
		}
		if s.Endpoint == "" && os.Getenv("CONTAINER_CONNECTION") != "" {
			return s, fmt.Errorf("Podman named connections require an explicit retained endpoint")
		}
	default:
		return s, fmt.Errorf("unsupported container runtime %q", name)
	}
	r, err := Open(s)
	if err != nil {
		return s, err
	}
	defer r.Close()
	s.Identity, err = r.Identity(ctx)
	return s, err
}
