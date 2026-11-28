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

func TestAppRPCDistinguishesPeerRejectionFromConnectionLoss(t *testing.T) {
	for _, rejected := range []bool{true, false} {
		client, server := net.Pipe()
		rpc := newAppRPC(context.Background(), client, client, nil, nil)
		go func() {
			defer server.Close()
			var request rpcFrame
			if json.NewDecoder(server).Decode(&request) == nil && rejected {
				_ = json.NewEncoder(server).Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": -32603, "message": "source recovery hold"}})
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := rpc.callContext(ctx, "thread/resume", nil)
		cancel()
		rpc.Close()
		var rejection *appRPCResponseError
		if err == nil || errors.As(err, &rejection) != rejected {
			t.Fatal("source rejection and unacknowledged transport loss collapsed", rejected, err)
		}
		if rejected && err.Error() != `app-server thread/resume failed: {"code":-32603,"message":"source recovery hold"}` {
			t.Fatal("stock diagnostic changed", err)
		}
	}
}

type appRPCWaitForEOFWriter struct {
	net.Conn
	done <-chan struct{}
}

func (w appRPCWaitForEOFWriter) Write(data []byte) (int, error) {
	n, err := w.Conn.Write(data)
	if err == nil {
		select {
		case <-w.done:
		case <-time.After(time.Second):
			return n, errors.New("RPC reader did not observe peer EOF")
		}
	}
	return n, err
}

func TestAppRPCReceivedResponseSurvivesPeerEOF(t *testing.T) {
	for _, rejected := range []bool{true, false} {
		for range 32 {
			client, server := net.Pipe()
			deadline := time.Now().Add(time.Second)
			if err := client.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if err := server.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			writer := &appRPCWaitForEOFWriter{Conn: client}
			rpc := newAppRPC(context.Background(), client, writer, nil, nil)
			writer.done = rpc.done
			go func() {
				defer server.Close()
				var request rpcFrame
				if json.NewDecoder(server).Decode(&request) != nil {
					return
				}
				response := map[string]any{"id": request.ID, "result": map[string]bool{"accepted": true}}
				if rejected {
					delete(response, "result")
					response["error"] = map[string]any{"code": -32603, "message": "source recovery hold"}
				}
				_ = json.NewEncoder(server).Encode(response)
			}()
			result, err := rpc.call("thread/resume", nil)
			rpc.Close()
			var rejection *appRPCResponseError
			if rejected {
				if !errors.As(err, &rejection) {
					t.Fatalf("received peer rejection lost after EOF: %v", err)
				}
			} else if err != nil || string(result) != `{"accepted":true}` {
				t.Fatalf("received successful response lost after EOF: %s %v", result, err)
			}
		}
	}
}
