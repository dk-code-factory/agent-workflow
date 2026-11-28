package clirunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type sdkBrokerProof struct {
	SessionID  string         `json:"-"`
	ResponseID string         `json:"response_id"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Usage      map[string]any `json:"usage"`
	Calls      []struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"calls"`
}
type sdkBrokerBatch struct {
	mu         sync.Mutex
	totals     map[string]int64
	responseID string
	proof      *sdkBrokerProof
	digest     string
	seen       map[string]string
	plan       map[string]workflows.ToolRequest
	pending    map[string]workflows.ToolRequest
	done       map[string]bool
	active     string
	tools      *workflows.InvocationTools
	observe    func(sdkBrokerProof) error
	authority  func() error
}

func newSDKBrokerBatch(tools *workflows.InvocationTools, observe func(sdkBrokerProof) error, authority func() error) *sdkBrokerBatch {
	return &sdkBrokerBatch{tools: tools, observe: observe, authority: authority, totals: map[string]int64{}, seen: map[string]string{}, pending: map[string]workflows.ToolRequest{}, done: map[string]bool{}}
}
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
func sameSDKRequest(a, b workflows.ToolRequest) bool {
	x, e := canonicalJSON(a.Arguments)
	y, f := canonicalJSON(b.Arguments)
	return e == nil && f == nil && a.CallID == b.CallID && a.Server == b.Server && a.Tool == b.Tool && bytes.Equal(x, y)
}
func decodeSDKProof(data map[string]any) (*sdkBrokerProof, string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, "", err
	}
	var p sdkBrokerProof
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&p); err != nil {
		return nil, "", err
	}
	if p.ResponseID == "" || len(p.ResponseID) > 256 || p.Model == "" || len(p.Calls) > 128 || len(p.Calls) > 0 && p.StopReason != "tool_use" || len(p.Calls) == 0 && p.StopReason != "end_turn" && p.StopReason != "max_tokens" && p.StopReason != "stop_sequence" {
		return nil, "", fmt.Errorf("invalid original SDK response proof")
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		n := p.Usage[key]
		value, ok := n.(json.Number)
		if !ok {
			return nil, "", fmt.Errorf("SDK response usage is unknown")
		}
		i, e := value.Int64()
		if e != nil || i < 0 {
			return nil, "", fmt.Errorf("SDK response usage is invalid")
		}
	}
	sum := sha256.Sum256(raw)
	return &p, hex.EncodeToString(sum[:]), nil
}
func (b *sdkBrokerBatch) start(data map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, _ := data["response_id"].(string)
	if id == "" || len(id) > 256 || b.seen[id] != "" || len(b.seen) >= 4096 || b.active != "" || len(b.pending) > 0 || b.responseID != "" && b.proof == nil {
		return fmt.Errorf("SDK model response overlaps unresolved native work")
	}
	for call := range b.plan {
		if !b.done[call] {
			return fmt.Errorf("SDK model response overlaps original planned callback")
		}
	}
	b.responseID = id
	b.proof = nil
	b.plan = nil
	b.digest = ""
	return nil
}

