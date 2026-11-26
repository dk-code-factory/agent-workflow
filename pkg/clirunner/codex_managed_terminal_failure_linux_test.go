//go:build linux

package clirunner

import (
	"errors"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestManagedCodexSourceFailureCannotAuthorizeProviderContinuation(t *testing.T) {
	for _, status := range []string{"failed", "interrupted", "unknown"} {
		err := managedTerminalFailure(status, map[string]any{"message": "original raw history does not match sealed witness"})
		if !errors.Is(err, workflows.ErrActionIntegrity) || errors.Is(err, workflows.ErrProviderFailure) || workflows.FailureClass(err) != "action_integrity" || !strings.Contains(err.Error(), "sealed witness") {
			t.Fatal("native uncertainty became an observed semantic provider failure", err)
		}
	}
	if err := managedTerminalFailure("failed", strings.Repeat("x", 4097)); len(err.Error()) > 4200 {
		t.Fatal("unbounded native diagnostic", len(err.Error()))
	}
}
