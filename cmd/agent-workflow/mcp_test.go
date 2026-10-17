package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPAdmissionWaitAndArtifactHandles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	source := "jobs: {x: {steps: [{id: check, argv: [printf, '%s', mcp-output]}]}}"
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".agent-workflow")
	store, err := workflows.OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer store.Close()
	owner := &controller.Server{Dir: dir, Store: store, Executor: &workflows.Executor{WorkspaceDir: root, Store: store}}
	done := make(chan error, 1)
	go func() { done <- owner.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	api := controller.NewClient(dir)
	for api.Call(ctx, "GET", "/health", nil, nil) != nil {
		if err := pause(ctx, time.Millisecond); err != nil {
			t.Fatal("controller did not start", err)
		}
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := newMCPServer(api)
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "conformance-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(name string, args any) map[string]any {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s: %+v", name, result.Content)
		}
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err = json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	definitions := call("workflow_discover", map[string]any{})
	if len(definitions["workflows"].([]any)) != 1 {
		t.Fatal(definitions)
	}
	request := map[string]any{"request_id": "mcp-request-1", "ref": "workflows/main.yml"}
	one := call("workflow_start", request)
	two := call("workflow_start", request)
	if one["id"] != two["id"] {
		t.Fatal("duplicate MCP admission")
	}
	id := one["id"].(string)
	final := call("run_wait", map[string]any{"id": id, "seconds": 5})
	if final["status"] != workflows.RunStatusSucceeded {
		t.Fatal(final)
	}
	artifacts := call("artifact_list", map[string]any{"id": id})["artifacts"].([]any)
	if len(artifacts) != 2 {
		t.Fatal(artifacts)
	}
	for _, entry := range artifacts {
		artifact := entry.(map[string]any)
		if artifact["name"] == "stdout" {
			content := call("artifact_get", map[string]any{"id": id, "artifact_id": artifact["id"]})
			if content["text"] != "mcp-output" {
				t.Fatal(content)
			}
		}
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "run_wait", Arguments: map[string]any{"id": id, "seconds": 21}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("unbounded MCP wait accepted")
	}
}
