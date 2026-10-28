package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/operational"
)

type Readiness struct {
	Ready       bool                              `json:"ready"`
	Checks      map[string]string                 `json:"checks"`
	Filesystems map[string]operational.Filesystem `json:"filesystems"`
}

func (s *Server) readiness(ctx context.Context) Readiness {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	report := Readiness{Ready: true, Checks: map[string]string{}, Filesystems: map[string]operational.Filesystem{}}
	check := func(name string, err error) {
		if err != nil {
			report.Ready = false
			report.Checks[name] = "unavailable"
		} else {
			report.Checks[name] = "ok"
		}
	}
	storageErr := s.Store.ProbeStorage(ctx)
	check("storage", storageErr)
	if storageErr != nil {
		s.storageErrors.Add(1)
	}
	heartbeat := s.schedulerHeartbeat.Load()
	if heartbeat == 0 || time.Since(time.Unix(0, heartbeat)) > 5*time.Second {
		report.Ready = false
		report.Checks["scheduler"] = "stale"
	} else {
		report.Checks["scheduler"] = "ok"
	}
	check("cleanup", s.Store.CheckCleanupPrerequisites(ctx))
	check("state_permissions", operational.CheckPrivateState(s.Dir))
	for name, path := range map[string]string{"state": s.Dir, "temporary": s.Operational.TemporaryDirectory()} {
		state, err := s.Operational.Check(path)
		report.Filesystems[name] = state
		check(name+"_capacity", err)
	}
	file, err := os.CreateTemp(s.Operational.TemporaryDirectory(), "agent-workflow-ready-*")
	if err == nil {
		err = file.Close()
		_ = os.Remove(file.Name())
	}
	check("temporary_write", err)
	return report
}
func (s *Server) requireReady(ctx context.Context) error {
	if report := s.readiness(ctx); !report.Ready {
		return fmt.Errorf("%w; inspect /ready", operational.ErrUnavailable)
	}
	return nil
}
func (s *Server) diagnosticAPI(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "GET" {
		return false
	}
	if r.URL.Path == "/ready" {
		report := s.readiness(r.Context())
		if !report.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(report)
		return true
	}
	if r.URL.Path != "/metrics" {
		return false
	}
	metrics, err := s.Store.Metrics(r.Context())
	if err != nil {
		s.storageErrors.Add(1)
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "storage metrics unavailable"})
		return true
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	occupied, suspended := len(s.Executor.Slots), 0
	if s.Executor.Capacity != nil {
		occupied = len(s.Executor.Capacity.Slots)
		suspended = s.Executor.Capacity.Suspended()
	}
	fmt.Fprintf(w, "agent_workflow_queue_depth %d\nagent_workflow_queue_age_seconds %g\nagent_workflow_occupied_slots %d\nagent_workflow_suspended_slots %d\nagent_workflow_failures_total %d\nagent_workflow_cleanup_backlog %d\nagent_workflow_storage_errors_total %d\n", metrics.QueueDepth, metrics.QueueAgeSeconds, occupied, suspended, metrics.Failures, metrics.CleanupBacklog, s.storageErrors.Load())
	for kind, states := range map[string]map[string]int{"runs": metrics.Runs, "workers": metrics.Workers} {
		keys := make([]string, 0, len(states))
		for key := range states {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, state := range keys {
			switch state {
			case "queued", "running", "succeeded", "failed", "canceled", "needs_attention", "waiting", "interrupted", "skipped", "idle", "closed":
				fmt.Fprintf(w, "agent_workflow_%s{state=%q} %d\n", kind, state, states[state])
			}
		}
	}
	return true
}
