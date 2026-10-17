package clirunner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type rpcFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}
type appRPC struct {
	readerCloser   io.Closer
	writerCloser   io.Closer
	closeOnce      sync.Once
	ctx            context.Context
	cancel         context.CancelFunc
	scanner        *bufio.Scanner
	writer         io.Writer
	writeMu        sync.Mutex
	mu             sync.Mutex
	sequence       int
	pending        map[string]chan rpcFrame
	notifications  chan rpcFrame
	done           chan struct{}
	err            error
	handle         func(context.Context, rpcFrame) (any, error)
	onNotification func(rpcFrame)
	requests       chan struct{}
	wg             sync.WaitGroup
	emit           func(workflows.ProviderEvent)
}

func newAppRPC(ctx context.Context, reader io.Reader, writer io.Writer, handler func(context.Context, rpcFrame) (any, error), notify func(rpcFrame)) *appRPC {
	ctx, cancel := context.WithCancel(ctx)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxOutput)
	rpc := &appRPC{ctx: ctx, cancel: cancel, scanner: scanner, writer: writer, pending: map[string]chan rpcFrame{}, notifications: make(chan rpcFrame, 1000), done: make(chan struct{}), handle: handler, onNotification: notify, requests: make(chan struct{}, 128)}
	rpc.readerCloser, _ = reader.(io.Closer)
	rpc.writerCloser, _ = writer.(io.Closer)
	go rpc.readLoop()
	return rpc
}
func (rpc *appRPC) write(value any) error {
	rpc.writeMu.Lock()
	defer rpc.writeMu.Unlock()
	return json.NewEncoder(rpc.writer).Encode(value)
}

func (rpc *appRPC) readLoop() {
	defer close(rpc.done)
	defer rpc.cancel()
	total := 0
	for rpc.scanner.Scan() {
		total += len(rpc.scanner.Bytes())
		if total > maxOutput {
			rpc.fail(fmt.Errorf("app-server stream exceeds limit"))
			return
		}
		var frame rpcFrame
		if err := json.Unmarshal(rpc.scanner.Bytes(), &frame); err != nil {
			rpc.fail(fmt.Errorf("invalid app-server frame: %w", err))
			return
		}
		if len(frame.ID) > 0 && frame.Method != "" {
			select {
			case rpc.requests <- struct{}{}:
			default:
				rpc.fail(fmt.Errorf("app-server callback concurrency exceeds 128"))
				return
			}
			rpc.wg.Add(1)
			go func(frame rpcFrame) {
				defer rpc.wg.Done()
				defer func() { <-rpc.requests }()
				response := map[string]any{"id": frame.ID}
				if rpc.handle == nil {
					response["error"] = map[string]any{"code": -32601, "message": "engine denies unconfigured interactive requests"}
				} else {
					result, err := rpc.handle(rpc.ctx, frame)
					if err != nil {
						response["error"] = map[string]any{"code": -32601, "message": err.Error()}
					} else {
						response["result"] = result
					}
				}
				if err := rpc.write(response); err != nil {
					rpc.fail(err)
					rpc.cancel()
				}
			}(frame)
		} else if len(frame.ID) > 0 {
			rpc.mu.Lock()
			response := rpc.pending[string(frame.ID)]
			rpc.mu.Unlock()
			if response != nil {
				select {
				case response <- frame:
				default:
					rpc.fail(fmt.Errorf("duplicate app-server RPC response"))
					return
				}
			}
		} else if frame.Method != "" {
			if rpc.onNotification != nil {
				rpc.onNotification(frame)
			}
			select {
			case rpc.notifications <- frame:
			default:
				rpc.fail(fmt.Errorf("app-server notification backlog exceeds limit"))
				return
			}
		} else {
			rpc.fail(fmt.Errorf("app-server frame has neither identity nor method"))
			return
		}
	}
	if err := rpc.scanner.Err(); err != nil {
		rpc.fail(err)
	} else {
		rpc.fail(io.ErrUnexpectedEOF)
	}
}
func (rpc *appRPC) fail(err error) {
	rpc.mu.Lock()
	if rpc.err == nil {
		rpc.err = err
	}
	rpc.mu.Unlock()
}
func (rpc *appRPC) failure() error {
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if rpc.err != nil {
		return rpc.err
	}
	return io.ErrUnexpectedEOF
}
func (rpc *appRPC) Close() {
	rpc.closeOnce.Do(func() {
		rpc.cancel()
		if rpc.writerCloser != nil {
			_ = rpc.writerCloser.Close()
		}
		if rpc.readerCloser != nil {
			_ = rpc.readerCloser.Close()
		}
		<-rpc.done
		rpc.wg.Wait()
	})
}

func (rpc *appRPC) call(method string, params any) (json.RawMessage, error) {
	return rpc.callContext(rpc.ctx, method, params)
}
func (rpc *appRPC) callContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	rpc.mu.Lock()
	rpc.sequence++
	id := rpc.sequence
	key := strconv.Itoa(id)
	response := make(chan rpcFrame, 1)
	rpc.pending[key] = response
	rpc.mu.Unlock()
	defer func() { rpc.mu.Lock(); delete(rpc.pending, key); rpc.mu.Unlock() }()
	if err := rpc.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case frame := <-response:
		if len(frame.Error) > 0 && string(frame.Error) != "null" {
			return nil, fmt.Errorf("app-server %s failed: %s", method, frame.Error)
		}
		return frame.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-rpc.done:
		select {
		case frame := <-response:
			if len(frame.Error) > 0 && string(frame.Error) != "null" {
				return nil, fmt.Errorf("app-server %s failed: %s", method, frame.Error)
			}
			return frame.Result, nil
		default:
			return nil, rpc.failure()
		}
	}
}
func (rpc *appRPC) next() (rpcFrame, error) {
	select {
	case frame := <-rpc.notifications:
		return frame, nil
	default:
	}
	select {
	case frame := <-rpc.notifications:
		return frame, nil
	case <-rpc.done:
		select {
		case frame := <-rpc.notifications:
			return frame, nil
		default:
			return rpcFrame{}, rpc.failure()
		}
	case <-rpc.ctx.Done():
		select {
		case frame := <-rpc.notifications:
			return frame, nil
		default:
			return rpcFrame{}, rpc.failure()
		}
	}
}
