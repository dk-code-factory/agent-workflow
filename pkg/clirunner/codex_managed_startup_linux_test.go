//go:build linux

package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type managedStartupConn struct {
	net.Conn
	reads  atomic.Int32
	writes atomic.Int32
}

func (c *managedStartupConn) Read(data []byte) (int, error) {
	c.reads.Add(1)
	return c.Conn.Read(data)
}

func (c *managedStartupConn) Write(data []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(data)
}

type managedStartupReply struct {
	frame rpcFrame
	err   error
}

func queuedManagedStartupPeer(t *testing.T) (*managedStartupConn, <-chan error, <-chan managedStartupReply, func()) {
	t.Helper()
	client, peer := net.Pipe()
	counted := &managedStartupConn{Conn: client}
	request := make(chan error, 1)
	response := make(chan managedStartupReply, 1)
	requestExited := make(chan struct{})
	responseExited := make(chan struct{})
	join := sync.OnceFunc(func() {
		_ = client.Close()
		_ = peer.Close()
		<-requestExited
		<-responseExited
	})
	t.Cleanup(join)
	deadline := time.Now().Add(time.Second)
	if err := client.SetDeadline(deadline); err != nil {
		_ = client.Close()
		_ = peer.Close()
		close(requestExited)
		close(responseExited)
		t.Fatal(err)
	}
	if err := peer.SetDeadline(deadline); err != nil {
		_ = client.Close()
		_ = peer.Close()
		close(requestExited)
		close(responseExited)
		t.Fatal(err)
	}
	go func() {
		defer close(requestExited)
		request <- json.NewEncoder(peer).Encode(map[string]any{"id": "queued", "method": "unexpected/permission", "params": map[string]any{}})
	}()
	go func() {
		defer close(responseExited)
		var frame rpcFrame
		err := json.NewDecoder(peer).Decode(&frame)
		response <- managedStartupReply{frame: frame, err: err}
	}()
	return counted, request, response, join
}

func TestManagedRPCQueuedRequestWaitsForImageVerification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, request, response, joinPeer := queuedManagedStartupPeer(t)
		ctx, cancel := context.WithCancel(t.Context())
		verified := make(chan struct{})
		release := sync.OnceFunc(func() { close(verified) })
		entered := make(chan struct{})
		type startup struct {
			rpc *appRPC
			pid int
			err error
		}
		result := make(chan startup, 1)
		exited := make(chan struct{})
		var active *appRPC
		defer func() {
			cancel()
			release()
			_ = client.Close()
			<-exited
			if active == nil {
				select {
				case row := <-result:
					active = row.rpc
				default:
				}
			}
			if active != nil {
				active.Close()
			}
			joinPeer()
		}()
		go func() {
			defer close(exited)
			rpc, pid, err := startVerifiedManagedRPC(ctx, client, client, func(checkCtx context.Context) (int, error) {
				close(entered)
				select {
				case <-verified:
					return 123, nil
				case <-checkCtx.Done():
					return 0, context.Cause(checkCtx)
				}
			}, nil, nil)
			result <- startup{rpc: rpc, pid: pid, err: err}
		}()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("image verification was not entered")
		}
		if client.reads.Load() != 0 || client.writes.Load() != 0 {
			t.Fatal("protocol IO started before verification", client.reads.Load(), client.writes.Load())
		}
		select {
		case row := <-response:
			t.Fatal("queued request received a pre-verification response", row)
		default:
		}
		release()
		synctest.Wait()
		select {
		case row := <-result:
			active = row.rpc
			if row.err != nil || row.rpc == nil || row.pid != 123 {
				t.Fatal("verified RPC did not start", row)
			}
		default:
			t.Fatal("RPC startup did not finish after verification")
		}
		select {
		case err := <-request:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("verified reader did not consume queued request")
		}
		select {
		case row := <-response:
			if row.err != nil || string(row.frame.ID) != `"queued"` || len(row.frame.Error) == 0 {
				t.Fatal("maintained RPC did not deny the queued request", row)
			}
		default:
			t.Fatal("verified RPC did not return its actual denial response")
		}
	})
}

func TestManagedRPCFailedVerificationStartsNoProtocolIO(t *testing.T) {
	for _, variant := range []string{"rejected", "canceled-before-verification", "canceled-during-verification"} {
		t.Run(variant, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, request, response, joinPeer := queuedManagedStartupPeer(t)
				defer joinPeer()
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				rejected := errors.New("owned image verification rejected")
				if variant == "canceled-before-verification" {
					cancel(rejected)
				}
				calls := 0
				rpc, _, err := startVerifiedManagedRPC(ctx, client, client, func(context.Context) (int, error) {
					calls++
					if variant == "canceled-during-verification" {
						cancel(rejected)
						return 123, nil
					}
					return 0, rejected
				}, nil, nil)
				if rpc != nil {
					defer rpc.Close()
				}
				synctest.Wait()
				wantCalls := 1
				if variant == "canceled-before-verification" {
					wantCalls = 0
				}
				if rpc != nil || !errors.Is(err, rejected) || calls != wantCalls || client.reads.Load() != 0 || client.writes.Load() != 0 {
					t.Fatal("unverified or canceled startup opened protocol IO", rpc, err, calls, client.reads.Load(), client.writes.Load())
				}
				select {
				case row := <-response:
					t.Fatal("failed verification produced a protocol response", row)
				default:
				}
				select {
				case err := <-request:
					t.Fatal("failed verification consumed the queued request", err)
				default:
				}
			})
		})
	}
}
