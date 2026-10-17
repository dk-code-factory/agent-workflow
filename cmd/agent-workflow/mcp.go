package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/dkropachev/agent-workflow/internal/buildinfo"
	"github.com/dkropachev/agent-workflow/pkg/controller"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type artifactQuery struct {
	ID         string `json:"id"`
	ArtifactID string `json:"artifact_id"`
}

type runHandle struct {
	ID string `json:"id" jsonschema:"durable run ID"`
}
type eventQuery struct {
	ID    string `json:"id"`
	After int64  `json:"after,omitempty"`
}
type resumeQuery struct {
	RequestID      string `json:"request_id,omitempty"`
	ID             string `json:"id"`
	RetryUncertain bool   `json:"retry_uncertain,omitempty"`
}
type waitQuery struct {
	ID      string `json:"id"`
	Seconds int    `json:"seconds,omitempty"`
}

func serveMCP(ctx context.Context, dir string, _ io.Writer) error {
	return newMCPServer(controller.NewClient(dir)).Run(ctx, &mcp.StdioTransport{})
}

func newMCPServer(client *controller.Client) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-workflow", Version: buildinfo.Version}, nil)
	for _, operation := range []string{"backup", "restore", "bundle"} {
		op := operation
		mcp.AddTool(server, &mcp.Tool{Name: "database_" + op, Description: "Database " + op + " requires a stopped controller and exclusive workspace ownership; no model calls."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Path string `json:"path"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			out, err := databaseMaintenance(ctx, client.Dir, op, in.Path)
			return nil, out, err
		})
	}
	mcp.AddTool(server, &mcp.Tool{Name: "controller_stop", Description: "Stop the controller after supervised work stops, allowing exclusive database maintenance."}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "POST", "/shutdown", nil, &out)
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{Name: "run_usage", Description: "Inspect normalized provider-reported usage and cost; unknown cost and incomplete usage remain explicit."}, func(ctx context.Context, _ *mcp.CallToolRequest, in runHandle) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "GET", "/runs/"+in.ID+"/usage", nil, &out)
		return nil, out, err
	})

	mcp.AddTool(server, &mcp.Tool{Name: "workflow_discover", Description: "List available validated workflow definitions and input/output contracts."}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		var out []any
		err := client.Call(ctx, "GET", "/workflows", nil, &out)
		return nil, map[string]any{"workflows": out}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "workflow_start", Description: "Admit a workflow and return a durable handle immediately. Secrets are controller environment references."}, func(ctx context.Context, _ *mcp.CallToolRequest, in controller.StartRequest) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "POST", "/runs", in, &out)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "run_status", Description: "Read a durable workflow result."}, func(ctx context.Context, _ *mcp.CallToolRequest, in runHandle) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "GET", "/runs/"+in.ID, nil, &out)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "run_events", Description: "Poll ordered versioned progress events (up to 1000)."}, func(ctx context.Context, _ *mcp.CallToolRequest, in eventQuery) (*mcp.CallToolResult, map[string]any, error) {
		var out []any
		err := client.Call(ctx, "GET", fmt.Sprintf("/runs/%s/events?after=%d", in.ID, in.After), nil, &out)
		return nil, map[string]any{"events": out}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "run_cancel", Description: "Cancel a run and its supervised subprocess groups."}, func(ctx context.Context, _ *mcp.CallToolRequest, in runHandle) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "POST", "/runs/"+in.ID+"/cancel", nil, &out)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "run_resume", Description: "Resume admitted definitions, retaining completed work. Uncertain effects require an explicit acknowledgement."}, func(ctx context.Context, _ *mcp.CallToolRequest, in resumeQuery) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		suffix := "?retry_uncertain=" + strconv.FormatBool(in.RetryUncertain) + "&request_id=" + url.QueryEscape(in.RequestID)
		err := client.Call(ctx, "POST", "/runs/"+in.ID+"/resume"+suffix, nil, &out)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "run_wait", Description: "Wait at most 20 seconds, then return a pollable status."}, func(ctx context.Context, _ *mcp.CallToolRequest, in waitQuery) (*mcp.CallToolResult, map[string]any, error) {
		if in.Seconds < 0 || in.Seconds > 20 {
			return nil, nil, fmt.Errorf("seconds must be 0..20")
		}
		deadline := time.Now().Add(time.Duration(in.Seconds) * time.Second)
		for {
			var out map[string]any
			err := client.Call(ctx, "GET", "/runs/"+in.ID, nil, &out)
			if err != nil {
				return nil, nil, err
			}
			if out["status"] != "running" && out["status"] != "queued" || !time.Now().Before(deadline) {
				return nil, out, nil
			}
			if err = pause(ctx, 200*time.Millisecond); err != nil {
				return nil, nil, err
			}
		}
	})
	mcp.AddTool(server, &mcp.Tool{Name: "artifact_get", Description: "Retrieve a redacted command stdout/stderr artifact scoped to its originating run."}, func(ctx context.Context, _ *mcp.CallToolRequest, in artifactQuery) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "GET", "/runs/"+in.ID+"/artifacts/"+in.ArtifactID, nil, &out)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "artifact_list", Description: "List command output artifacts for a run."}, func(ctx context.Context, _ *mcp.CallToolRequest, in runHandle) (*mcp.CallToolResult, map[string]any, error) {
		var out []any
		err := client.Call(ctx, "GET", "/runs/"+in.ID+"/artifacts", nil, &out)
		return nil, map[string]any{"artifacts": out}, err
	})
	addWorkerTools(server, client)
	return server
}

type workerHandle struct {
	RunID    string `json:"run_id"`
	WorkerID string `json:"worker_id"`
}
type workerSend struct {
	RunID     string `json:"run_id"`
	WorkerID  string `json:"worker_id"`
	RequestID string `json:"request_id"`
	Prompt    string `json:"prompt"`
}
type workerWait struct {
	RunID    string `json:"run_id"`
	WorkerID string `json:"worker_id"`
	Seconds  int    `json:"seconds,omitempty"`
}

func addWorkerTools(server *mcp.Server, client *controller.Client) {
	mcp.AddTool(server, &mcp.Tool{Name: "worker_spawn", Description: "Spawn a run-scoped worker with a durable resumable session. Child tools and budgets cannot exceed its parent."}, func(ctx context.Context, _ *mcp.CallToolRequest, in workflows.WorkerSpec) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "POST", "/runs/"+in.RunID+"/workers", in, &out)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "worker_send", Description: "Durably enqueue a message. Retrying the same request_id and content returns the same acknowledgement."}, func(ctx context.Context, _ *mcp.CallToolRequest, in workerSend) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "POST", "/runs/"+in.RunID+"/workers/"+in.WorkerID+"/send", controller.SendRequest{RequestID: in.RequestID, Prompt: in.Prompt}, &out)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "worker_wait", Description: "Poll a worker mailbox for at most 20 seconds. This wait does not consume an execution slot."}, func(ctx context.Context, _ *mcp.CallToolRequest, in workerWait) (*mcp.CallToolResult, map[string]any, error) {
		if in.Seconds < 0 || in.Seconds > 20 {
			return nil, nil, fmt.Errorf("seconds must be 0..20")
		}
		deadline := time.Now().Add(time.Duration(in.Seconds) * time.Second)
		for {
			var messages []workflows.WorkerMessage
			if err := client.Call(ctx, "GET", "/runs/"+in.RunID+"/workers/"+in.WorkerID+"/messages", nil, &messages); err != nil {
				return nil, nil, err
			}
			pending := false
			for _, msg := range messages {
				if msg.Status == workflows.RunStatusRunning || msg.Status == workflows.RunStatusQueued {
					pending = true
				}
			}
			if !pending || !time.Now().Before(deadline) {
				return nil, map[string]any{"messages": messages, "pending": pending}, nil
			}
			if err := pause(ctx, 100*time.Millisecond); err != nil {
				return nil, nil, err
			}
		}
	})
	for _, operation := range []string{"interrupt", "close"} {
		operation := operation
		mcp.AddTool(server, &mcp.Tool{Name: "worker_" + operation, Description: operation + " a worker while retaining its durable mailbox and provider session ID."}, func(ctx context.Context, _ *mcp.CallToolRequest, in workerHandle) (*mcp.CallToolResult, map[string]any, error) {
			var out map[string]any
			err := client.Call(ctx, "POST", "/runs/"+in.RunID+"/workers/"+in.WorkerID+"/"+operation, nil, &out)
			return nil, out, err
		})
	}
	mcp.AddTool(server, &mcp.Tool{Name: "worker_status", Description: "Read a worker's session ID, status, and remaining budget."}, func(ctx context.Context, _ *mcp.CallToolRequest, in workerHandle) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		err := client.Call(ctx, "GET", "/runs/"+in.RunID+"/workers/"+in.WorkerID, nil, &out)
		return nil, out, err
	})
}
