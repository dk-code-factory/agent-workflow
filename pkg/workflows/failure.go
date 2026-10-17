package workflows

import (
	"context"
	"errors"
	"github.com/dkropachev/agent-workflow/pkg/process"
	"strings"
)

func classifyFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, process.ErrCleanup) {
		return "needs_attention"
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
	case strings.Contains(text, "repeat exhausted"), strings.Contains(text, "limit"), strings.Contains(text, "budget"):
		return "limit"
	case strings.Contains(text, "command failed"):
		return "command"
	}
	return "execution"
}
