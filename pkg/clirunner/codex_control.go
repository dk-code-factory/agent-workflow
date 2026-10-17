package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type codexControl struct {
	mu            sync.Mutex
	rpc           *appRPC
	thread, turn  string
	completed     chan struct{}
	completeOnce  sync.Once
	interruptOnce sync.Once
	cleanup       func()
	timer         *time.Timer
}

func newCodexControl(cleanup func()) *codexControl {
	return &codexControl{cleanup: cleanup, completed: make(chan struct{})}
}
func (c *codexControl) identity() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thread, c.turn
}
func (c *codexControl) setThread(thread string) { c.mu.Lock(); c.thread = thread; c.mu.Unlock() }
func (c *codexControl) setTurn(turn string)     { c.mu.Lock(); c.turn = turn; c.mu.Unlock() }
func (c *codexControl) notify(frame rpcFrame) {
	if frame.Method != "turn/started" && frame.Method != "turn/completed" {
		return
	}
	var params struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(frame.Params, &params) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if params.ThreadID == "" || params.ThreadID != c.thread || params.Turn.ID == "" {
		return
	}
	if frame.Method == "turn/started" && c.turn == "" {
		c.turn = params.Turn.ID
	}
	if frame.Method == "turn/completed" && (c.turn == "" || c.turn == params.Turn.ID) {
		c.completeOnce.Do(func() { close(c.completed) })
	}
}
func (c *codexControl) interrupt() error {
	c.interruptOnce.Do(func() {
		c.mu.Lock()
		rpc, thread, turn := c.rpc, c.thread, c.turn
		c.timer = time.AfterFunc(5*time.Second, c.cleanup)
		c.mu.Unlock()
		if rpc == nil || thread == "" || turn == "" {
			c.cleanup()
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = rpc.callContext(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
		}()
	})
	return nil
}
func (c *codexControl) finish(ctx context.Context) {
	if ctx.Err() != nil {
		c.mu.Lock()
		rpc, turn := c.rpc, c.turn
		c.mu.Unlock()
		if rpc != nil && turn != "" {
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-c.completed:
			case <-rpc.done:
			case <-timer.C:
			}
			timer.Stop()
		}
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
	}
	c.mu.Unlock()
}

func codexRequestHandler(agent Agent, req workflows.SessionRequest, control *codexControl) func(context.Context, rpcFrame) (any, error) {
	return func(ctx context.Context, frame rpcFrame) (any, error) {

		if frame.Method != "item/tool/call" || req.Tools == nil || req.Tools.Binding == nil {
			return nil, fmt.Errorf("engine denies unconfigured interactive request")
		}
		var call struct {
			ThreadID  string          `json:"threadId"`
			TurnID    string          `json:"turnId"`
			CallID    string          `json:"callId"`
			Tool      string          `json:"tool"`
			Namespace *string         `json:"namespace"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(frame.Params, &call); err != nil {
			return nil, fmt.Errorf("invalid dynamic tool request")
		}
		thread, turn := control.identity()
		if call.ThreadID != thread || call.TurnID != turn || call.CallID == "" || call.Namespace != nil {
			return nil, fmt.Errorf("dynamic tool identity does not match invocation")
		}
		var definition *workflows.ToolDefinition
		for _, d := range req.Tools.Binding.Catalog() {
			if workflows.ToolWireName(d) == call.Tool {
				copy := d
				definition = &copy
				break
			}
		}
		if definition == nil {
			return nil, fmt.Errorf("dynamic tool is outside broker grants")
		}
		// Upstream 0.161.0 does not provide a certified invocation-level
		// quiescence barrier. Keep its lease occupied; managed worker admission
		// remains closed rather than guessing that one callback blocks the turn.
		result, err := req.Tools.Binding.Call(ctx, workflows.ToolRequest{CallID: call.CallID, Server: definition.Server, Tool: definition.Name, Arguments: call.Arguments})
		if req.Tools.Lease != nil {
			if acquireErr := req.Tools.Lease.Reacquire(ctx); acquireErr != nil {
				return nil, acquireErr
			}
		}
		text := string(result)
		success := err == nil
		if err != nil {
			text = err.Error()
		}
		return map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": text}}, "success": success}, nil
	}
}
