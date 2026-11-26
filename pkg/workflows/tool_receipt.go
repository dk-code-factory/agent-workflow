package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

// CheckToolBindingAuthority validates the original engine invocation and its
// current execution fence. It grants no tools and cannot bless an arbitrary
// implementation of ToolBinding as an engine-owned broker.
func engineToolBinding(binding ToolBinding) (*toolBinding, bool) {
	// The worker wrapper adds durable failure observation without changing the
	// original broker identity. No provider-supplied unwrap interface is trusted.
	if wrapper, ok := binding.(workerToolBinding); ok {
		binding = wrapper.ToolBinding
	}
	actual, ok := binding.(*toolBinding)
	return actual, ok && actual != nil
}

func CheckToolBindingAuthority(ctx context.Context, binding ToolBinding, runID, invocationID string) error {
	if conversation, ok := binding.(*conversationTools); ok {
		store, owner, err := managedCodexUsageOwner(ctx, runID, invocationID)
		if err != nil {
			return err
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = managedCodexUsageFence(ctx, tx, owner); err != nil {
			return err
		}
		a, err := managedCaptureTaskAdmission(ctx, tx, owner, invocationID)
		if err != nil {
			return err
		}
		if a.Task.ParentID != conversation.execution.attempt.ID || conversation.turn != a.Task.Attempt.StepID || conversation.execution.state.Active == nil || conversation.execution.state.Active.To != conversation.role {
			return fmt.Errorf("conversation binding differs from original active turn")
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if conversation.base != nil {
			return CheckToolBindingAuthority(ctx, conversation.base, runID, invocationID)
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	actual, ok := engineToolBinding(binding)
	if !ok || runID == "" || invocationID == "" || actual.invocation.RunID != runID || actual.invocation.InvocationID != invocationID {
		return fmt.Errorf("tool binding differs from original engine invocation")
	}
	return actual.broker.checkOwner(ctx, actual.invocation)
}

// ToolReceiptError identifies a denial committed before dispatch. Its private
// proof is checked against the current owner and original durable broker row;
// the error text or a terminal status alone never authorizes model continuation.
// Engine dispatch errors are deliberately excluded: a child may already exist.
type ToolReceiptError struct {
	cause                                    error
	text, run, invocation, call, fingerprint string
	owner                                    string
	generation                               int64
}

func (e *ToolReceiptError) Error() string { return e.text }
func (e *ToolReceiptError) Unwrap() error { return e.cause }

func deniedToolReceipt(in ToolInvocation, record ToolCallRecord, cause error) error {
	return &ToolReceiptError{cause: cause, text: record.Error, run: in.RunID, invocation: in.InvocationID, call: record.CallID, fingerprint: record.ArgumentFingerprint, owner: in.Owner, generation: in.Generation}
}

func toolArgumentFingerprint(req ToolRequest) string {
	var args any
	if decodeJSONWithNumbers(req.Arguments, &args) != nil {
		args = string(req.Arguments)
	}
	canonical, _ := json.Marshal(args)
	digest := sha256.Sum256(append([]byte(req.Server+"\x00"+req.Tool+"\x00"), canonical...))
	return hex.EncodeToString(digest[:])
}

// KnownToolDenial permits an ordinary failed tool response only for an exact,
// currently owned, committed denial. Uncertain effects, storage failures,
// cancellation, integrity failures and all unclassified engine failures hold.
func KnownToolDenial(ctx context.Context, binding ToolBinding, req ToolRequest, failure error) bool {
	if failure == nil || ctx.Err() != nil || errors.Is(failure, ErrUncertainTool) || errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) || errors.Is(failure, ErrActionIntegrity) {
		return false
	}
	var storage *runtimeexec.StorageError
	var proof *ToolReceiptError
	if errors.As(failure, &storage) || !errors.As(failure, &proof) || proof == nil {
		return false
	}
	actual, ok := engineToolBinding(binding)
	if !ok {
		return false
	}
	in := actual.invocation
	if proof.run != in.RunID || proof.invocation != in.InvocationID || proof.owner != in.Owner || proof.generation != in.Generation || proof.call != req.CallID || proof.fingerprint != toolArgumentFingerprint(req) {
		return false
	}
	if actual.broker.checkOwner(ctx, in) != nil {
		return false
	}
	var raw []byte
	err := actual.broker.Store.db.QueryRowContext(ctx, `SELECT c.record FROM tool_calls c JOIN runs r ON r.id=c.run_id WHERE c.run_id=? AND c.invocation_id=? AND c.call_id=? AND c.status='denied' AND c.fingerprint=? AND r.generation=? AND r.owner=? AND r.status='running' AND EXISTS(SELECT 1 FROM executions e WHERE e.run_id=r.id AND e.generation=r.generation AND e.finalizing=0)`, in.RunID, in.InvocationID, req.CallID, proof.fingerprint, in.Generation, in.Owner).Scan(&raw)
	if err != nil {
		return false
	}
	var record ToolCallRecord
	if decodeJSONWithNumbers(raw, &record) != nil {
		return false
	}
	return record.Status == "denied" && record.RunID == in.RunID && record.InvocationID == in.InvocationID && record.CallID == req.CallID && record.Server == req.Server && record.Tool == req.Tool && record.ArgumentFingerprint == proof.fingerprint && record.Error == proof.text
}
