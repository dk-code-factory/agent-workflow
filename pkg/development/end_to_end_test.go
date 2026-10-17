package development

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type providerFunc func(context.Context, workflows.AgentRequest) (map[string]any, error)

func (f providerFunc) RunAgent(ctx context.Context, req workflows.AgentRequest) (map[string]any, error) {
	return f(ctx, req)
}

// This is credential-free: real Git and Go commands, fake model turns, and a
// fake GitHub process backed by a local bare remote. It exercises the published
// example, including a failed first iteration and parent/child workspace scope.
func TestDevelopmentWorkflowFixesDefectAndPublishesOnce(t *testing.T) {
	r, _ := fixture(t)
	ctx := context.Background()
	files := map[string]string{"go.mod": "module pilot\n\ngo 1.25.0\n", "calc.go": "package pilot\nfunc Add(a,b int) int { return a - b }\n", "calc_test.go": "package pilot\nimport \"testing\"\nfunc TestAdd(t *testing.T){ if Add(2,3)!=5{t.Fatal(\"wrong sum\")} }\n", ".gitignore": ".agent-workflow/\n"}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(r.Root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"develop.yml", "develop-iteration.yml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(r.Root, "workflows", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.git(ctx, r.Root, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := r.git(ctx, r.Root, "commit", "-m", "Seed development workflow"); err != nil {
		t.Fatal(err)
	}
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	var mu sync.Mutex
	builds := 0
	var runID, githubState string
	agents := providerFunc(func(ctx context.Context, req workflows.AgentRequest) (map[string]any, error) {
		if strings.Contains(req.Prompt, "Implement the task in this isolated workspace") {
			mu.Lock()
			builds++
			iteration := builds
			mu.Unlock()
			expression := "a + b + 1"
			if iteration > 1 {
				prior, err := os.ReadFile(filepath.Join(req.Workspace, "calc.go"))
				if err != nil {
					return nil, err
				}
				if !strings.Contains(string(prior), "a + b + 1") {
					t.Error("repeat did not use the previous candidate commit")
				}
				expression = "a + b"
			}
			data := "package pilot\nfunc Add(a,b int) int {\n return " + expression + "\n}\n"
			if err := os.WriteFile(filepath.Join(req.Workspace, "calc.go"), []byte(data), 0600); err != nil {
				return nil, err
			}
			return map[string]any{"text": "Implemented candidate"}, nil
		}
		if strings.Contains(strings.ToLower(req.Prompt), "review") {
			code, err := os.ReadFile(filepath.Join(req.Workspace, "calc.go"))
			if err != nil {
				return nil, err
			}
			clean := strings.Contains(string(code), "return a + b\n")
			findings := []string{}
			if !clean {
				findings = []string{"Add still returns the wrong sum"}
			}
			sha, err := r.git(ctx, req.Workspace, "rev-parse", "HEAD")
			if err != nil {
				return nil, err
			}
			payload := []map[string]any{{"number": 1, "html_url": "https://github.com/owner/repository/pull/1", "state": "open", "head": map[string]any{"sha": sha, "ref": "agent-workflow/" + runID, "repo": map[string]string{"full_name": "owner/repository"}}, "base": map[string]string{"ref": "main"}}}
			encoded, _ := json.Marshal(payload)
			mu.Lock()
			err = os.WriteFile(filepath.Join(githubState, "payload.json"), encoded, 0600)
			mu.Unlock()
			if err != nil {
				return nil, err
			}
			response := map[string]any{"clean": clean}
			if properties, ok := req.Output.Schema["properties"].(map[string]any); ok && properties["findings"] != nil {
				response["findings"] = findings
			}
			text, _ := json.Marshal(response)
			return map[string]any{"text": string(text)}, nil
		}
		return map[string]any{"text": "Correct Add and verify its test"}, nil
	})
	executor := &workflows.Executor{WorkspaceDir: r.Root, Store: r.Store, Actions: r, Agents: agents, MaxParallel: 4}
	admitted, err := executor.Prepare(ctx, workflows.RunRequest{Ref: "workflows/develop.yml", Inputs: map[string]any{"task": "Fix Add", "check": "go test ./...", "title": "Fix Add"}})
	if err != nil {
		t.Fatal(err)
	}
	runID = admitted.ID
	githubState = fakeGitHub(t, r, runID, "unpublished", false)
	result, err := executor.Resume(ctx, runID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != workflows.RunStatusSucceeded || len(result.Jobs["iterate"].Iterations) != 2 || builds != 2 {
		t.Fatalf("bounded fix cycle failed: builds=%d status=%s iterations=%d", builds, result.Status, len(result.Jobs["iterate"].Iterations))
	}
	if result.Outputs["pr"] != "https://github.com/owner/repository/pull/1" {
		t.Fatal("publication output missing", result.Outputs)
	}
	if _, err = executor.Resume(ctx, runID, nil, false); err != nil {
		t.Fatal(err)
	}
	creates, err := os.ReadFile(filepath.Join(githubState, "creates"))
	if err != nil || string(creates) != "x" {
		t.Fatal("duplicate publication", string(creates), err)
	}
	attempts, err := r.Store.Attempts(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	nested := false
	for _, attempt := range attempts {
		if attempt.RunID != runID {
			nested = true
		}
	}
	if !nested {
		t.Fatal("originating-run attempt history lost reusable child attempts")
	}
	artifacts, err := r.Store.Artifacts(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	nestedArtifact := false
	for _, artifact := range artifacts {
		if artifact.RunID != runID {
			nestedArtifact = true
			if _, err = r.Store.Artifact(ctx, runID, artifact.ID); err != nil {
				t.Fatal("originating run cannot retrieve its child artifact", err)
			}
		}
	}
	if !nestedArtifact {
		t.Fatal("reusable command artifacts are missing from root export")
	}
	caller, err := os.ReadFile(filepath.Join(r.Root, "calc.go"))
	if err != nil || !strings.Contains(string(caller), "a - b") {
		t.Fatal("caller checkout was edited")
	}
}
