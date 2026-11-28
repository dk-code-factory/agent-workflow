package clirunner

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

const claudeNativeReadOnlyProfile = "claude-sdk@0.3.322/native-task-read-only-v1"

func admitNativePolicy(role Agent, definition *workflows.AgentDefinition) (*workflows.NativeTaskPolicy, error) {
	if definition.ToolPolicy == "" {
		if definition.NativePolicy != nil {
			return nil, fmt.Errorf("native task policy lacks its declaration")
		}
		return nil, nil
	}
	if definition.ToolPolicy != "read-only" || role.Provider != "claude-sdk" || role.Execution.Mode == "container" || ClaudeSDKVersion != "0.3.322" {
		return nil, fmt.Errorf("native task read-only policy requires the pinned local Claude SDK runtime")
	}
	if err := verifySDKBridge(role); err != nil {
		return nil, err
	}
	tools := []string{}
	seen := map[string]bool{}
	for _, name := range role.Tools {
		if (name == "Read" || name == "Glob" || name == "Grep") && !seen[name] {
			tools = append(tools, name)
			seen[name] = true
		}
	}
	sort.Strings(tools)
	policy := &workflows.NativeTaskPolicy{Mode: "read-only", Provider: "claude-sdk", Profile: claudeNativeReadOnlyProfile, NativeTools: tools}
	if definition.NativePolicy != nil && !reflect.DeepEqual(definition.NativePolicy, policy) {
		return nil, fmt.Errorf("retained native task read-only policy changed")
	}
	return policy, nil
}
