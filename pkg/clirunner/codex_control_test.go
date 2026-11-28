package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestCodexControlFinishJoinsInterruptBeforeIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		calls := 0
		control := newCodexControl(func() { calls++; <-release })
		interrupted := make(chan struct{})
		go func() { _ = control.interrupt(); close(interrupted) }()
		synctest.Wait()
		if calls != 1 {
			t.Fatal("missing identity did not request immediate termination")
		}
		finished := make(chan struct{})
		go func() { control.finish(context.Background()); close(finished) }()
		synctest.Wait()
		select {
		case <-finished:
			t.Fatal("finish did not join immediate cancellation")
		default:
		}
		unblock()
		<-interrupted
		<-finished
		_ = control.interrupt()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("immediate cancellation repeated: %d calls", calls)
		}
	})
}

func TestCodexControlFinishBeforeLateInterrupt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		control := newCodexControl(func() { calls++ })
		control.finish(context.Background())
		_ = control.interrupt()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if calls != 0 {
			t.Fatal("late interrupt escaped completed teardown")
		}
	})
}

func TestCodexControlInterruptPreservesIdentityAndCompletionGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, peer := net.Pipe()
		defer peer.Close()
		calls := 0
		control := newCodexControl(func() { calls++ })
		control.setThread("original-thread")
		control.setTurn("original-turn")
		rpc := newAppRPC(context.Background(), client, client, nil, control.notify)
		defer rpc.Close()
		control.rpc = rpc
		request := make(chan error, 1)
		go func() {
			var frame rpcFrame
			if err := json.NewDecoder(peer).Decode(&frame); err != nil {
				request <- err
				return
			}
			var params map[string]string
			if json.Unmarshal(frame.Params, &params) != nil || frame.Method != "turn/interrupt" || params["threadId"] != "original-thread" || params["turnId"] != "original-turn" {
				request <- fmt.Errorf("changed interruption identity: %+v", frame)
				return
			}
			encoder := json.NewEncoder(peer)
			if err := encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{}}); err != nil {
				request <- err
				return
			}
			request <- encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "original-thread", "turn": map[string]any{"id": "original-turn"}}})
		}()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = control.interrupt()
		control.finish(ctx)
		if err := <-request; err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if calls != 0 {
			t.Fatal("completed interruption retained a fallback timer")
		}
	})
}
