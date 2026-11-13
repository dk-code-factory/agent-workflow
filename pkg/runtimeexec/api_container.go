package runtimeexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

// ContainerSession owns an inert-create/attach/start/wait/remove lifecycle. It is
// also used by resident job containers. Close verifies absence before returning.
type ContainerSession struct {
	Runtime      containerapi.Runtime
	Invocation   *Invocation
	Owner        containerapi.Owner
	stopGuardian func() error
	once         sync.Once
	closeErr     error
}
type LaunchOptions struct {
	Policy                 Policy
	Selection              containerapi.Selection
	Image                  containerapi.Image
	Guardian               GuardianAsset
	Workspace, State, Root string
	Command, Env           []string
	WorkingDir             string
	Actions                string
	EngineState            string
	TaskStateDir           string
}

func containerIdentity(p Policy) (int, int) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid = 65532
	}
	if gid == 0 {
		gid = 65532
	}
	if p.UID != nil {
		uid = *p.UID
	}
	if p.GID != nil {
		gid = *p.GID
	}
	return uid, gid
}

func BuildRequest(o LaunchOptions, owner containerapi.Owner, lifetime string) (containerapi.CreateRequest, error) {
	if err := o.Policy.Validate(); err != nil {
		return containerapi.CreateRequest{}, err
	}
	if err := containerapi.ValidateImage(o.Image); err != nil {
		return containerapi.CreateRequest{}, err
	}
	if err := o.Guardian.Verify(); err != nil {
		return containerapi.CreateRequest{}, err
	}
	if o.Guardian.Platform != o.Image.OS+"/"+o.Image.Architecture {
		return containerapi.CreateRequest{}, fmt.Errorf("guardian platform %q does not match admitted image platform %s/%s", o.Guardian.Platform, o.Image.OS, o.Image.Architecture)
	}
	uid, gid := containerIdentity(o.Policy)
	q := containerapi.CreateRequest{Name: owner.Name, Image: o.Image.ID, User: strconv.Itoa(uid) + ":" + strconv.Itoa(gid), WorkingDir: "/workspace", Labels: owner.Labels, Entrypoint: []string{MountedGuardian}, Command: append([]string{"__container_workload_guardian_v1", "/engine-lifetime/lease", owner.Labels["agent-workflow.token"], "--"}, o.Command...), KeepID: o.Selection.Transport == containerapi.PodmanCLI && o.Selection.Endpoint == "", Network: o.Policy.NetworkMode, Env: []string{"HOME=/state/home", "TMPDIR=/tmp", "AGENT_WORKFLOW_WORKSPACE_WRITABLE=" + strconv.FormatBool(o.Policy.Writable)}}
	if o.WorkingDir != "" {
		q.WorkingDir = o.WorkingDir
	}
	for _, value := range o.Env {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" || key == "AGENT_WORKFLOW_WORKSPACE_WRITABLE" || strings.HasPrefix(key, "AGENT_WORKFLOW_") {
			return q, fmt.Errorf("invalid/reserved container environment key %q", key)
		}
		q.Env = append(q.Env, value)
	}
	workspace, err := filepath.Abs(o.Workspace)
	if err != nil {
		return q, err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return q, err
	}
	// The workspace bind must not expose the engine database, credentials,
	// ownership records or general management socket through another mount path.
	inside := func(root, target string) (string, bool) {
		rel, err := filepath.Rel(root, target)
		return rel, err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if strings.HasPrefix(o.Selection.Endpoint, "unix://") {
		endpoint := strings.TrimPrefix(o.Selection.Endpoint, "unix://")
		if real, err := filepath.EvalSymlinks(endpoint); err == nil {
			endpoint = real
		}
		if _, ok := inside(workspace, endpoint); ok {
			return q, fmt.Errorf("container runtime socket must be outside the workspace mount")
		}
	}
	private := map[string]bool{}
	for _, directory := range []string{o.EngineState, filepath.Join(workspace, ".agent-workflow")} {
		if directory == "" {
			continue
		}
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return q, err
		}
		if real, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = real
		} else if !os.IsNotExist(err) {
			return q, err
		}
		if rel, ok := inside(workspace, absolute); ok {
			if rel == "." {
				return q, fmt.Errorf("workspace cannot be the engine state directory")
			}
			if _, err := os.Lstat(absolute); err == nil {
				private[filepath.ToSlash(filepath.Join("/workspace", rel))] = true
			} else if !os.IsNotExist(err) {
				return q, err
			}
		}
	}
	for target := range private {
		covered := false
		for ancestor := range private {
			if ancestor != target && strings.HasPrefix(target, ancestor+"/") {
				covered = true
				break
			}
		}
		if !covered {
			q.PrivatePaths = append(q.PrivatePaths, target)
		}
	}
	sort.Strings(q.PrivatePaths)
	if info, err := os.Lstat(filepath.Join(workspace, ".git")); err == nil && !info.IsDir() {
		return q, fmt.Errorf("containers require independent Git administration, not a host worktree")
	} else if err != nil && !os.IsNotExist(err) {
		return q, err
	}
	bind := func(source, target string, ro bool) error {
		path, err := filepath.Abs(source)
		if err != nil {
			return err
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			home, err := os.UserHomeDir()
			if err == nil {
				home, _ = filepath.EvalSymlinks(home)
				rel, e := filepath.Rel(path, home)
				if e == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
					return fmt.Errorf("host home directories and ancestors cannot be mounted")
				}
			}
		}
		q.Mounts = append(q.Mounts, containerapi.Mount{Source: path, Target: target, ReadOnly: ro})
		return nil
	}
	for _, m := range []containerapi.Mount{{Source: workspace, Target: "/workspace", ReadOnly: !o.Policy.Writable}, {Source: o.State, Target: "/state"}, {Source: lifetime, Target: "/engine-lifetime", ReadOnly: true}, {Source: o.Guardian.Path, Target: MountedGuardian, ReadOnly: true}} {
		if err = bind(m.Source, m.Target, m.ReadOnly); err != nil {
			return q, err
		}
	}
	if o.Actions != "" {
		if err = bind(o.Actions, "/actions", true); err != nil {
			return q, err
		}
	}
	if o.TaskStateDir != "" {
		info, err := os.Lstat(o.TaskStateDir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return q, fmt.Errorf("task-state mount requires a real directory without group/world write permission")
		}
		entries, err := os.ReadDir(o.TaskStateDir)
		if err != nil || len(entries) != 1 || entries[0].Name() != "rpc.sock" || entries[0].Type()&os.ModeSocket == 0 {
			return q, fmt.Errorf("task-state mount must contain only its RPC socket")
		}
		if err = bind(o.TaskStateDir, "/engine-task-state", true); err != nil {
			return q, err
		}
	}
	if o.Policy.Network != nil {
		return q, fmt.Errorf("typed container launch with network-runtime is not supported yet")
	}
	return q, nil
}
func LaunchContainer(ctx context.Context, o LaunchOptions) (session *ContainerSession, returned error) {
	r, err := containerapi.Open(o.Selection)
	if err != nil {
		return nil, err
	}
	if err = containerapi.CheckSelection(ctx, r, o.Selection); err != nil {
		r.Close()
		return nil, err
	}
	image, err := r.ResolveImage(ctx, o.Image.ID)
	if err != nil || image.ID != o.Image.ID || image.OS != o.Image.OS || image.Architecture != o.Image.Architecture {
		r.Close()
		return nil, fmt.Errorf("retained image unavailable or changed: %w", err)
	}
	i, err := Admit(ctx, "container-api")
	if err != nil {
		r.Close()
		return nil, err
	}
	if i == nil {
		r.Close()
		return nil, fmt.Errorf("container execution requires durable invocation ownership")
	}
	root, err := filepath.Abs(o.Root)
	if err != nil {
		r.Close()
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		r.Close()
		return nil, err
	}
	hash := sha256.Sum256([]byte(root))
	owner := containerapi.Owner{Selection: &o.Selection, Name: "agent-workflow-" + i.Record.ID, Labels: map[string]string{"agent-workflow.managed": "true", "agent-workflow.run": i.Record.RunID, "agent-workflow.generation": strconv.FormatInt(i.Record.Generation, 10), "agent-workflow.invocation": i.Record.ID, "agent-workflow.workspace": hex.EncodeToString(hash[:]), "agent-workflow.token": i.Record.Token}}
	i.Record.Container = &owner
	if err = i.Save(ctx, "intent"); err != nil {
		r.Close()
		return nil, err
	}
	o.EngineState = i.binding.Dir
	q, err := BuildRequest(o, owner, i.Directory())
	if err != nil {
		r.Close()
		_ = i.Save(context.WithoutCancel(ctx), "cleaned")
		return nil, err
	}
	ids := strings.Split(q.User, ":")
	uid, _ := strconv.Atoi(ids[0])
	gid, _ := strconv.Atoi(ids[1])
	if err = i.setLifetimeOwner(uid, gid); err != nil {
		r.Close()
		return nil, err
	}
	if os.Getuid() == 0 {
		for _, path := range []string{o.State, filepath.Join(o.State, "home")} {
			if err = os.Chown(path, uid, gid); err != nil {
				r.Close()
				return nil, err
			}
		}
	}
	session = &ContainerSession{Runtime: r, Invocation: i, Owner: owner}
	defer func() {
		if returned != nil {
			returned = errors.Join(returned, session.Close())
			session = nil
		}
	}()
	session.stopGuardian, err = process.OwnContainer(owner)
	if err != nil {
		return session, err
	}
	if err = i.StartHeartbeat(); err != nil {
		return session, err
	}
	id, err := r.Create(ctx, q)
	if err != nil {
		return session, fmt.Errorf("container create outcome requires reconciliation: %w", err)
	}
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return session, fmt.Errorf("runtime returned an invalid container identity")
	}
	owner.ID = id
	session.Owner = owner
	i.Record.Container = &owner
	if err = i.Save(ctx, "created"); err != nil {
		return session, err
	}
	if _, exists, err := containerapi.VerifyOwned(ctx, r, owner); err != nil || !exists {
		return session, fmt.Errorf("created container ownership is unconfirmed: %w", err)
	}
	return session, nil
}
func (s *ContainerSession) Start(ctx context.Context) (containerapi.Attachment, error) {
	a, err := s.Runtime.Attach(ctx, s.Owner.ID)
	if err != nil {
		return nil, err
	}
	// Callers start Copy before Start; Podman uses a combined attach/start client.
	return a, nil
}
func (s *ContainerSession) Run(ctx context.Context, streams containerapi.Streams) (int, error) {
	a, err := s.Start(ctx)
	if err != nil {
		return -1, err
	}
	defer a.Close()
	done := make(chan error, 1)
	go func() { done <- a.Copy(streams) }()
	if err = s.Runtime.Start(ctx, s.Owner.ID); err != nil {
		a.Close()
		<-done
		return -1, err
	}
	if err = s.Invocation.Save(ctx, "running"); err != nil {
		a.Close()
		<-done
		return -1, err
	}
	exited := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := s.Runtime.Wait(ctx, s.Owner.ID)
		exited <- struct {
			code int
			err  error
		}{code, err}
	}()
	var copyErr error
	for {
		select {
		case copyErr = <-done:
			done = nil
			if copyErr != nil {
				return -1, copyErr
			}
		case v := <-exited:
			if v.err != nil {
				_ = a.Close()
			}
			if done != nil {
				copyErr = <-done
			}
			return v.code, errors.Join(v.err, copyErr)
		case <-ctx.Done():
			_ = a.Close()
			if done != nil {
				<-done
			}
			return -1, ctx.Err()
		}
	}

}
func (s *ContainerSession) Close() error {
	s.once.Do(func() {
		s.Invocation.StopHeartbeat()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := containerapi.Remove(ctx, s.Owner)
		if s.stopGuardian != nil {
			err = errors.Join(err, s.stopGuardian())
		}
		state := "cleaned"
		if err != nil {
			state = "uncertain"
			s.Invocation.Record.Error = "container cleanup is unconfirmed"
			err = errors.Join(process.ErrCleanup, err)
		}
		s.closeErr = errors.Join(err, s.Invocation.Save(ctx, state), s.Runtime.Close())
	})
	return s.closeErr
}
