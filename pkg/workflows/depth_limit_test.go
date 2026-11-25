package workflows

import (
	"fmt"
	"strings"
	"testing"
)

const depthLimitLeaf = `on:
  workflow_call:
    outputs:
      answer: {value: '${{ jobs.leaf.outputs.answer }}'}
jobs:
  leaf:
    outputs:
      answer: ${{ steps.task.text }}
    steps:
      - id: task
        uses: agent/run
        with: {agent: reviewer.toml, prompt: work}
`

func writeDepthLimitChain(t *testing.T, root string, depth int) string {
	t.Helper()
	for level := 0; level <= depth; level++ {
		source := depthLimitLeaf
		if level < depth {
			source = fmt.Sprintf(`on:
  workflow_call:
    outputs:
      answer: {value: '${{ jobs.child.outputs.answer }}'}
jobs:
  child:
    uses: workflows/depth-%d.yml
`, level+1)
		}
		writeWorkflow(t, root, fmt.Sprintf("depth-%d.yml", level), source)
	}
	return "workflows/depth-0.yml"
}

func TestReusableWorkflowDepth200ExecutesAndResumesAtCapacityOne(t *testing.T) {
	e, _, fake := agentTaskFixture(t, depthLimitLeaf, func(SessionRequest) (map[string]any, error) {
		return map[string]any{"text": "done"}, nil
	})
	ref := writeDepthLimitChain(t, e.WorkspaceDir, 200)
	definitions, err := e.Load(ref)
	if err != nil || len(definitions) != 201 {
		t.Fatalf("full-depth admission: definitions=%d error=%v", len(definitions), err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusSucceeded || run.Outputs["answer"] != "done" {
		t.Fatalf("full-depth result: status=%s outputs=%v", run.Status, run.Outputs)
	}
	resumed, err := e.Resume(t.Context(), run.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != RunStatusSucceeded || resumed.Outputs["answer"] != "done" {
		t.Fatalf("full-depth resume: status=%s outputs=%v", resumed.Status, resumed.Outputs)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("completed leaf was replayed: requests=%d", len(fake.requests))
	}
}

func TestReusableWorkflowDepth201RejectedBeforeDispatch(t *testing.T) {
	e, _, fake := agentTaskFixture(t, depthLimitLeaf, nil)
	ref := writeDepthLimitChain(t, e.WorkspaceDir, 201)
	if _, err := e.Load(ref); err == nil || !strings.Contains(err.Error(), "depth exceeds 200") {
		t.Fatalf("over-depth admission: %v", err)
	}
	leaf, err := Parse([]byte(depthLimitLeaf))
	if err != nil {
		t.Fatal(err)
	}
	// Available definitions must still meet the execution-time depth fence.
	all := map[string]*Workflow{"workflows/leaf.yml": leaf}
	if _, err := e.execute(t.Context(), "workflows/leaf.yml", RunRequest{}, all, e.Slots, 201, nil); err == nil || !strings.Contains(err.Error(), "depth exceeds 200") {
		t.Fatalf("over-depth execution: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatal("above-limit workflow dispatched", fake.requests)
	}
}

func TestReusableWorkflowDefinitionLimitIncludesSiblingCalls(t *testing.T) {
	e, _, fake := agentTaskFixture(t, depthLimitLeaf, nil)
	var source strings.Builder
	source.WriteString("jobs:\n")
	for i := 0; i < 201; i++ {
		writeWorkflow(t, e.WorkspaceDir, fmt.Sprintf("leaf-%d.yml", i), depthLimitLeaf)
		fmt.Fprintf(&source, "  child_%d:\n    uses: workflows/leaf-%d.yml\n", i, i)
		if i == 199 {
			writeWorkflow(t, e.WorkspaceDir, "main.yml", source.String())
			definitions, err := e.Load("workflows/main.yml")
			if err != nil || len(definitions) != 201 {
				t.Fatalf("201-definition admission: definitions=%d error=%v", len(definitions), err)
			}
		}
	}
	writeWorkflow(t, e.WorkspaceDir, "main.yml", source.String())
	if _, err := e.Load("workflows/main.yml"); err == nil || !strings.Contains(err.Error(), "closure exceeds 201 definitions") {
		t.Fatalf("202-definition admission: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatal("admission dispatched workflows", fake.requests)
	}
}
