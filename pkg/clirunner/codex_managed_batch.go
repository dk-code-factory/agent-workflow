package clirunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// This controller is not installed for stock Codex. A verified source profile
// must provide the complete native attestation; callbacks alone release nothing.
type codexParkedBatch struct {
	ThreadID     string          `json:"threadId"`
	TurnID       string          `json:"turnId"`
	InvocationID string          `json:"invocationId"`
	PolicyID     string          `json:"policyId"`
	BuildID      string          `json:"buildId"`
	BatchID      string          `json:"batchId"`
	ResponseID   string          `json:"responseId"`
	CallIDs      []string        `json:"callIds"`
	PermitEpoch  string          `json:"-"`
	Usage        json.RawMessage `json:"-"`
}

type codexSemanticBatch codexParkedBatch
type codexBatchPermit struct {
	Batch       codexSemanticBatch `json:"batch"`
	PermitEpoch string             `json:"permitEpoch"`
	Usage       json.RawMessage    `json:"usage"`
}

func (b codexParkedBatch) MarshalJSON() ([]byte, error) {
	return json.Marshal(codexBatchPermit{Batch: codexSemanticBatch(b), PermitEpoch: b.PermitEpoch, Usage: b.Usage})
}
func (b *codexParkedBatch) UnmarshalJSON(raw []byte) error {
	var value codexBatchPermit
	if err := strictCodexDecode(raw, &value); err != nil {
		return err
	}
	*b = codexParkedBatch(value.Batch)
	b.PermitEpoch, b.Usage = value.PermitEpoch, append(json.RawMessage(nil), value.Usage...)
	return nil
}

func validCodexNativeUsage(raw json.RawMessage) bool {
	if len(raw) > 65536 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for _, name := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		var amount int64
		if bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) || json.Unmarshal(fields[name], &amount) != nil || amount < 0 {
			return false
		}
	}
	return true
}

