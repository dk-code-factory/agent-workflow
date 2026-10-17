package controller

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/operational"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func TestReadinessAndMetricsRemainPrivateAndPromptFree(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	_, client, _, _ := fixture(t, agent)
	var readiness Readiness
	if err := client.Call(context.Background(), "GET", "/ready", nil, &readiness); err != nil || !readiness.Ready {
		t.Fatal(readiness, err)
	}
	request, err := http.NewRequest("GET", "http://unix/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.HTTP.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "agent_workflow_suspended_slots") || strings.Contains(string(data), "run_id=") || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain") {
		t.Fatal("metrics contract violated", string(data))
	}
}

func TestUnavailableDiskPrerequisitesRejectAdmissionButKeepInspection(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte("jobs: {x: {steps: [{argv: [true]}]}}"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".agent-workflow")
	store, err := workflows.OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	maximum := uint64(math.MaxUint64)
	server := &Server{Dir: dir, Store: store, Executor: &workflows.Executor{WorkspaceDir: root, Store: store}, Operational: operational.Config{MinFreeBytes: &maximum}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := NewClient(dir)
	deadline := time.Now().Add(3 * time.Second)
	for client.Call(ctx, "GET", "/health", nil, nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("controller failed to start")
		}
		time.Sleep(time.Millisecond)
	}
	if err = client.Call(ctx, "POST", "/runs", StartRequest{Ref: "workflows/main.yml"}, nil); err == nil {
		t.Fatal("disk-unready controller admitted effects")
	}
	var page map[string]any
	if err = client.Call(ctx, "GET", "/runs?limit=100", nil, &page); err != nil {
		t.Fatal("inspection unavailable while unready", err)
	}
	response, err := client.HTTP.Get("http://unix/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status Readiness
	_ = json.NewDecoder(response.Body).Decode(&status)
	if response.StatusCode != 503 || status.Ready || status.Checks["state_capacity"] != "unavailable" {
		t.Fatal("readiness failed to expose capacity gate", response.StatusCode, status)
	}
}
