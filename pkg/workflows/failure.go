package workflows

import (
	"context"
	"errors"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/operational"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"strings"
)

func FailureClass(err error) string { return classifyFailure(err) }

func classifyFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errIterationBlocked) {
		return "iteration_blocked"
	}
	if errors.Is(err, ErrActionIntegrity) {
		return "action_integrity"
	}
	if errors.Is(err, process.ErrCleanup) {
		return "needs_attention"
	}
	if errors.Is(err, operational.ErrUnavailable) {
		return "prerequisite"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if cancellationOnly(err) {
		return "canceled"
	}
	if errors.Is(err, ErrProviderFailure) {
		return "provider_failure"
	}
	if errors.Is(err, ErrUsageExhausted) {
		return "usage_exhausted"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "401"), strings.Contains(text, "authentication"), strings.Contains(text, "credential"), strings.Contains(text, "oauth"):
		return "authentication"
	case strings.Contains(text, "database"), strings.Contains(text, "sqlite"), strings.Contains(text, "disk"), strings.Contains(text, "storage"):
		return "storage"
	case strings.Contains(text, "explicit --retry"), strings.Contains(text, "uncertain"), strings.Contains(text, "unexpected remote"), strings.Contains(text, "reconcil"):
		return "needs_attention"
	case strings.Contains(text, "conflict"):
		return "integration"
	case strings.Contains(text, "structured output"):
		return "schema"
	case strings.Contains(text, "repeat exhausted"), strings.Contains(text, "limit"), strings.Contains(text, "budget"):
		return "limit"
	case strings.Contains(text, "command failed"):
		return "command"
	}
	return "execution"
}

// cancellationOnly distinguishes an interrupted operation from independent
// failures joined with its cancellation. Wrappers retain their normal identity;
// every branch of an aggregate must be cancellation before diagnostics may clear.
func cancellationOnly(err error) bool { return onlyError(err, context.Canceled) }

// onlyError permits clearing a known signal only when every aggregate branch
// is that signal. Match its exact identity before unwrapping: pause denial
// itself wraps the broader external-input conflict family.
func onlyError(err, target error) bool {
	if err == nil {
		return false
	}
	if err == target {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		found := false
		for _, cause := range joined.Unwrap() {
			if cause == nil {
				continue
			}
			found = true
			if !onlyError(cause, target) {
				return false
			}
		}
		return found
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyError(wrapped.Unwrap(), target)
	}
	return errors.Is(err, target)
}

// executionFailureStatus arbitrates errors before their owning step or job is
// persisted. A canceled context cannot turn a genuine returned failure into a
// canceled result that aggregation would discard. Deadline behavior is retained.
func executionFailureStatus(ctx context.Context, err error) string {
	if errors.Is(err, process.ErrCleanup) || errors.Is(err, operational.ErrUnavailable) || errors.Is(err, ErrActionIntegrity) {
		return RunStatusNeedsAttention
	}
	if ctx.Err() != nil && (ctx.Err() != context.Canceled || cancellationOnly(err)) {
		return RunStatusCanceled
	}
	return RunStatusFailed
}

func hardExecutionFailure(err error) bool {
	switch classifyFailure(err) {
	case "action_integrity", "needs_attention", "prerequisite", "storage", "canceled":
		return true
	}
	return false
}

func iterationPersistence(err error, ordered bool) error {
	if err == nil || !ordered {
		return err
	}
	return fmt.Errorf("%w: %w", errIterationBlocked, err)
}
