package containerapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

type dockerRuntime struct{ client *client.Client }

func openDocker(s Selection) (*dockerRuntime, error) {
	// Unix sockets avoid ambient TLS/proxy credentials and cannot silently select
	// a different context. Remote Docker endpoints need an explicit future policy.
	if !strings.HasPrefix(s.Endpoint, "unix:///") {
		return nil, fmt.Errorf("Docker API execution requires a retained local Unix socket")
	}
	c, err := client.New(client.WithHost(s.Endpoint)) // negotiation enabled by default
	if err != nil {
		return nil, err
	}
	return &dockerRuntime{c}, nil
}
func (r *dockerRuntime) Close() error { return r.client.Close() }
func (r *dockerRuntime) Identity(ctx context.Context) (string, error) {
	v, err := r.client.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", err
	}
	if v.Info.ID == "" {
		return "", fmt.Errorf("Docker daemon has no identity")
	}
	return v.Info.ID, nil
}
func (r *dockerRuntime) ResolveImage(ctx context.Context, ref string) (Image, error) {
	v, err := r.client.ImageInspect(ctx, ref)
	if err != nil {
		return Image{}, err
	}
	if v.Config == nil {
		return Image{}, fmt.Errorf("incomplete image inspection")
	}
	i := Image{ID: v.ID, OS: v.Os, Architecture: v.Architecture, Entrypoint: v.Config.Entrypoint, Cmd: v.Config.Cmd, Labels: v.Config.Labels}
	return i, ValidateImage(i)
}
func (r *dockerRuntime) Create(ctx context.Context, q CreateRequest) (string, error) {
	if !validContentID(q.Image) {
		return "", fmt.Errorf("container create requires immutable image content identity")
	}
	if q.User == "" || strings.Split(q.User, ":")[0] == "0" {
		return "", fmt.Errorf("container create requires non-root user")
	}
	pids := int64(256)
	h := &container.HostConfig{ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, NetworkMode: container.NetworkMode(q.Network), Tmpfs: map[string]string{"/tmp": "rw,nosuid,nodev,size=512m"}, Resources: container.Resources{PidsLimit: &pids, Memory: 4 << 30, NanoCPUs: 2_000_000_000}}
	if q.Network == "" {
		h.NetworkMode = "none"
	}
	for _, target := range q.PrivatePaths {
		h.Tmpfs[target] = "ro,nosuid,nodev,noexec,size=4096,mode=000"
	}
	for _, m := range q.Mounts {
		h.Mounts = append(h.Mounts, mount.Mount{Type: mount.TypeBind, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	v, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{Name: q.Name, Config: &container.Config{Image: q.Image, User: q.User, WorkingDir: q.WorkingDir, Entrypoint: q.Entrypoint, Cmd: q.Command, Env: q.Env, Labels: q.Labels, OpenStdin: true, StdinOnce: true, AttachStdin: true, AttachStdout: true, AttachStderr: true}, HostConfig: h})
	return v.ID, err
}

type dockerAttachment struct{ response client.HijackedResponse }

func (a *dockerAttachment) Close() error { a.response.Close(); return nil }
func (a *dockerAttachment) Copy(s Streams) error {
	if s.Stdout == nil {
		s.Stdout = io.Discard
	}
	if s.Stderr == nil {
		s.Stderr = io.Discard
	}
	input := make(chan error, 1)
	go func() {
		var err error
		if s.Stdin != nil {
			_, err = io.Copy(a.response.Conn, s.Stdin)
		}
		_ = a.response.CloseWrite()
		input <- err
	}()
	_, err := stdcopy.StdCopy(s.Stdout, s.Stderr, a.response.Reader)
	// Output EOF closes the socket even if the workload did not consume stdin.
	a.response.Close()
	return errors.Join(err, <-input)
}
func (r *dockerRuntime) Attach(ctx context.Context, id string) (Attachment, error) {
	v, err := r.client.ContainerAttach(ctx, id, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, err
	}
	return &dockerAttachment{v.HijackedResponse}, nil
}
func (r *dockerRuntime) Start(ctx context.Context, id string) error {
	_, err := r.client.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}
func (r *dockerRuntime) Wait(ctx context.Context, id string) (int, error) {
	w := r.client.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	// The SDK owns cancellation; consume its result/error so its unbuffered
	// decoder channel cannot remain blocked after a simultaneous cancellation.
	select {
	case err := <-w.Error:
		if err == nil {
			err = fmt.Errorf("container wait closed without status")
		}
		return -1, err
	case v := <-w.Result:
		if v.Error != nil {
			return -1, fmt.Errorf("container wait: %s", v.Error.Message)
		}
		return int(v.StatusCode), nil
	}
}
func (r *dockerRuntime) ExecCreate(ctx context.Context, id string, q ExecRequest) (string, error) {
	v, err := r.client.ExecCreate(ctx, id, client.ExecCreateOptions{Cmd: q.Command, Env: q.Env, WorkingDir: q.WorkingDir, User: q.User, AttachStdin: true, AttachStdout: true, AttachStderr: true})
	return v.ID, err
}
func (r *dockerRuntime) ExecAttach(ctx context.Context, id string) (Attachment, error) {
	v, err := r.client.ExecAttach(ctx, id, client.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	return &dockerAttachment{v.HijackedResponse}, nil
}
func (r *dockerRuntime) ExecInspect(ctx context.Context, id string) (ExecStatus, error) {
	v, err := r.client.ExecInspect(ctx, id, client.ExecInspectOptions{})
	return ExecStatus{ID: v.ID, ContainerID: v.ContainerID, Running: v.Running, ExitCode: v.ExitCode}, err
}
func (r *dockerRuntime) Inspect(ctx context.Context, id string) (Object, bool, error) {
	v, err := r.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, err
	}
	o := v.Container
	if o.Config == nil || o.State == nil {
		return Object{}, true, fmt.Errorf("incomplete container inspection")
	}
	return Object{ID: o.ID, Name: o.Name, Labels: o.Config.Labels, Running: o.State.Running}, true, nil
}
func (r *dockerRuntime) Remove(ctx context.Context, id string) error {
	_, err := r.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	return err
}
