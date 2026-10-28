package controller

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestShutdownResponseCompletesBeforeConnectionClose(t *testing.T) {
	s := &Server{Executor: &workflows.Executor{}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	stopped := make(chan struct{})
	s.stop = func() {
		// Force the production shutdown ordering: close the connection after
		// Flush, before handle returns and net/http finishes its response.
		server.CloseClientConnections()
		close(stopped)
	}
	server.Start()
	t.Cleanup(server.Close)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &Client{HTTP: &http.Client{Transport: transport}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// With no output destination, Call reads the entire response, including
	// HTTP framing, just as the CLI and the durable shutdown tests do.
	if err := client.Call(ctx, http.MethodPost, "/shutdown", nil, nil); err != nil {
		t.Fatalf("shutdown response was incomplete: %v", err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("shutdown did not stop the controller")
	}
}
