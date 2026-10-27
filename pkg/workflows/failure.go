package workflows

import (
	"context"
	"errors"
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
	if errors.Is(err, context.Canceled) {
		return "canceled"
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
	case errors.Is(err, ErrUsageExhausted):
		return "usage_exhausted"
	case errors.Is(err, ErrProviderFailure):
		return "provider_failure"
	case strings.Contains(text, "repeat exhausted"), strings.Contains(text, "limit"), strings.Contains(text, "budget"):
		return "limit"
	case strings.Contains(text, "command failed"):
		return "command"
	}
	return "execution"
}

func hardExecutionFailure(err error) bool {
	switch classifyFailure(err) {
	case "action_integrity", "needs_attention", "prerequisite", "storage", "canceled":
		return true
	}
	return false
}
