package controller

import (
	"context"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestAdmissionRequestIdentityIncludesTaskGroup(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 2), release: make(chan struct{})}
	server, client, _, _ := fixture(t, agent)
	// Always release the fake provider, including on an assertion failure.
	defer close(agent.release)
	ctx := context.Background()
	request := StartRequest{RequestID: "task-group-request", Ref: "workflows/main.yml", TaskGroup: "retained-group"}
	var first, replay workflows.Run
	if err := client.Call(ctx, "POST", "/runs", request, &first); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "POST", "/runs", request, &replay); err != nil || first.ID != replay.ID {
		t.Fatalf("admission replay changed: %s %s, %v", first.ID, replay.ID, err)
	}
	_, snapshot, err := server.Store.Load(ctx, first.ID)
	if err != nil || snapshot.Metadata["task_group"] != request.TaskGroup {
		t.Fatalf("missing admitted group: %+v, %v", snapshot.Metadata, err)
	}
	for _, changed := range []string{"other-group", ""} {
		request.TaskGroup = changed
		if err := client.Call(ctx, "POST", "/runs", request, nil); err == nil {
			t.Fatalf("request ID accepted changed group %q", changed)
		}
	}
}
