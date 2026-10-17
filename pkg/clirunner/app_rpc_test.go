package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

func TestAppRPCReaderCorrelatesResponsesWhileCallbackWaits(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	blocked := make(chan struct{})
	release := make(chan struct{})
	rpc := newAppRPC(context.Background(), client, client, func(ctx context.Context, frame rpcFrame) (any, error) {
		close(blocked)
		select {
		case <-release:
			return map[string]any{"done": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, nil)
	defer rpc.Close()
	serverDone := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(server)
		encoder := json.NewEncoder(server)
		var request rpcFrame
		if err := decoder.Decode(&request); err != nil {
			serverDone <- err
			return
		}
		_ = encoder.Encode(map[string]any{"id": "callback", "method": "item/tool/call", "params": map[string]any{}})
		_ = encoder.Encode(map[string]any{"method": "progress", "params": map[string]any{"x": 1}})
		_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]bool{"ordinary": true}})
		var response rpcFrame
		err := decoder.Decode(&response)
		if err == nil && string(response.ID) != `"callback"` {
			err = errors.New("callback response identity changed")
		}
		serverDone <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := rpc.callContext(ctx, "ordinary", map[string]any{})
	if err != nil || string(result) != `{"ordinary":true}` {
		t.Fatalf("callback blocked ordinary RPC: %s %v", result, err)
	}
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("callback not dispatched")
	}
	frame, err := rpc.next()
	if err != nil || frame.Method != "progress" {
		t.Fatalf("progress blocked or reordered: %+v %v", frame, err)
	}
	close(release)
	select {
	case err = <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("callback response blocked")
	}
}

func TestAppRPCMalformedFrameReleasesPendingCalls(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	rpc := newAppRPC(context.Background(), client, client, nil, nil)
	defer rpc.Close()
	go func() {
		var request rpcFrame
		_ = json.NewDecoder(server).Decode(&request)
		_, _ = server.Write([]byte("{bad}\n"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := rpc.callContext(ctx, "test", nil); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("malformed input did not promptly fail pending call: %v", err)
	}
}

func TestAppRPCDeniesUnknownInteractiveRequests(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	rpc := newAppRPC(context.Background(), client, client, nil, nil)
	defer rpc.Close()
	if err := json.NewEncoder(server).Encode(map[string]any{"id": 7, "method": "unexpected/permission", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	var response rpcFrame
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	if err := json.NewDecoder(server).Decode(&response); err != nil || len(response.Error) == 0 {
		t.Fatalf("unknown request was not denied: %+v %v", response, err)
	}
}
