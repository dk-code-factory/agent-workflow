package runtimeexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

// Exec journals an identity before starting a command in the resident container.
// A per-command heartbeat is mounted below the parent's lifetime directory, so
// cancellation can stop descendants without ending the parent's lifetime.
func (s *ContainerSession) Exec(ctx context.Context, q containerapi.ExecRequest, streams containerapi.Streams) (code int, returned error) {
	if _, exists, err := containerapi.VerifyOwned(ctx, s.Runtime, s.Owner); err != nil || !exists {
		return -1, fmt.Errorf("%w: parent container ownership is unconfirmed: %v", process.ErrCleanup, err)
	}
	i, err := Admit(ctx, "container-exec")
	if err != nil {
		return -1, err
	}
	if i == nil {
		return -1, fmt.Errorf("exec requires durable invocation ownership")
	}
	owner := s.Owner
	i.Record.Container = &owner
	i.dir = filepath.Join(s.Invocation.Directory(), "commands", i.Record.ID)
	if err = os.MkdirAll(i.dir, 0700); err != nil {
		return -1, err
	}
	if s.Invocation.lifetimeUID != nil {
		if err = os.Chown(filepath.Dir(i.dir), *s.Invocation.lifetimeUID, *s.Invocation.lifetimeGID); err != nil {
			return -1, err
		}
		if err = i.setLifetimeOwner(*s.Invocation.lifetimeUID, *s.Invocation.lifetimeGID); err != nil {
			return -1, err
		}
	}
	if err = i.Save(ctx, "intent"); err != nil {
		return -1, err
	}
	defer func() {
		i.StopHeartbeat()
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		state := "cleaned"
		if i.Record.ExecID != "" {
			for {
				status, e := s.Runtime.ExecInspect(finish, i.Record.ExecID)
				if e == nil && !status.Running && status.ID == i.Record.ExecID && status.ContainerID == owner.ID {
					if status.ExitCode == 125 || status.ExitCode < 0 {
						state = "uncertain"
						i.Record.Error = "exec guardian cleanup is unconfirmed"
						returned = errors.Join(returned, fmt.Errorf("%w: %s", process.ErrCleanup, i.Record.Error))
					}
					break
				}
				if finish.Err() != nil {
					state = "uncertain"
					returned = errors.Join(returned, process.ErrCleanup, fmt.Errorf("exec cleanup could not be confirmed"))
					break
				}
				select {
				case <-finish.Done():
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		returned = errors.Join(returned, i.Save(context.WithoutCancel(ctx), state))
	}()
	q.Command = append([]string{MountedGuardian, "__container_exec_guardian_v1", "/engine-lifetime/commands/" + i.Record.ID + "/lease", i.Record.Token, "--"}, q.Command...)
	if err = i.StartHeartbeat(); err != nil {
		return -1, err
	}
	id, err := s.Runtime.ExecCreate(ctx, owner.ID, q)
	if err != nil {
		return -1, err
	}
	if id == "" {
		return -1, fmt.Errorf("runtime returned empty exec identity")
	}
	i.Record.ExecID = id
	if err = i.Save(ctx, "created"); err != nil {
		return -1, err
	}
	a, err := s.Runtime.ExecAttach(ctx, id)
	if err != nil {
		return -1, err
	}
	defer a.Close()
	done := make(chan error, 1)
	go func() { done <- a.Copy(streams) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		i.StopHeartbeat()
		a.Close()
		<-done
		return -1, ctx.Err()
	}
	if err != nil {
		return -1, err
	}
	status, err := s.Runtime.ExecInspect(ctx, id)
	if err != nil {
		return -1, err
	}
	if status.Running || status.ID != id || status.ContainerID != owner.ID {
		return -1, fmt.Errorf("%w: exec completion is unconfirmed", process.ErrCleanup)
	}
	return status.ExitCode, nil
}
