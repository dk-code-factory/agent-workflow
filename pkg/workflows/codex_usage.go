package workflows

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

// ManagedUsageWitness is adapter-validated native evidence. Ownership comes
// exclusively from the existing engine context, including tool-free repairs.
type ManagedUsageWitness struct {
	ResponseID, Digest, SessionID, TurnID, PolicyID, BuildID, Model string
	Usage                                                           map[string]any
	Batch                                                           json.RawMessage
}

type managedCodexUsageProof struct {
	ResponseID string `json:"response_id"`
	Digest     string `json:"witness_sha256"`
	SessionID  string `json:"session_id"`
	TurnID     string `json:"turn_id"`
	PolicyID   string `json:"policy_id"`
	BuildID    string `json:"build_id"`
	Model      string `json:"model"`
}
type managedCodexUsageRecord struct {
	UsageObservation
	Proof *managedCodexUsageProof `json:"managed_codex_witness,omitempty"`
	Batch json.RawMessage         `json:"managed_codex_batch,omitempty"`
}

func managedCodexUsageOwner(ctx context.Context, run, invocation string) (*SQLiteStore, runtimeexec.Binding, error) {
	binding, ok := runtimeexec.BindingFrom(ctx)
	if !ok || run == "" || invocation == "" || binding.RunID != run || binding.LogicalID != invocation || binding.Owner == "" || binding.Generation <= 0 {
		return nil, binding, fmt.Errorf("managed usage lacks its original engine invocation context")
	}
	journal := binding.Journal
	if worker, ok := journal.(workerInvocationJournal); ok {
		journal = worker.Journal
	}
	store, ok := journal.(*SQLiteStore)
	if !ok || store == nil {
		return nil, binding, fmt.Errorf("managed usage requires the engine-owned durable journal")
	}
	return store, binding, nil
}

func managedCodexUsageFence(ctx context.Context, tx *sql.Tx, binding runtimeexec.Binding) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs r WHERE r.id=? AND r.owner=? AND r.generation=? AND r.status='running' AND EXISTS(SELECT 1 FROM executions e WHERE e.run_id=r.id AND e.generation=r.generation AND e.finalizing=0)`, binding.RunID, binding.Owner, binding.Generation).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("managed usage execution ownership changed")
	}
	return nil
}

func managedCodexUsageIdentity(ctx context.Context, tx *sql.Tx, run, invocation string) (UsageIdentity, error) {
	return managedProviderUsageIdentity(ctx, tx, run, invocation, "codex-app-server")
}

func managedProviderUsageIdentity(ctx context.Context, tx *sql.Tx, run, invocation, provider string) (UsageIdentity, error) {
	var identity UsageIdentity
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT record FROM usage_invocations WHERE id=? AND origin_id=?`, invocation, run).Scan(&raw); err != nil {
		return identity, err
	}
	if err := decodeJSONWithNumbers(raw, &identity); err != nil {
		return identity, err
	}
	if identity.ID != invocation || identity.RunID != run || identity.Provider != provider {
		return identity, fmt.Errorf("managed usage identity differs from original provider invocation")
	}
	return identity, nil
}

