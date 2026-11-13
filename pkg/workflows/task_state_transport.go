package workflows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const taskStateMount = "/engine-task-state"

type taskStateServiceKey struct{}

// This capability exists only for the lifetime of one helper hook. It is sent
// on stdin, never added to the admitted action identity or retained checkpoint.
type taskStateDescriptor struct {
	Socket string `json:"socket"`
	Token  string `json:"token"`
	Group  string `json:"group"`
}

type taskStateCapability struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	readOnly bool
	revoked  bool
}

type taskStateService struct {
	dir       string
	group     string
	store     *SQLiteStore
	server    *http.Server
	done      chan struct{}
	mu        sync.Mutex
	closed    bool
	caps      map[string]*taskStateCapability
	closeOnce sync.Once
	closeErr  error
	handlers  sync.WaitGroup
}

func (e *Executor) newTaskStateService(ctx context.Context) (*taskStateService, error) {
	f, ok := ctx.Value(fenceKey{}).(executionFence)
	if !ok {
		return nil, fmt.Errorf("task state requires execution ownership")
	}
	store := invocationStore(e.Store)
	if store == nil {
		return nil, nil
	}
	_, snap, err := store.Load(ctx, f.id)
	if err != nil {
		return nil, err
	}
	group := nativeString(snap.Metadata, "task_group")
	if group == "" {
		return nil, nil
	}
	// A short disposable path avoids Unix socket path limits when the workspace
	// or engine state is deeply nested. Only this endpoint's directory is removed.
	dir, err := os.MkdirTemp("", "aw-task-state-")
	if err != nil {
		return nil, err
	}
	s := &taskStateService{dir: dir, group: group, store: store, done: make(chan struct{}), caps: map[string]*taskStateCapability{}}
	defer func() {
		if err != nil {
			_ = os.Chmod(dir, 0700)
			_ = os.RemoveAll(dir)
		}
	}()
	listener, err := net.Listen("unix", filepath.Join(dir, "rpc.sock"))
	if err != nil {
		return nil, err
	}
	// The admitted image may use another UID/GID. Directory traversal and socket
	// connection are permitted; the directory remains unwritable and every RPC
	// requires an unguessable, active, owner-bound capability.
	if err = os.Chmod(dir, 0711); err == nil {
		err = os.Chmod(filepath.Join(dir, "rpc.sock"), 0666)
	}
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	s.server = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 4096}
	go func() {
		defer close(s.done)
		_ = s.server.Serve(listener)
	}()
	return s, nil
}

func (s *taskStateService) capability(ctx context.Context, phase string) (*taskStateDescriptor, func(), error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return nil, nil, err
	}
	token := hex.EncodeToString(bytes[:])
	owned, cancel := context.WithCancel(ctx)
	c := &taskStateCapability{ctx: owned, cancel: cancel, readOnly: phase != "execute"}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil, nil, fmt.Errorf("task-state endpoint is closed")
	}
	s.caps[token] = c
	s.mu.Unlock()
	revoke := func() {
		s.mu.Lock()
		delete(s.caps, token)
		s.mu.Unlock()
		cancel()
		// Joining the capability lock guarantees no RPC can outlive its hook.
		c.mu.Lock()
		c.revoked = true
		c.mu.Unlock()
	}
	return &taskStateDescriptor{Socket: taskStateMount + "/rpc.sock", Token: token, Group: s.group}, revoke, nil
}

func (s *taskStateService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "task-state endpoint is closed", http.StatusServiceUnavailable)
		return
	}
	s.handlers.Add(1)
	s.mu.Unlock()
	defer s.handlers.Done()
	if r.Method != http.MethodPost || r.URL.Path != "/v1" || r.URL.RawQuery != "" {
		http.Error(w, "task-state RPC requires POST /v1", http.StatusNotFound)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	c := s.caps[token]
	s.mu.Unlock()
	if c == nil {
		http.Error(w, "inactive task-state capability", http.StatusUnauthorized)
		return
	}
	var request TaskStateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid task-state request", http.StatusBadRequest)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		http.Error(w, "task-state request requires one JSON document", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revoked || c.ctx.Err() != nil {
		http.Error(w, "inactive task-state capability", http.StatusUnauthorized)
		return
	}
	if c.readOnly && !TaskStateReadOnly(request.Operation) {
		http.Error(w, "task-state writes require the execute phase", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	defer cancel()
	result, err := s.store.TaskState(ctx, request)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > 1<<20 {
		http.Error(w, "task-state response exceeds protocol limit", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (s *taskStateService) close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.closeEndpoint() })
	return s.closeErr
}

func (s *taskStateService) closeEndpoint() error {
	s.mu.Lock()
	s.closed = true
	for _, c := range s.caps {
		c.cancel()
	}
	s.mu.Unlock()
	err := s.server.Close()
	<-s.done
	s.handlers.Wait()
	// HTTP Close stops sockets; join each canceled store operation as well.
	s.mu.Lock()
	for token, c := range s.caps {
		c.mu.Lock()
		c.revoked = true
		c.mu.Unlock()
		delete(s.caps, token)
	}
	s.mu.Unlock()
	return errors.Join(err, os.Chmod(s.dir, 0700), os.RemoveAll(s.dir))
}

func (s *jobContainerScope) ensureTaskState(ctx context.Context) error {
	if s.taskState != nil {
		return nil
	}
	var err error
	s.taskState, err = s.executor.newTaskStateService(ctx)
	return err
}
