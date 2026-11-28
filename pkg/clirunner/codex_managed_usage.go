package clirunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type codexModelWitness struct {
	Batch codexSemanticBatch `json:"batch"`
	Usage json.RawMessage    `json:"usage"`
}

// Canonical JSON binds semantic witness bytes across notification/permit
// envelopes without depending on JSON object key ordering. Both observation
// and sealing must use this durable identity.
func codexWitnessDigest(value codexModelWitness) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var canonical any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		return "", err
	}
	raw, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// This recorder owns immutable wire evidence, independently of exposed broker
// tools. Empty-catalog final responses and fresh repairs use the same journal.
// A completion event charges known usage only; it never releases capacity.
type codexManagedAccounting struct {
	mu                                            sync.Mutex
	ctx                                           context.Context
	run, invocation, thread, policy, build, model string
	turn                                          string
	final                                         *workflows.ManagedUsageWitness
	failure                                       error
}

func (a *codexManagedAccounting) observe(value codexModelWitness) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := value.Batch
	if a.failure != nil {
		return a.failure
	}
	if b.ThreadID != a.thread || b.InvocationID != a.invocation || b.PolicyID != a.policy || b.BuildID != a.build || b.ResponseID == "" || len(b.ResponseID) > 256 || b.TurnID == "" || b.BatchID == "" || a.turn != "" && a.turn != b.TurnID || !validCodexNativeUsage(value.Usage) {
		a.failure = fmt.Errorf("managed completed observation differs from original invocation")
		return a.failure
	}
	digest, err := codexWitnessDigest(value)
	if err != nil {
		a.failure = err
		return err
	}
	var usage map[string]any
	decoder := json.NewDecoder(bytes.NewReader(value.Usage))
	decoder.UseNumber()
	if err = decoder.Decode(&usage); err != nil {
		a.failure = err
		return err
	}
	w := workflows.ManagedUsageWitness{ResponseID: b.ResponseID, Digest: digest, SessionID: b.ThreadID, TurnID: b.TurnID, PolicyID: b.PolicyID, BuildID: b.BuildID, Model: a.model, Usage: usage, Batch: mustManagedBatchJSON(b)}
	// Record commits even after cancellation if this original execution still
	// owns the invocation. Authority/deadline checks follow the known charge.
	if err = workflows.RecordManagedCodexUsage(a.ctx, a.run, a.invocation, w); err != nil {
		a.failure = err
		return err
	}
	if len(b.CallIDs) == 0 {
		a.final = &w
	} else {
		a.final = nil
	}
	a.turn = b.TurnID
	return nil
}
func (a *codexManagedAccounting) bindStart(turn string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failure != nil {
		return a.failure
	}
	if turn == "" || a.turn != "" && a.turn != turn {
		a.failure = fmt.Errorf("managed completion preceded a conflicting turn/start reply")
		return a.failure
	}
	a.turn = turn
	return nil
}
func (a *codexManagedAccounting) totals() (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failure != nil {
		return nil, a.failure
	}
	if a.final == nil {
		return nil, fmt.Errorf("managed terminal turn lacks an original no-tool Completed witness")
	}
	return workflows.ManagedCodexUsageTotals(a.ctx, a.run, a.invocation, a.final.ResponseID, a.final.Digest)
}

func mustManagedBatchJSON(batch codexSemanticBatch) json.RawMessage {
	raw, _ := json.Marshal(batch)
	return raw
}

func (a *codexManagedAccounting) seal(batch codexParkedBatch) error {
	if err := a.observe(codexModelWitness{Batch: codexSemanticBatch(batch), Usage: batch.Usage}); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	digest, err := codexWitnessDigest(codexModelWitness{Batch: codexSemanticBatch(batch), Usage: batch.Usage})
	if err != nil {
		return err
	}
	return workflows.SealManagedCodexBatch(a.ctx, a.run, a.invocation, digest, mustManagedBatchJSON(codexSemanticBatch(batch)))
}
func (a *codexManagedAccounting) checkpoint(result workflows.SessionResult) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failure != nil {
		return a.failure
	}
	if a.final == nil {
		return fmt.Errorf("managed terminal has no original no-tool witness")
	}
	return workflows.CheckpointManagedCodexTerminal(a.ctx, result, a.final.ResponseID, a.final.Digest)
}