// RecordManagedCodexUsage commits a flat original-response observation before
// effects. Identical recovery observations are idempotent across processes.
// Proof metadata lives only in these existing JSON rows; legacy accounting and
// public request/capability structures remain unchanged.
func RecordManagedCodexUsage(ctx context.Context, run, invocation string, witness ManagedUsageWitness) error {
	store, binding, err := managedCodexUsageOwner(ctx, run, invocation)
	if err != nil {
		return err
	}
	if witness.ResponseID == "" || len(witness.ResponseID) > 256 || len(witness.Digest) != 64 || witness.SessionID == "" || witness.TurnID == "" || witness.PolicyID == "" || witness.BuildID == "" || witness.Model == "" {
		return fmt.Errorf("managed usage witness identity is incomplete")
	}
	if decoded, err := hex.DecodeString(witness.Digest); err != nil || len(decoded) != 32 {
		return fmt.Errorf("managed usage witness digest is invalid")
	}
	if _, nested := witness.Usage["total"]; nested {
		return fmt.Errorf("managed original usage cannot be a cumulative session counter")
	}
	if _, nested := witness.Usage["last"]; nested {
		return fmt.Errorf("managed original usage must be flat")
	}
	for _, name := range []string{"input_tokens", "cached_input_tokens", "output_tokens", "total_tokens"} {
		if tokenValue(witness.Usage[name]) == nil {
			return fmt.Errorf("managed usage witness counters are unknown")
		}
	}
	input, output, total := tokenValue(witness.Usage["input_tokens"]), tokenValue(witness.Usage["output_tokens"]), tokenValue(witness.Usage["total_tokens"])
	if *input > math.MaxInt64-*output || *input+*output != *total || *tokenValue(witness.Usage["cached_input_tokens"]) > *input {
		return fmt.Errorf("managed usage witness counters are inconsistent")
	}
	batch, err := decodeManagedCodexBatch(witness.Batch)
	if err != nil || batch.InvocationID != invocation || batch.ThreadID != witness.SessionID || batch.TurnID != witness.TurnID || batch.ResponseID != witness.ResponseID || batch.PolicyID != witness.PolicyID || batch.BuildID != witness.BuildID {
		return fmt.Errorf("managed usage original native batch differs")
	}
	proof := managedCodexUsageProof{witness.ResponseID, witness.Digest, witness.SessionID, witness.TurnID, witness.PolicyID, witness.BuildID, witness.Model}
	// A completed native request's known charge survives cancellation. Current
	// execution fencing is still checked inside this bounded transaction.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(commitCtx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(commitCtx, tx, binding); err != nil {
		return err
	}
	identity, err := managedCodexUsageIdentity(commitCtx, tx, run, invocation)
	if err != nil {
		return err
	}
	if identity.Model != "unknown" && identity.Model != witness.Model || identity.SessionID != "" && identity.SessionID != witness.SessionID {
		return fmt.Errorf("managed usage model/session differs from original invocation")
	}
	key := "codex-response:" + witness.ResponseID
	var priorRaw []byte
	err = tx.QueryRowContext(commitCtx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, invocation, key).Scan(&priorRaw)
	if err == nil {
		var prior managedCodexUsageRecord
		if err = decodeJSONWithNumbers(priorRaw, &prior); err != nil {
			return err
		}
		incoming := NormalizeUsage(UsageUpdate{Usage: witness.Usage, Provider: "codex-app-server"})
		left, _ := json.Marshal([]any{prior.InputTokens, prior.CachedInputTokens, prior.OutputTokens})
		right, _ := json.Marshal([]any{incoming.InputTokens, incoming.CachedInputTokens, incoming.OutputTokens})
		if prior.Proof == nil || *prior.Proof != proof || !bytes.Equal(prior.Batch, witness.Batch) || string(left) != string(right) {
			return fmt.Errorf("managed original response witness conflicts with durable accounting")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var count int
	if err = tx.QueryRowContext(commitCtx, `SELECT count(*) FROM usage_observations WHERE invocation_id=?`, invocation).Scan(&count); err != nil {
		return err
	}
	if count >= 4096 {
		return fmt.Errorf("managed usage exceeds response bound")
	}
	if identity.Finished {
		return fmt.Errorf("managed response cannot extend a finished invocation")
	}
	update := UsageUpdate{Key: key, Provider: "codex-app-server", Model: witness.Model, SessionID: witness.SessionID, Usage: witness.Usage, Incomplete: true}
	if err = observeUsageTx(commitCtx, tx, identity, update); err != nil {
		return err
	}
	var raw []byte
	if err = tx.QueryRowContext(commitCtx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, invocation, key).Scan(&raw); err != nil {
		return err
	}
	var record managedCodexUsageRecord
	if err = decodeJSONWithNumbers(raw, &record); err != nil {
		return err
	}
	record.Proof = &proof
	record.Batch = append(json.RawMessage(nil), witness.Batch...)
	raw, err = json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(commitCtx, `UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key=?`, raw, invocation, key); err != nil {
		return err
	}
	return tx.Commit()
}

// ManagedCodexUsageTotals includes all durable original response rows, including
// earlier processes. It is used only after the adapter proves terminal success
// and observes the final no-tool response; it never creates a new allowance.
func ManagedCodexUsageTotals(ctx context.Context, run, invocation, finalResponse, finalDigest string) (map[string]any, error) {
	store, binding, err := managedCodexUsageOwner(ctx, run, invocation)
	if err != nil {
		return nil, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(ctx, tx, binding); err != nil {
		return nil, err
	}
	identity, err := managedCodexUsageIdentity(ctx, tx, run, invocation)
	if err != nil {
		return nil, err
	}
	_, bookkeeping, err := readManagedCodexAdmission(ctx, tx, invocation)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT observation_key,record FROM usage_observations WHERE invocation_id=? ORDER BY observation_key`, invocation)
	if err != nil {
		return nil, err
	}
	totals := map[string]int64{"input_tokens": 0, "cached_input_tokens": 0, "output_tokens": 0}
	found := false
	count := 0
	var original *managedCodexUsageProof
	for rows.Next() {
		var key string
		var raw []byte
		if err = rows.Scan(&key, &raw); err != nil {
			break
		}
		if key == managedCodexAdmissionKey {
			continue
		}
		if !strings.HasPrefix(key, "codex-response:") {
			err = fmt.Errorf("managed invocation contains an unproven or incomplete observation")
			break
		}
		var record managedCodexUsageRecord
		if err = decodeJSONWithNumbers(raw, &record); err != nil {
			break
		}
		if record.Proof == nil || key != "codex-response:"+record.Proof.ResponseID || record.Scope != "invocation" || record.CounterReset {
			err = fmt.Errorf("managed usage original response proof is missing or uncertain")
			break
		}
		proof := record.Proof
		if original == nil {
			original = proof
		}
		if proof.SessionID != identity.SessionID || proof.Model != identity.Model || proof.SessionID != original.SessionID || proof.TurnID != original.TurnID || proof.PolicyID != original.PolicyID || proof.BuildID != original.BuildID || proof.Model != original.Model {
			err = fmt.Errorf("managed usage proof scope differs from original invocation")
			break
		}
		if record.Proof.ResponseID == finalResponse && record.Proof.Digest == finalDigest {
			found = true
		}
		for name, value := range map[string]*int64{"input_tokens": record.InputTokens, "cached_input_tokens": record.CachedInputTokens, "output_tokens": record.OutputTokens} {
			if value == nil || *value < 0 || *value > math.MaxInt64-totals[name] {
				err = fmt.Errorf("managed usage total is unknown or overflows")
				break
			}
			totals[name] += *value
		}
		if err != nil {
			break
		}
		count++
		if count > 4096 {
			err = fmt.Errorf("managed usage exceeds response bound")
			break
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	if !found {
		return nil, fmt.Errorf("managed final original response has no durable accounting proof")
	}
	if err = validateManagedCodexBookkeeping(bookkeeping, run, invocation, original); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"input_tokens": totals["input_tokens"], "cached_input_tokens": totals["cached_input_tokens"], "output_tokens": totals["output_tokens"]}, nil
}

// CheckManagedCodexAuthority checks the existing admission after committing a
// known charge and immediately before effects. It neither consumes another
// allowance nor introduces a token/cost limit. The active context carries the
// admitted deadline; worker messages must remain the same running, previously
// admitted turn (Remaining may correctly be zero after its last admission).
func CheckManagedCodexAuthority(ctx context.Context, run, invocation string) error {
	return checkManagedProviderAuthority(ctx, run, invocation, "codex-app-server")
}

func checkManagedProviderAuthority(ctx context.Context, run, invocation, provider string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, binding, err := managedCodexUsageOwner(ctx, run, invocation)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(ctx, tx, binding); err != nil {
		return err
	}
	if err = checkEffectAdmission(ctx, tx, run); err != nil {
		return err
	}
	identity, err := managedProviderUsageIdentity(ctx, tx, run, invocation, provider)
	if err != nil {
		return err
	}
	if identity.Finished {
		return fmt.Errorf("managed original invocation has already finished")
	}
	if binding.Kind == "worker" {
		var workerRaw, messageRaw []byte
		if err = tx.QueryRowContext(ctx, `SELECT w.record,m.record FROM workers w JOIN messages m ON m.worker_id=w.id WHERE w.id=? AND w.run_id=? AND m.id=? AND w.status='running' AND m.status='running'`, identity.WorkerID, run, invocation).Scan(&workerRaw, &messageRaw); err != nil {
			return err
		}
		var worker Worker
		var message WorkerMessage
		if err = decodeJSONWithNumbers(workerRaw, &worker); err != nil {
			return err
		}
		if err = decodeJSONWithNumbers(messageRaw, &message); err != nil {
			return err
		}
		if worker.ID != identity.WorkerID || worker.RunID != run || worker.Status != RunStatusRunning || worker.Remaining < 0 || message.ID != invocation || message.WorkerID != worker.ID || message.Generation != binding.Generation || message.Status != RunStatusRunning || message.StartedAt.IsZero() {
			return fmt.Errorf("managed original worker admission changed")
		}
		if worker.TurnDuration > 0 && !time.Now().Before(message.StartedAt.Add(worker.TurnDuration)) {
			return fmt.Errorf("managed original worker deadline expired")
		}
	}
	return tx.Commit()
}