func (b *sdkBrokerBatch) seal(ctx context.Context, data map[string]any, model, session string) error {
	p, digest, err := decodeSDKProof(data)
	if err != nil {
		return err
	}
	if session == "" {
		return fmt.Errorf("SDK response session identity is missing")
	}
	p.SessionID = session
	if p.Model != model {
		return fmt.Errorf("SDK response model differs from admitted model")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.ResponseID != b.responseID {
		return fmt.Errorf("SDK response seal differs from actual model start")
	}
	if _, ok := b.seen[p.ResponseID]; ok {
		return fmt.Errorf("SDK original response identity repeated")
	}
	if len(b.seen) >= 4096 || b.active != "" {
		return fmt.Errorf("SDK model work overlaps active broker work")
	}
	if b.proof != nil {
		for id := range b.plan {
			if !b.done[id] {
				return fmt.Errorf("SDK model work overlaps unresolved original broker plan")
			}
		}
	}
	plan := map[string]workflows.ToolRequest{}
	for _, call := range p.Calls {
		if call.ID == "" || len(call.ID) > 256 || plan[call.ID].CallID != "" || b.done[call.ID] {
			return fmt.Errorf("SDK original broker identity is invalid or reused")
		}
		var definition *workflows.ToolDefinition
		if b.tools != nil && b.tools.Binding != nil {
			for _, item := range b.tools.Binding.Catalog() {
				if "mcp__broker__"+workflows.ToolWireName(item) == call.Name {
					v := item
					definition = &v
					break
				}
			}
		}
		if definition == nil {
			return fmt.Errorf("SDK original plan exceeds broker catalog")
		}
		plan[call.ID] = workflows.ToolRequest{CallID: call.ID, Server: definition.Server, Tool: definition.Name, Arguments: call.Arguments}
	}
	for name, value := range p.Usage {
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("SDK response counter is not exact")
		}
		n, e := number.Int64()
		if e != nil || n < 0 || n > math.MaxInt64-b.totals[name] {
			return fmt.Errorf("SDK original response totals overflow")
		}
	}
	if b.observe != nil {
		if err = b.observe(*p); err != nil {
			return err
		}
	}
	// A full original response has known charges even if a normalized callback
	// subsequently fails identity validation. Recording them grants no effects.
	for id, request := range b.pending {
		original, ok := plan[id]
		if !ok || !sameSDKRequest(original, request) {
			return fmt.Errorf("SDK callback differs from original response plan")
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	for name, value := range p.Usage {
		n, _ := value.(json.Number).Int64()
		b.totals[name] += n
	}
	b.plan = plan
	b.proof = p
	b.digest = digest
	b.seen[p.ResponseID] = digest
	return nil
}
func (b *sdkBrokerBatch) queue(request workflows.ToolRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if request.CallID == "" || len(request.CallID) > 256 || b.done[request.CallID] {
		return fmt.Errorf("invalid/repeated SDK callback identity")
	}
	if prior, ok := b.pending[request.CallID]; ok {
		if sameSDKRequest(prior, request) {
			return nil
		}
		return fmt.Errorf("conflicting SDK callback identity")
	}
	if len(b.pending) > 0 || b.active != "" {
		return fmt.Errorf("SDK callback violated pinned serial execution")
	}
	if b.proof != nil {
		original, ok := b.plan[request.CallID]
		if !ok || !sameSDKRequest(original, request) {
			return fmt.Errorf("unplanned SDK callback after response seal")
		}
	}
	b.pending[request.CallID] = request
	return nil
}
func (b *sdkBrokerBatch) blocked(ctx context.Context, frame bridgeFrame, writer *bridgeWriter, callbacks *sync.WaitGroup, report func(error)) error {
	p, digest, err := decodeSDKProof(frame.Data)
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.proof == nil || p.ResponseID != b.proof.ResponseID || digest != b.digest || len(frame.CallIDs) != 1 {
		b.mu.Unlock()
		return fmt.Errorf("SDK wait lacks exact completed response proof")
	}
	id := frame.CallIDs[0]
	request, ok := b.pending[id]
	if !ok && b.done[id] {
		b.mu.Unlock()
		return nil
	}
	if !ok || !sameSDKRequest(b.plan[id], request) {
		b.mu.Unlock()
		return fmt.Errorf("SDK wait lacks its original observed callback")
	}
	if b.active == id {
		b.mu.Unlock()
		return nil
	}
	if b.active != "" || len(b.pending) != 1 || b.tools == nil || b.tools.Lease == nil || b.tools.Binding == nil {
		b.mu.Unlock()
		return fmt.Errorf("SDK wait is not a single isolated broker callback")
	}
	if b.authority != nil {
		if err = b.authority(); err != nil {
			b.mu.Unlock()
			return err
		}
	}
	if err = ctx.Err(); err == nil {
		err = b.tools.Lease.Suspend()
	}
	if err != nil {
		b.mu.Unlock()
		return err
	}
	b.active = id
	b.mu.Unlock()
	callbacks.Add(1)
	go func() {
		defer callbacks.Done()
		if b.authority != nil {
			if err := b.authority(); err != nil {
				report(err)
				return
			}
		}
		value, callErr := b.tools.Binding.Call(ctx, request)
		if callErr != nil && !workflows.KnownToolDenial(ctx, b.tools.Binding, request, callErr) {
			report(callErr)
			return
		}
		// The reader remains live while the parent waits for capacity. No mutex
		// held here can hide a late model/native request or conflicting callback.
		if err := b.tools.Lease.Reacquire(ctx); err != nil {
			report(err)
			return
		}
		// Capacity may have been unavailable while another execution acquired
		// the durable owner. A live context alone cannot authorize SDK resume.
		if b.authority != nil {
			if err := b.authority(); err != nil {
				report(err)
				return
			}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if err := ctx.Err(); err != nil {
			report(err)
			return
		}
		response := map[string]any{"type": "tool_result", "call_id": id}
		if callErr != nil {
			response["error"] = callErr.Error()
		} else {
			response["result"] = value
		}
		if err := writer.send(response); err != nil {
			report(err)
			return
		}
		delete(b.pending, id)
		b.done[id] = true
		b.active = ""
	}()
	return nil
}
func (b *sdkBrokerBatch) complete(outputs map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proof == nil || b.active != "" || len(b.pending) > 0 {
		return fmt.Errorf("SDK original response/callback did not complete")
	}
	for id := range b.plan {
		if !b.done[id] {
			return fmt.Errorf("SDK terminal omitted an original planned callback")
		}
	}
	usage, ok := outputs["usage"].(map[string]any)
	if !ok {
		return fmt.Errorf("SDK final accounting is missing")
	}
	// Normalization combines uncached and cache-creation input. Verify the
	// original categories first so an offsetting change cannot hide a different
	// native terminal behind the same normalized total.
	for _, name := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		counter, present := usage[name]
		raw, err := json.Marshal(counter)
		var value int64
		if !present || counter == nil || err != nil || json.Unmarshal(raw, &value) != nil || value < 0 || value != b.totals[name] {
			return fmt.Errorf("SDK final %s differs from original response accounting", name)
		}
	}
	expected := workflows.NormalizeUsage(workflows.UsageUpdate{Provider: "claude-sdk", Usage: map[string]any{"input_tokens": b.totals["input_tokens"], "output_tokens": b.totals["output_tokens"], "cache_read_input_tokens": b.totals["cache_read_input_tokens"], "cache_creation_input_tokens": b.totals["cache_creation_input_tokens"]}})
	actual := workflows.NormalizeUsage(workflows.UsageUpdate{Provider: "claude-sdk", Usage: usage})
	left, _ := json.Marshal([]any{expected.InputTokens, expected.CachedInputTokens, expected.OutputTokens})
	right, _ := json.Marshal([]any{actual.InputTokens, actual.CachedInputTokens, actual.OutputTokens})
	if expected.InputTokens == nil || expected.OutputTokens == nil || !bytes.Equal(left, right) {
		return fmt.Errorf("SDK final aggregate differs from original response accounting")
	}
	return nil
}
