// Package controller owns durable workflow admission independently of clients.
package controller

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

type StartRequest struct {
	RequestID  string            `json:"request_id,omitempty"`
	Ref        string            `json:"ref"`
	Inputs     map[string]any    `json:"inputs,omitempty"`
	SecretRefs map[string]string `json:"secret_refs,omitempty"`
}
type Server struct {
	MaxQueued  int
	Workers    *workflows.WorkerManager
	Executor   *workflows.Executor
	Store      *workflows.SQLiteStore
	Dir        string
	MaxRuns    int
	RunTimeout time.Duration
	mu         sync.Mutex
	active     map[string]context.CancelFunc
	canceled   map[string]bool
	ctx        context.Context
	wake       chan struct{}
	wg         sync.WaitGroup
	stop       context.CancelFunc
}

func ResolveSecrets(refs map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for key, env := range refs {
		if env == "" {
			return nil, fmt.Errorf("secret %s has empty environment reference", key)
		}
		v, ok := os.LookupEnv(env)
		if !ok {
			return nil, fmt.Errorf("credential reference %q is unavailable in controller environment", env)
		}
		out[key] = v
	}
	return out, nil
}
func (s *Server) Serve(ctx context.Context) error {
	ctx, stopOwner := context.WithCancel(ctx)
	defer stopOwner()
	lock, err := Own(s.Dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = s.Store.Recover(ctx); err != nil {
		return err
	}
	s.ctx = ctx
	s.stop = stopOwner
	s.active = map[string]context.CancelFunc{}
	s.canceled = map[string]bool{}
	s.wake = make(chan struct{}, 1)
	if s.MaxQueued <= 0 {
		s.MaxQueued = 128
	}
	if s.RunTimeout <= 0 {
		s.RunTimeout = 30 * time.Minute
	}
	if s.MaxRuns <= 0 {
		s.MaxRuns = 4
	}
	if s.Executor.Slots == nil {
		limit := s.Executor.MaxParallel
		if limit <= 0 {
			limit = 4
		}
		s.Executor.Slots = make(chan struct{}, limit)
	}
	var workerDone chan struct{}
	if s.Workers != nil {
		s.Workers.Slots = s.Executor.Slots
		if err = s.Workers.Init(ctx); err != nil {
			return err
		}
		workerDone = make(chan struct{})
		go func() { defer close(workerDone); s.Workers.Serve(ctx) }()
		defer func() { stopOwner(); <-workerDone }()
	}
	socket, cleanSocketDir, err := newSocketAddress(s.Dir)
	if err != nil {
		return err
	}
	defer cleanSocketDir()
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		return err
	}
	if err = writeSocketAddress(s.Dir, socket); err != nil {
		_ = listener.Close()
		return err
	}
	defer os.Remove(filepath.Join(s.Dir, "controller.address"))
	server := &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); s.loop() }()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-stop:
		}
	}()
	err = server.Serve(listener)
	close(stop)
	stopOwner()
	<-loopDone
	s.mu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
	// The owner lock is held until every supervised execution has stopped.
	s.wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (s *Server) loop() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		runs, err := s.Store.List(s.ctx)
		if err != nil {
			continue
		}
		// Oldest admitted runs first.
		for i := len(runs) - 1; i >= 0; i-- {
			r := runs[i]
			if r.Status != workflows.RunStatusQueued {
				continue
			}
			s.mu.Lock()
			_, busy := s.active[r.ID]
			if busy || len(s.active) >= s.MaxRuns {
				s.mu.Unlock()
				continue
			}
			current, _, loadErr := s.Store.Load(s.ctx, r.ID)
			if loadErr != nil || current.Status != workflows.RunStatusQueued {
				s.mu.Unlock()
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, s.RunTimeout)
			s.active[r.ID] = cancel
			s.wg.Add(1)
			s.mu.Unlock()
			go func(id string, ctx context.Context) {
				if s.Workers != nil {
					defer s.Workers.CancelRun(context.Background(), id)
				}
				defer s.wg.Done()
				defer func() {
					s.mu.Lock()
					delete(s.active, id)
					delete(s.canceled, id)
					s.mu.Unlock()
					select {
					case s.wake <- struct{}{}:
					default:
					}
				}()
				_, snap, err := s.Store.Load(ctx, id)
				var secrets map[string]string
				if err == nil {
					secrets, err = ResolveSecrets(snap.SecretRefs)
				}
				if err == nil {
					_, err = s.Executor.Resume(ctx, id, secrets, false)
				}
				if err != nil {
					r, _, loadErr := s.Store.Load(context.Background(), id)
					if loadErr == nil && (r.Status == workflows.RunStatusQueued || r.Status == workflows.RunStatusRunning) {
						r.Status = workflows.RunStatusNeedsAttention
						r.Error = workflows.Redact(err.Error(), secrets)
						_ = s.Store.Save(context.Background(), r)
					}
				}
			}(r.ID, ctx)
		}
	}
}
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	knownSecrets := map[string]string{}
	if source, ok := s.Executor.Agents.(interface{ SecretValues() map[string]string }); ok {
		for key, value := range source.SecretValues() {
			knownSecrets[key] = value
		}
	}
	fail := func(code int, err error) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": workflows.Redact(err.Error(), knownSecrets)})
	}
	encode := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if r.URL.Path == "/health" && r.Method == "GET" {
		h := sha256.Sum256(s.Executor.Roles)
		encode(map[string]any{"version": 1, "roles_hash": hex.EncodeToString(h[:]), "parallel": cap(s.Executor.Slots)})
		return
	}
	if r.URL.Path == "/workflows" && r.Method == "GET" {
		v, err := s.Executor.Discover()
		if err != nil {
			fail(500, err)
			return
		}
		encode(v)
		return
	}
	if r.URL.Path == "/shutdown" && r.Method == "POST" {
		encode(map[string]bool{"stopping": true})
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		s.stop()
		return
	}
	if r.URL.Path == "/runs" {
		switch r.Method {
		case "GET":
			v, err := s.Store.List(r.Context())
			if err != nil {
				fail(500, err)
				return
			}
			encode(v)
			return
		case "POST":
			var req StartRequest
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
			d.DisallowUnknownFields()
			if err := d.Decode(&req); err != nil {
				fail(400, err)
				return
			}
			secrets, err := ResolveSecrets(req.SecretRefs)
			for key, value := range secrets {
				knownSecrets[key] = value
			}
			if err != nil {
				fail(400, err)
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if req.RequestID != "" {
				if len(req.RequestID) > 128 {
					fail(400, fmt.Errorf("request_id exceeds 128 bytes"))
					return
				}
				existing, snap, lookupErr := s.Store.AdmissionRequest(r.Context(), req.RequestID)
				if lookupErr == nil {
					ref, _ := workflows.CanonicalLocalRef(req.Ref)
					a, _ := json.Marshal(req.Inputs)
					b, _ := json.Marshal(snap.Inputs)
					c, _ := json.Marshal(req.SecretRefs)
					d, _ := json.Marshal(snap.SecretRefs)
					if existing.WorkflowRef != ref || string(a) != string(b) || string(c) != string(d) {
						fail(409, fmt.Errorf("request_id reused with different content"))
						return
					}
					encode(existing)
					return
				}
				if !errors.Is(lookupErr, sql.ErrNoRows) {
					fail(500, lookupErr)
					return
				}
			}
			runs, err := s.Store.List(r.Context())
			if err != nil {
				fail(500, err)
				return
			}
			queued := 0
			for _, run := range runs {
				if run.Status == workflows.RunStatusQueued {
					queued++
				}
			}
			if queued >= s.MaxQueued {
				fail(429, fmt.Errorf("queued run limit reached"))
				return
			}
			run, err := s.Executor.Prepare(r.Context(), workflows.RunRequest{RequestID: req.RequestID, Ref: req.Ref, Inputs: req.Inputs, SecretRefs: req.SecretRefs, Secrets: secrets})
			if err != nil {
				fail(400, err)
				return
			}
			select {
			case s.wake <- struct{}{}:
			default:
			}
			w.WriteHeader(202)
			encode(run)
			return
		}
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "runs" || len(parts[1]) != 32 || strings.Trim(parts[1], "0123456789abcdef") != "" {
		fail(404, fmt.Errorf("unknown endpoint"))
		return
	}
	id := parts[1]
	run, snap, err := s.Store.Load(r.Context(), id)
	if err != nil {
		if r.Method == "POST" && len(parts) == 3 && parts[2] == "cancel" {
			s.mu.Lock()
			if cancel := s.active[id]; cancel != nil {
				cancel()
			}
			s.mu.Unlock()
			if s.Workers != nil {
				_ = s.Workers.CancelRun(context.WithoutCancel(r.Context()), id)
			}
		}
		if r.Method == "POST" && len(parts) == 5 && parts[2] == "workers" && (parts[4] == "interrupt" || parts[4] == "close") && s.Workers != nil {
			_ = s.Workers.Interrupt(context.WithoutCancel(r.Context()), id, parts[3], parts[4] == "close")
		}
		code := 500
		if errors.Is(err, sql.ErrNoRows) {
			code = 404
		}
		fail(code, err)
		return
	}
	for _, ref := range snap.SecretRefs {
		if value, ok := os.LookupEnv(ref); ok {
			knownSecrets[ref] = value
		}
	}
	if s.Executor.RoleRunner != nil {
		if runner, err := s.Executor.RoleRunner(snap.Roles); err == nil {
			if source, ok := runner.(interface{ SecretValues() map[string]string }); ok {
				for key, value := range source.SecretValues() {
					knownSecrets[key] = value
				}
			}
		}
	}
	if len(parts) == 2 && r.Method == "GET" {
		encode(run)
		return
	}
	if len(parts) >= 3 && parts[2] == "workers" {
		if s.Workers == nil {
			fail(501, fmt.Errorf("live workers are not configured"))
			return
		}
		s.workerAPI(w, r, id, parts[3:], knownSecrets)
		return
	}
	if len(parts) == 4 && parts[2] == "artifacts" && r.Method == "GET" {
		data, err := s.Store.Artifact(r.Context(), id, parts[3])
		if err != nil {
			fail(404, err)
			return
		}
		encode(map[string]string{"id": parts[3], "text": string(data)})
		return
	}
	if len(parts) == 3 && (parts[2] == "cancel" || parts[2] == "resume") {
		if origin, ok := snap.Metadata["origin_run_id"].(string); ok && origin != "" && origin != id {
			fail(409, fmt.Errorf("control the originating run %s", origin))
			return
		}
	}
	if len(parts) == 3 {
		switch parts[2] {
		case "artifacts":
			if r.Method == "GET" {
				v, err := s.Store.Artifacts(r.Context(), id)
				if err != nil {
					fail(500, err)
					return
				}
				encode(v)
				return
			}
		case "events":
			if r.Method == "GET" {
				after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
				v, err := s.Store.Events(r.Context(), id, after)
				if err != nil {
					fail(500, err)
					return
				}
				encode(v)
				return
			}
		case "attempts":
			if r.Method == "GET" {
				v, err := s.Store.Attempts(r.Context(), id)
				if err != nil {
					fail(500, err)
					return
				}
				encode(v)
				return
			}
		case "cancel":
			if r.Method == "POST" {
				s.mu.Lock()
				defer s.mu.Unlock()
				cancelErr := s.Store.RequestCancel(r.Context(), id)
				cancel, active := s.active[id]
				if active {
					cancel()
					s.canceled[id] = true
				}
				if cancelErr != nil {
					if s.Workers != nil {
						_ = s.Workers.CancelRun(context.WithoutCancel(r.Context()), id)
					}
					fail(500, cancelErr)
					return
				}
				run, _, err = s.Store.Load(r.Context(), id)
				if err != nil {
					fail(500, err)
					return
				}
				if !active && (run.Status == workflows.RunStatusQueued || run.Status == workflows.RunStatusNeedsAttention) {
					run.Status = workflows.RunStatusCanceled
					run.Error = "canceled before execution"
					run.FinishedAt = time.Now().UTC()
					if err = s.Store.Save(r.Context(), run); err != nil {
						fail(500, err)
						return
					}
				}
				if s.Workers != nil {
					if err = s.Workers.CancelRun(r.Context(), id); err != nil {
						fail(500, err)
						return
					}
				}
				encode(map[string]any{"id": id, "cancellation_requested": true})
				return
			}
		case "resume":
			if r.Method == "POST" {
				s.mu.Lock()
				defer s.mu.Unlock()
				if _, ok := s.active[id]; ok || run.Status == workflows.RunStatusRunning || run.Status == workflows.RunStatusQueued {
					fail(409, fmt.Errorf("run is already active"))
					return
				}
				secrets, err := ResolveSecrets(snap.SecretRefs)
				if err != nil {
					fail(400, err)
					return
				}
				if run.Status == workflows.RunStatusSucceeded {
					encode(run)
					return
				}
				attempts, err := s.Store.Attempts(r.Context(), id)
				if err != nil {
					fail(500, err)
					return
				}
				retry := r.URL.Query().Get("retry_uncertain") == "true"
				for _, a := range attempts {
					if a.Status != workflows.RunStatusSucceeded && a.Status != workflows.RunStatusSkipped && a.Phase == "" && !retry {
						fail(409, fmt.Errorf("explicit retry_uncertain acknowledgement required for attempt %s", a.ID))
						return
					}
				}
				if run.Status == workflows.RunStatusSucceeded {
					encode(run)
					return
				}
				if len(s.active) >= s.MaxRuns {
					fail(429, fmt.Errorf("active run limit reached"))
					return
				}
				ctx, cancel := context.WithTimeout(s.ctx, s.RunTimeout)
				s.active[id] = cancel
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					defer func() { s.mu.Lock(); delete(s.active, id); s.mu.Unlock() }()
					_, _ = s.Executor.Resume(ctx, id, secrets, retry)
				}()
				w.WriteHeader(202)
				encode(map[string]string{"id": id})
				return
			}
		}
	}
	fail(405, fmt.Errorf("unsupported operation"))
}
