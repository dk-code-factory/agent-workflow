// Package runtimeexec shares invocation ownership across agent and command steps.
package runtimeexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/containerapi"
	"github.com/dkropachev/agent-workflow/pkg/process"
)

type Policy struct {
	Mode     string `yaml:"mode"`
	Image    string `yaml:"image"`
	Runtime  string `yaml:"runtime"`
	Writable bool   `yaml:"writable"`
	Cleanup  string `yaml:"cleanup"`
	UID      *int   `yaml:"uid"`
	GID      *int   `yaml:"gid"`
}

func (p Policy) Validate() error {
	return p.ValidateFor(runtime.GOOS)
}
func (p Policy) ValidateFor(goos string) error {
	if p.Mode != "" && p.Mode != "local" && p.Mode != "container" {
		return fmt.Errorf("execution mode must be local or container")
	}
	if p.Cleanup != "" && p.Cleanup != "process-group" && p.Cleanup != "strict" {
		return fmt.Errorf("cleanup must be process-group or strict")
	}
	if p.Mode != "container" && p.Cleanup == "strict" && goos == "darwin" {
		return fmt.Errorf("strict descendant cleanup on macOS requires container execution")
	}
	if p.Mode != "container" && (p.Image != "" || p.Runtime != "" || p.Writable || p.UID != nil || p.GID != nil) {
		return fmt.Errorf("container parameters require execution.mode: container")
	}
	if p.Mode == "container" && p.Image == "" {
		return fmt.Errorf("container image is required")
	}
	if p.UID != nil && *p.UID <= 0 || p.GID != nil && *p.GID <= 0 {
		return fmt.Errorf("container UID/GID must be non-root positive IDs")
	}
	return nil
}

type Record struct {
	ID             string              `json:"id"`
	RunID          string              `json:"run_id"`
	Generation     int64               `json:"execution_generation"`
	LogicalID      string              `json:"logical_invocation_id"`
	Owner          string              `json:"-"`
	Kind           string              `json:"kind"`
	State          string              `json:"cleanup_state"`
	Token          string              `json:"token"`
	CompletionPath string              `json:"completion_path"`
	Container      *containerapi.Owner `json:"container,omitempty"`
	StartedAt      time.Time           `json:"started_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	Error          string              `json:"error,omitempty"`
}

type Journal interface {
	AdmitInvocation(context.Context, Record) error
	UpdateInvocation(context.Context, Record) error
}
type Binding struct {
	Journal                            Journal
	Dir, RunID, Owner, LogicalID, Kind string
	Generation                         int64
}
type bindingKey struct{}

func WithBinding(ctx context.Context, b Binding) context.Context {
	return context.WithValue(ctx, bindingKey{}, b)
}
func BindingFrom(ctx context.Context) (Binding, bool) {
	b, ok := ctx.Value(bindingKey{}).(Binding)
	return b, ok
}

func NewID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data[:])
}

type Invocation struct {
	Record        Record
	binding       Binding
	dir           string
	stopHeartbeat context.CancelFunc
	heartbeatDone chan struct{}
	mu            sync.Mutex
	finalized     bool
}

func Admit(ctx context.Context, kind string) (*Invocation, error) {
	b, ok := BindingFrom(ctx)
	if !ok {
		return nil, nil
	}
	id := NewID()
	dir := filepath.Join(b.Dir, "invocations", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	r := Record{ID: id, RunID: b.RunID, Generation: b.Generation, LogicalID: b.LogicalID, Owner: b.Owner, Kind: kind, State: "intent", Token: NewID(), CompletionPath: filepath.Join(dir, "complete.json"), StartedAt: time.Now().UTC()}
	r.UpdatedAt = r.StartedAt
	if err := b.Journal.AdmitInvocation(ctx, r); err != nil {
		return nil, err
	}
	return &Invocation{Record: r, binding: b, dir: dir}, nil
}

func (i *Invocation) Save(ctx context.Context, state string) error {
	i.Record.State = state
	i.Record.UpdatedAt = time.Now().UTC()
	return i.binding.Journal.UpdateInvocation(ctx, i.Record)
}
func (i *Invocation) Directory() string { return i.dir }
func (i *Invocation) StartHeartbeat() error {
	ctx, cancel := context.WithCancel(context.Background())
	i.stopHeartbeat = cancel
	i.heartbeatDone = make(chan struct{})
	sequence := uint64(1)
	write := func() error {
		data, _ := json.Marshal(map[string]any{"token": i.Record.Token, "sequence": sequence})
		sequence++
		tmp := filepath.Join(i.dir, "lease.partial")
		if err := os.WriteFile(tmp, data, 0600); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(i.dir, "lease"))
	}
	if err := write(); err != nil {
		cancel()
		close(i.heartbeatDone)
		return err
	}
	go func() {
		defer close(i.heartbeatDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if write() != nil {
					return
				}
			}
		}
	}()
	return nil
}
func (i *Invocation) StopHeartbeat() {
	if i.stopHeartbeat != nil {
		i.stopHeartbeat()
		<-i.heartbeatDone
	}
}

func Supervise(ctx context.Context, cmd *exec.Cmd) (func(), error) {
	i, err := Admit(ctx, "native")
	if err != nil {
		return nil, err
	}
	if i == nil {
		return process.Supervise(cmd)
	}
	return i.Supervise(ctx, cmd, nil)
}

func (i *Invocation) Supervise(ctx context.Context, cmd *exec.Cmd, cleanup []string) (func(), error) {
	stop, err := process.SuperviseWithOptions(cmd, process.SupervisionOptions{Cleanup: cleanup, CompletionPath: i.Record.CompletionPath, Token: i.Record.Token})
	if err != nil {
		_ = i.Save(context.WithoutCancel(ctx), "cleaned")
		return nil, err
	}
	if err = i.Save(ctx, "launching"); err != nil {
		stop()
		return nil, err
	}
	baseCancel := cmd.Cancel
	cmd.Cancel = func() error { i.StopHeartbeat(); return baseCancel() }
	return func() {
		i.StopHeartbeat()
		stop()
		if cmd.ProcessState == nil && cmd.Process != nil {
			return
		} // caller still owns Wait
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.finalized {
			return
		}
		i.finalized = true
		i.StopHeartbeat()
		finishCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var cleanupErr error
		if i.Record.Container != nil {
			cleanupErr = containerapi.Remove(finishCtx, *i.Record.Container)
		} else if cmd.Process != nil {
			cleanupErr = process.ReadCompletion(i.Record.CompletionPath, i.Record.Token)
		}
		state := "cleaned"
		if cleanupErr != nil {
			state = "uncertain"
			i.Record.Error = "invocation cleanup is unconfirmed"
		}
		if err := i.Save(finishCtx, state); err != nil {
			i.Record.State = "uncertain"
		}
	}, nil
}

func Reconcile(ctx context.Context, record Record) (Record, error) {
	if record.State == "cleaned" {
		return record, nil
	}
	record.UpdatedAt = time.Now().UTC()
	if record.State == "intent" && record.Container == nil {
		record.State = "cleaned"
		record.Error = ""
		return record, nil
	}
	var err error
	if record.Container != nil {
		err = containerapi.Remove(ctx, *record.Container)
	} else {
		err = process.ReadCompletion(record.CompletionPath, record.Token)
	}
	if err != nil {
		record.State = "uncertain"
		record.Error = "invocation cleanup is unconfirmed"
		return record, errors.Join(process.ErrCleanup, err)
	}
	record.State = "cleaned"
	record.Error = ""
	record.UpdatedAt = time.Now().UTC()
	return record, nil
}
