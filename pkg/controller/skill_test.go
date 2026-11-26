package controller

import (
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillAdmissionRequestIDPinsCapturedBytes(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 2), release: make(chan struct{})}
	s, client, _, _ := fixture(t, agent)
	root := t.TempDir()
	entry := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(entry, []byte("---\nname: selected\ndescription: Fixture.\n---\noriginal"), 0600); err != nil {
		t.Fatal(err)
	}
	source := "on: {workflow_call: {inputs: {selected: {type: skill, required: true}}}}\njobs: {x: {steps: [{uses: agent/run, with: {prompt: hello, agent: a.yaml}}]}}"
	if err := os.WriteFile(filepath.Join(s.Executor.WorkspaceDir, "workflows/main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	request := StartRequest{RequestID: "skill-idempotency", Ref: "workflows/main.yml", Inputs: map[string]any{"selected": map[string]any{"directory": root}}}
	var one, two workflows.Run
	if err := client.Call(t.Context(), "POST", "/runs", request, &one); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(t.Context(), "POST", "/runs", request, &two); err != nil || one.ID != two.ID {
		t.Fatal("same bytes did not deduplicate", one, two, err)
	}
	if err := os.WriteFile(entry, []byte("---\nname: selected\ndescription: Fixture.\n---\nchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(t.Context(), "POST", "/runs", request, nil); err == nil {
		t.Fatal("same input path with changed bytes reused admission")
	}
	close(agent.release)
	awaitStatus(t, client, one.ID, workflows.RunStatusSucceeded)
	if agent.calls.Load() != 1 {
		t.Fatal("idempotency duplicated provider effects")
	}
}
