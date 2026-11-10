package workflows

import (
	"context"
	"errors"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/operational"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"strings"

	"modernc.org/sqlite"
)

var errStepPersistence = errors.New("step journal storage persistence failed")

func FailureClass(err error) string { return classifyFailure(err) }

func classifyFailure(err error) string {
	if err == nil {
		return ""
	}
	if retained, ok := err.(*retainedLifecycleFailure); ok {
		return retained.class
	}
	if errors.Is(err, errStepPersistence) {
		return "storage"
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
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return "storage"
	}
	// An aggregate's ordinary error must not hide an unsafe constituent failure.
	var causes []error
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes = joined.Unwrap()
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		causes = []error{wrapped.Unwrap()}
	}
	classes := make(map[string]bool, len(causes))
	for _, cause := range causes {
		class := classifyFailure(cause)
		if class != "canceled" && hardFailureClass(class) {
			return class
		}
		classes[class] = true
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
	case classes["authentication"], authenticationDiagnostic(text):
		return "authentication"
	case strings.Contains(text, "database"), strings.Contains(text, "sqlite"), strings.Contains(text, "disk"), strings.Contains(text, "storage"):
		return "storage"
	case strings.Contains(text, "explicit --retry"), strings.Contains(text, "uncertain"), strings.Contains(text, "unexpected remote"), strings.Contains(text, "reconcil"):
		return "needs_attention"
	case classes["integration"], strings.Contains(text, "conflict"):
		return "integration"
	case classes["schema"], strings.Contains(text, "structured output"):
		return "schema"
	case classes["limit"], strings.Contains(text, "repeat exhausted"), strings.Contains(text, "limit"), strings.Contains(text, "budget"):
		return "limit"
	case classes["command"], strings.Contains(text, "command failed"):
		return "command"
	}
	return "execution"
}

func authenticationDiagnostic(text string) bool {
	return strings.Contains(text, "401") || strings.Contains(text, "authentication") || strings.Contains(text, "credential") || strings.Contains(text, "oauth")
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
	return unsafeFailureClass(err) != ""
}

// Admission considers cancellation in every branch without replacing the
// diagnostic for an independent failure joined with that cancellation.
func unsafeFailureClass(err error) string {
	if retained, ok := err.(*retainedLifecycleFailure); ok && retained.unsafeClass != "" {
		return retained.unsafeClass
	}
	if class := classifyFailure(err); hardFailureClass(class) {
		return class
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return ""
}

func hardFailureClass(class string) bool {
	switch class {
	case "action_integrity", "needs_attention", "prerequisite", "storage", "canceled", "iteration_blocked":
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
