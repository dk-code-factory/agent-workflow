// Package network implements the explicitly prepared shared network runtime.
// Selection never creates or starts a runtime. References fence every request
// against replacement of the selected service.
package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const Registry = "/var/lib/agent-workflow/runtimes"
const RunRoot = "/run/agent-workflow-runtimes"

// ReferenceEnvironment carries the nonsecret reference admitted before provider launch.
const ReferenceEnvironment = "AGENT_WORKFLOW_NETWORK_REFERENCE"

const launchEnvironment = "AGENT_WORKFLOW_NETWORK_REQUEST"

// Linux native argv/environment aggregate limits fit within this JSON bound,
// including worst-case escaping. Payloads never travel in a socket packet.
const maxLaunchPayload = 64 << 20

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,23}$`)

type Reference struct {
	Name       string `json:"name"`
	Identity   string `json:"identity"`
	Generation uint64 `json:"generation"`
	Endpoint   string `json:"endpoint"`
}

func (r Reference) Validate() error {
	if !validName.MatchString(r.Name) || len(r.Identity) != 32 || strings.Trim(r.Identity, "0123456789abcdef") != "" || r.Generation == 0 || r.Endpoint != Endpoint(r.Name, r.Identity) {
		return errors.New("invalid network runtime reference")
	}
	return nil
}
func Endpoint(name, identity string) string {
	return "unix://" + RunRoot + "/" + name + "/" + identity + "/podman.sock"
}
func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return errors.New("network-runtime must be a simple name (1–24 letters, digits, _ or -)")
	}
	return nil
}

type Membership struct {
	ID       string `json:"id"`
	UID      uint32 `json:"uid"`
	State    string `json:"state"`
	ExitCode int    `json:"exit_code"`
}
type Forward struct {
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	UID       uint32 `json:"uid"`
	ID        int    `json:"id"`
}
type Status struct {
	Reference         Reference             `json:"reference"`
	Healthy           bool                  `json:"healthy"`
	NetworkNamespace  string                `json:"network_namespace"`
	Members           map[string]Membership `json:"members"`
	Forwards          []Forward             `json:"forwards"`
	MemberCount       int                   `json:"member_count"`
	ForwardCount      int                   `json:"forward_count"`
	MembersTruncated  bool                  `json:"members_truncated"`
	ForwardsTruncated bool                  `json:"forwards_truncated"`
}
type Request struct {
	Operation string    `json:"operation"`
	Reference Reference `json:"reference"`
	ID        string    `json:"id,omitempty"`
	Path      string    `json:"path,omitempty"`
	Args      []string  `json:"args,omitempty"`
	Env       []string  `json:"env,omitempty"`
	Dir       string    `json:"dir,omitempty"`
	Forward   Forward   `json:"forward,omitempty"`
	Umask     int       `json:"umask,omitempty"`
	Terminal  bool      `json:"terminal,omitempty"`
}
type Response struct {
	Status Status     `json:"status"`
	Member Membership `json:"member"`
	Error  string     `json:"error,omitempty"`
}

func connect(ctx context.Context, name string) (*net.UnixConn, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unixpacket", RunRoot+"/"+name+"/control.sock")
	if err != nil {
		return nil, fmt.Errorf("network runtime %s unavailable: %w", name, err)
	}
	u := c.(*net.UnixConn)
	if err = trustedConnection(u); err != nil {
		u.Close()
		return nil, err
	}
	return u, nil
}
func Call(ctx context.Context, name string, req Request) (Response, error) {
	var out Response
	c, err := connect(ctx, name)
	if err != nil {
		return out, err
	}
	defer c.Close()
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)
	data, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	if _, err = c.Write(data); err != nil {
		return out, err
	}
	data = make([]byte, 1<<20)
	n, err := c.Read(data)
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(data[:n], &out); err != nil {
		return out, err
	}
	if out.Error != "" {
		return out, errors.New(out.Error)
	}
	return out, nil
}
func Resolve(ctx context.Context, name string) (Reference, error) {
	out, err := Call(ctx, name, Request{Operation: "status"})
	if err != nil {
		return Reference{}, err
	}
	if !out.Status.Healthy {
		return Reference{}, errors.New("network runtime is unhealthy")
	}
	return out.Status.Reference, out.Status.Reference.Validate()
}
func Check(ctx context.Context, ref Reference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	out, err := Call(ctx, ref.Name, Request{Operation: "check", Reference: ref})
	if err != nil {
		return err
	}
	if !out.Status.Healthy {
		return errors.New("network runtime is unhealthy")
	}
	return nil
}

// CheckRetained fences owned recovery against replacement without admitting new work.
func CheckRetained(ctx context.Context, ref Reference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	_, err := Call(ctx, ref.Name, Request{Operation: "cleanup-check", Reference: ref})
	return err
}

func Completed(ctx context.Context, ref Reference, id string) error {
	out, err := Call(ctx, ref.Name, Request{Operation: "member", Reference: ref, ID: id})
	if err != nil {
		return err
	}
	if out.Member.State != "cleaned" {
		return errors.New("network invocation cleanup is unconfirmed")
	}
	return nil
}

// Environment replaces inherited container selectors with the admitted endpoint.
func Environment(env []string, ref Reference) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+4)
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		switch k {
		case "DOCKER_HOST", "CONTAINER_HOST", "DOCKER_CONTEXT", "CONTAINER_CONNECTION", "AGENT_WORKFLOW_NETWORK_RUNTIME", ReferenceEnvironment, launchEnvironment, "AGENT_WORKFLOW_NETWORK_TERMINAL":
			continue
		}
		out = append(out, v)
	}
	data, _ := json.Marshal(ref) // Reference contains only strings and an integer.
	return append(out, "DOCKER_HOST="+ref.Endpoint, "CONTAINER_HOST="+ref.Endpoint, "AGENT_WORKFLOW_NETWORK_RUNTIME="+ref.Name, ReferenceEnvironment+"="+string(data))
}

// Wrap rewrites an unstarted command to the shared launch client. Its streams,
// environment, directory and context remain owned by exec.Cmd. The service uses
// SO_PEERCRED, never caller-supplied user IDs. Run the returned cleanup after
// Wait/Run, including when Start fails.
func Wrap(cmd *exec.Cmd, ref Reference, id string) (func(), error) {
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	if len(cmd.ExtraFiles) > 0 {
		return nil, errors.New("contained launch does not support extra inherited descriptors")
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if cmd.Dir == "" {
		cmd.Dir, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	req := Request{Operation: "exec", Reference: ref, ID: id, Path: cmd.Path, Args: append([]string(nil), cmd.Args[1:]...), Env: Environment(cmd.Env, ref), Dir: cmd.Dir}
	for _, value := range cmd.Environ() {
		if value == "AGENT_WORKFLOW_NETWORK_TERMINAL=1" {
			req.Terminal = true
		}
	}
	payload, err := sealedPayload(req)
	if err != nil {
		return nil, err
	}
	cmd.Env = Environment(cmd.Env, ref)
	cmd.ExtraFiles = []*os.File{payload}
	cmd.Path = executable
	cmd.Err = nil
	cmd.Args = []string{executable, "__network_runtime_client_v1"}
	return func() { _ = payload.Close() }, nil
}

// Reserve fences container creation against busy stop. Recovery releases this
// durable reservation only after exact container ownership/absence is proved.
func Reserve(ctx context.Context, ref Reference, id string) error {
	_, err := Call(ctx, ref.Name, Request{Operation: "reserve", Reference: ref, ID: id})
	return err
}
func Release(ctx context.Context, ref Reference, id string) error {
	_, err := Call(ctx, ref.Name, Request{Operation: "release", Reference: ref, ID: id})
	return err
}
