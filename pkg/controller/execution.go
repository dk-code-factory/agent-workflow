package controller

import (
	"context"
	"errors"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"log/slog"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// executeOwner is the only controller execution path for both first admission
// and resume. Ownership outlives deadline release and supervised cleanup.
func (s *Server) executeOwner(ctx context.Context, cancel context.CancelFunc, id string, generation int64) {
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
	defer cancel()
	_, snap, err := s.Store.Load(ctx, id)
	var secrets map[string]string
	if err == nil {
		secrets, err = ResolveSecrets(snap.SecretRefs)
	}
	var retry bool
	if err == nil {
		retry, err = s.Store.ExecutionRetry(ctx, id, generation)
	}
	if err == nil {
		_, err = s.Executor.Resume(ctx, id, secrets, retry)
	}
	// Also stop workers on failures before Executor reaches finalization.
	if s.Workers != nil {
		if cleanupErr := s.Workers.StopRun(context.Background(), id); cleanupErr != nil {
			err = errors.Join(err, process.ErrCleanup, cleanupErr)
		}
	}
	if err != nil {
		logger := s.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("execution failed", "run_id", id, "generation", generation, "failure_class", workflows.FailureClass(err))
		current, _, loadErr := s.Store.Load(context.Background(), id)
		if loadErr == nil && current.Generation == generation {
			if current.Status == workflows.RunStatusQueued || current.Status == workflows.RunStatusRunning || errors.Is(err, process.ErrCleanup) {
				current.Status = workflows.RunStatusNeedsAttention
				if errors.Is(err, context.Canceled) && !errors.Is(err, process.ErrCleanup) {
					if requested, cancelErr := s.Store.CancellationRequested(context.Background(), id); cancelErr == nil && requested {
						current.Status = workflows.RunStatusCanceled
					}
				}
				current.Error = workflows.Redact(err.Error(), secrets)
				current.FailureClass = workflows.FailureClass(err)
				current.FinishedAt = time.Now().UTC()
				if saveErr := s.Store.Save(context.Background(), current); saveErr != nil {
					s.storageErrors.Add(1)
					slog.Error("failed to persist execution outcome", "run_id", id, "generation", generation, "failure_class", "storage")
				}
			}
		}
	}
}