type codexBrokerCall struct {
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId"`
	CallID    string          `json:"callId"`
	Namespace *string         `json:"namespace"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type codexPendingCall struct {
	call    codexBrokerCall
	request workflows.ToolRequest
	ready   chan struct{}
	result  map[string]any
}

type codexManagedBatchController struct {
	ctx                       context.Context
	cancel                    context.CancelCauseFunc
	mu                        sync.Mutex
	wg                        sync.WaitGroup
	tools                     *workflows.InvocationTools
	catalog                   map[string]workflows.ToolDefinition
	thread                    string
	turn                      string
	invocation, policy, build string
	bound                     bool // the official turn/start reply, not merely turn/started
	pending                   map[string]*codexPendingCall
	seenCalls                 map[string]bool
	seenBatches               map[string]bool
	batch                     *codexParkedBatch
	running                   bool
	err                       error
	interrupt                 func(error)
	// Revalidate the admitted engine owner/fence immediately before dispatch
	// and again after reacquisition. There is no permissive default.
	authorize func(context.Context) error
	observe   func(context.Context, codexParkedBatch) error
}

func newCodexManagedBatchController(ctx context.Context, tools *workflows.InvocationTools, thread, invocation, policy, build string, authorize func(context.Context) error, observe func(context.Context, codexParkedBatch) error, interrupt func(error)) (*codexManagedBatchController, error) {
	if tools == nil || tools.Binding == nil || tools.Lease == nil || thread == "" || invocation == "" || policy == "" || build == "" || authorize == nil || observe == nil {
		return nil, fmt.Errorf("managed broker controller requires pinned authority and a held engine lease")
	}
	catalog := make(map[string]workflows.ToolDefinition)
	for _, definition := range tools.Binding.Catalog() {
		wire := workflows.ToolWireName(definition)
		if _, duplicate := catalog[wire]; duplicate || len(catalog) >= 128 {
			return nil, fmt.Errorf("managed broker catalog is duplicated or exceeds 128 tools")
		}
		catalog[wire] = definition
	}
	ctx, cancel := context.WithCancelCause(ctx)
	return &codexManagedBatchController{ctx: ctx, cancel: cancel, tools: tools, catalog: catalog, thread: thread, invocation: invocation, policy: policy, build: build, pending: map[string]*codexPendingCall{}, seenCalls: map[string]bool{}, seenBatches: map[string]bool{}, authorize: authorize, observe: observe, interrupt: interrupt}, nil
}

func strictCodexDecode(raw json.RawMessage, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("extra managed protocol value")
	}
	return nil
}

func (c *codexManagedBatchController) fail(failure error) {
	c.mu.Lock()
	first := c.err == nil
	if first {
		c.err = failure
		c.cancel(failure)
	}
	c.mu.Unlock()
	if first && c.interrupt != nil {
		c.interrupt(failure)
	}
}

func (c *codexManagedBatchController) bindStart(turn string) error {
	c.mu.Lock()
	if c.bound || turn == "" || c.batch != nil && c.batch.TurnID != turn {
		c.mu.Unlock()
		err := fmt.Errorf("managed official turn identity changed")
		c.fail(err)
		return err
	}
	for _, pending := range c.pending {
		if pending.call.TurnID != turn {
			c.mu.Unlock()
			err := fmt.Errorf("managed callback preceded a conflicting turn/start reply")
			c.fail(err)
			return err
		}
	}
	c.turn, c.bound = turn, true
	c.maybeStartLocked()
	c.mu.Unlock()
	return nil
}

// Called inline by the continuously running RPC event reader. It never waits on
// broker work or capacity reacquisition; each callback stays parked separately.
func (c *codexManagedBatchController) notify(frame rpcFrame) {
	if frame.Method != "agentWorkflow/batchParked" {
		return
	}
	var batch codexParkedBatch
	if err := strictCodexDecode(frame.Params, &batch); err != nil {
		c.fail(err)
		return
	}
	if batch.ThreadID != c.thread || batch.InvocationID != c.invocation || batch.PolicyID != c.policy || batch.BuildID != c.build || batch.TurnID == "" || batch.BatchID == "" || batch.ResponseID == "" || batch.PermitEpoch == "" || len(batch.PermitEpoch) > 256 || !validCodexNativeUsage(batch.Usage) || len(batch.CallIDs) == 0 || len(batch.CallIDs) > 128 || !sort.StringsAreSorted(batch.CallIDs) {
		c.fail(fmt.Errorf("managed attestation identity or complete call set differs"))
		return
	}
	for i, id := range batch.CallIDs {
		if id == "" || len(id) > 256 || i > 0 && id == batch.CallIDs[i-1] {
			c.fail(fmt.Errorf("managed attestation has an invalid or duplicate call"))
			return
		}
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	if c.batch != nil || c.running || c.seenBatches[batch.BatchID] || c.bound && batch.TurnID != c.turn {
		c.mu.Unlock()
		c.fail(fmt.Errorf("managed attestation was repeated, reordered or changed"))
		return
	}
	for id := range c.pending {
		if !containsCodexCall(batch.CallIDs, id) {
			c.mu.Unlock()
			c.fail(fmt.Errorf("managed attestation omitted a pending callback"))
			return
		}
	}
	c.batch = &batch
	c.seenBatches[batch.BatchID] = true
	c.maybeStartLocked()
	c.mu.Unlock()
}

func containsCodexCall(ids []string, id string) bool {
	index := sort.SearchStrings(ids, id)
	return index < len(ids) && ids[index] == id
}

func (c *codexManagedBatchController) handle(ctx context.Context, frame rpcFrame) (any, error) {
	var call codexBrokerCall
	if frame.Method != "item/tool/call" || strictCodexDecode(frame.Params, &call) != nil || call.ThreadID != c.thread || call.TurnID == "" || call.CallID == "" || len(call.CallID) > 256 || call.Namespace != nil || len(call.Arguments) > 1<<20 || !json.Valid(call.Arguments) {
		err := fmt.Errorf("invalid managed broker callback")
		c.fail(err)
		return nil, err
	}
	definition, allowed := c.catalog[call.Tool]
	if !allowed {
		err := fmt.Errorf("managed callback exceeds exact broker grants")
		c.fail(err)
		return nil, err
	}
	pending := &codexPendingCall{call: call, request: workflows.ToolRequest{CallID: call.CallID, Server: definition.Server, Tool: definition.Name, Arguments: append(json.RawMessage(nil), call.Arguments...)}, ready: make(chan struct{})}
	c.mu.Lock()
	if c.err != nil || c.seenCalls[call.CallID] || len(c.seenCalls) >= 4096 || len(c.pending) >= 128 || c.bound && call.TurnID != c.turn || c.batch != nil && (!containsCodexCall(c.batch.CallIDs, call.CallID) || c.batch.TurnID != call.TurnID) {
		c.mu.Unlock()
		err := fmt.Errorf("managed callback is duplicated, late, conflicting or exceeds bounds")
		c.fail(err)
		return nil, err
	}
	c.pending[call.CallID], c.seenCalls[call.CallID] = pending, true
	c.maybeStartLocked()
	c.mu.Unlock()
	select {
	case <-pending.ready:
		if err := context.Cause(c.ctx); err != nil {
			return nil, err
		}
		return pending.result, nil
	case <-c.ctx.Done():
		return nil, context.Cause(c.ctx)
	case <-ctx.Done():
		c.fail(ctx.Err())
		return nil, ctx.Err()
	}
}

func (c *codexManagedBatchController) maybeStartLocked() {
	if c.err != nil || !c.bound || c.batch == nil || c.running || len(c.pending) != len(c.batch.CallIDs) {
		return
	}
	calls := make([]*codexPendingCall, 0, len(c.batch.CallIDs))
	argumentBytes := 0
	for _, id := range c.batch.CallIDs {
		call := c.pending[id]
		if call == nil || call.call.TurnID != c.turn {
			return
		}
		argumentBytes += len(call.call.Arguments)
		calls = append(calls, call)
	}
	if argumentBytes > 8<<20 {
		// The read loop remains live; the worker below will reject before
		// releasing capacity or dispatching an effect.
		c.running = true
		c.wg.Add(1)
		go func() { defer c.wg.Done(); c.fail(fmt.Errorf("managed complete argument batch exceeds bound")) }()
		return
	}
	batch := *c.batch
	batch.CallIDs = append([]string(nil), batch.CallIDs...)
	c.running = true
	c.wg.Add(1)
	go func() { defer c.wg.Done(); c.execute(batch, calls) }()
}

func (c *codexManagedBatchController) execute(batch codexParkedBatch, calls []*codexPendingCall) {
	// Commit the exact original native response observation before capacity
	// release or broker effects. The observer retains known charges even when
	// admission expires or is cancelled; it fences the original owner itself.
	// Every accounting failure holds this entire batch.
	if err := c.observe(c.ctx, batch); err != nil {
		c.fail(err)
		return
	}
	if err := context.Cause(c.ctx); err != nil {
		return
	}
	if err := c.authorize(c.ctx); err != nil {
		c.fail(err)
		return
	}
	if err := c.tools.Lease.Suspend(); err != nil {
		c.fail(err)
		return
	}
	type outcome struct {
		call  *codexPendingCall
		value json.RawMessage
		err   error
	}
	completed := make(chan outcome, len(calls))
	var work sync.WaitGroup
	for _, call := range calls {
		work.Add(1)
		go func(call *codexPendingCall) {
			defer work.Done()
			value, err := c.tools.Binding.Call(c.ctx, call.request)
			if err != nil && !workflows.KnownToolDenial(c.ctx, c.tools.Binding, call.request, err) {
				c.fail(err)
			}
			completed <- outcome{call, value, err}
		}(call)
	}
	work.Wait()
	close(completed)
	if err := context.Cause(c.ctx); err != nil {
		return
	}
	results := make(map[string]map[string]any, len(calls))
	for result := range completed {
		text := string(result.value)
		if result.err != nil {
			text = result.err.Error()
		}
		results[result.call.call.CallID] = map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": text}}, "success": result.err == nil, "agentWorkflowPermit": batch}
	}
	// Reacquire exactly once AFTER every parallel child/broker result settles.
	// Reacquiring after the first result deadlocks a second child at capacity 1.
	if err := c.tools.Lease.Reacquire(c.ctx); err != nil {
		c.fail(err)
		return
	}
	if err := c.authorize(c.ctx); err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || c.batch == nil || c.batch.BatchID != batch.BatchID {
		return
	}
	for _, call := range calls {
		call.result = results[call.call.CallID]
		delete(c.pending, call.call.CallID)
		close(call.ready)
	}
	c.batch, c.running = nil, false
}

func (c *codexManagedBatchController) close() {
	c.mu.Lock()
	if c.err == nil {
		c.err = errors.New("managed controller closed")
	}
	c.cancel(c.err)
	c.mu.Unlock()
	c.wg.Wait()
}
