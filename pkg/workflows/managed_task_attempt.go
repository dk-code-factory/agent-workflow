package workflows

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

// Only the existing conversation controller installs this tag. Public agent
// tasks retain their WorkerManager admission; this is no new execution loop.
type managedTaskAdmission struct {
	Attempt                     Attempt   `json:"attempt"`
	ParentID                    string    `json:"parent_id"`
	ConversationSHA256          string    `json:"conversation_sha256"`
	PersistedConversationSHA256 string    `json:"persisted_conversation_sha256"`
	Deadline                    time.Time `json:"deadline"`
	parent                      *Attempt
}
type managedTaskKey struct{}

func managedConversationDigest(a Attempt) string {
	c := a.Conversation
	if c == nil {
		return ""
	}
	return actionHash([]any{a.ID, a.RunID, a.JobID, a.StepID, a.Iteration, a.Number, a.StartedAt, a.InputFingerprint, c.Active, c.Calls, c.MaxCalls, c.MaxMessages, c.Prompt, c.Roles, c.FinishBy, c.Deadline})
}
func withManagedConversationAttempt(ctx context.Context, c *conversationExecution, turn Attempt, secrets map[string]string) context.Context {
	tag := managedTaskAdmission{
		Attempt: turn, ParentID: c.attempt.ID, Deadline: c.state.Deadline,
		ConversationSHA256:          managedConversationDigest(*c.attempt),
		PersistedConversationSHA256: managedConversationDigest(redactAttempt(*c.attempt, secrets)),
		parent:                      c.attempt,
	}
	ctx = context.WithValue(ctx, managedTaskKey{}, tag)
	ctx = context.WithValue(ctx, managedSDKTerminalKey{}, managedSDKTerminalRecorder(func(tc context.Context, result SessionResult) error {
		return recordManagedTaskTerminal(tc, tag, result, "", "", secrets)
	}))
	return context.WithValue(ctx, managedCodexTerminalKey{}, managedCodexTerminalRecorder(func(tc context.Context, result SessionResult, response, digest string) error {
		return recordManagedTaskTerminal(tc, tag, result, response, digest, secrets)
	}))
}
func managedTaskSnapshot(ctx context.Context, tx *sql.Tx, binding runtimeexec.Binding, inv string, tag managedTaskAdmission) (*managedCodexAdmission, error) {
	if tag.Attempt.ID != inv || binding.Kind != "" || tag.ParentID == "" || tag.ConversationSHA256 == "" || tag.PersistedConversationSHA256 == "" || tag.Deadline.IsZero() || !time.Now().Before(tag.Deadline) {
		return nil, fmt.Errorf("managed original conversation admission is missing or expired")
	}
	// Compare live authority before redaction as well as its persisted form. Two
	// different secrets may redact to the same bytes, but cannot borrow a tag.
	live, ok := ctx.Value(managedTaskKey{}).(managedTaskAdmission)
	leftTag, _ := json.Marshal(live)
	rightTag, _ := json.Marshal(tag)
	if !ok || live.parent == nil || !bytes.Equal(leftTag, rightTag) || managedConversationDigest(*live.parent) != tag.ConversationSHA256 {
		return nil, fmt.Errorf("managed live conversation authority changed")
	}
	var origin string
	if err := tx.QueryRowContext(ctx, `SELECT origin_id FROM runs WHERE id=?`, tag.Attempt.RunID).Scan(&origin); err != nil {
		return nil, err
	}
	if tag.Attempt.RunID != binding.RunID && origin != binding.RunID {
		return nil, fmt.Errorf("managed task belongs to another origin execution")
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT record FROM attempts WHERE id=? AND run_id=? AND status='running'`, inv, tag.Attempt.RunID).Scan(&raw); err != nil {
		return nil, err
	}
	var original Attempt
	if err := decodeJSONWithNumbers(raw, &original); err != nil {
		return nil, err
	}
	// The whole original turn, including input fingerprint and StartedAt, is
	// immutable until the existing Journal.Complete transaction.
	left, _ := json.Marshal(original)
	right, _ := json.Marshal(tag.Attempt)
	if !bytes.Equal(left, right) || original.StartedAt.IsZero() {
		return nil, fmt.Errorf("managed original conversation turn changed")
	}
	if err := tx.QueryRowContext(ctx, `SELECT record FROM attempts WHERE id=? AND run_id=? AND status='running'`, tag.ParentID, tag.Attempt.RunID).Scan(&raw); err != nil {
		return nil, err
	}
	var parent Attempt
	if err := decodeJSONWithNumbers(raw, &parent); err != nil {
		return nil, err
	}
	c := parent.Conversation
	if c == nil || c.Active == nil || c.Calls < 1 || c.Calls > c.MaxCalls || managedConversationDigest(parent) != tag.PersistedConversationSHA256 || !c.Deadline.Equal(tag.Deadline) {
		return nil, fmt.Errorf("managed persisted conversation authority changed")
	}
	return &managedCodexAdmission{Task: &tag, MessageID: inv, StartedAt: original.StartedAt, BoundarySHA256: actionHash(tag)}, nil
}
func managedCaptureTaskAdmission(ctx context.Context, tx *sql.Tx, binding runtimeexec.Binding, inv string) (*managedCodexAdmission, error) {
	tag, ok := ctx.Value(managedTaskKey{}).(managedTaskAdmission)
	if !ok {
		return nil, fmt.Errorf("managed nonworker lacks its original conversation controller")
	}
	return managedTaskSnapshot(ctx, tx, binding, inv, tag)
}
func sameManagedTaskAdmission(left, right *managedCodexAdmission) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return left != nil && left.Task != nil && bytes.Equal(a, b)
}
func managedTaskHasMarker(ctx context.Context, s *SQLiteStore, inv string) bool {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE invocation_id=? AND observation_key IN (?,?)`, inv, managedSDKAttemptKey, managedCodexAdmissionKey).Scan(&count)
	// A storage failure must never cause a second final accounting path.
	return err != nil || count != 0
}
func recordManagedTaskTerminal(ctx context.Context, tag managedTaskAdmission, result SessionResult, response, digest string, secrets map[string]string) error {
	binding, ok := runtimeexec.BindingFrom(ctx)
	if !ok {
		return fmt.Errorf("managed conversation terminal lacks original execution")
	}
	run, inv := binding.RunID, tag.Attempt.ID
	provider := "claude-sdk"
	if response != "" {
		provider = "codex-app-server"
	}
	if err := checkManagedProviderAuthority(ctx, run, inv, provider); err != nil {
		return err
	}
	if result.Status != RunStatusSucceeded || result.SessionID == "" || !NormalizeUsage(usageUpdate(result.Outputs, true)).Complete {
		return fmt.Errorf("managed conversation terminal/accounting is incomplete")
	}
	if provider == "codex-app-server" {
		if result.ProviderStatus != "completed" || result.Outputs["usage_incomplete"] != false {
			return fmt.Errorf("managed conversation lacks native completion")
		}
		totals, err := ManagedCodexUsageTotals(ctx, run, inv, response, digest)
		if err != nil {
			return err
		}
		expected := NormalizeUsage(UsageUpdate{Usage: totals})
		actual := NormalizeUsage(usageUpdate(result.Outputs, true))
		a, _ := json.Marshal([]any{expected.InputTokens, expected.CachedInputTokens, expected.OutputTokens})
		b, _ := json.Marshal([]any{actual.InputTokens, actual.CachedInputTokens, actual.OutputTokens})
		if !bytes.Equal(a, b) {
			return fmt.Errorf("managed conversation total differs from original responses")
		}
	} else if result.ProviderStatus != "success" || result.ProviderVersion != "claude-agent-sdk@0.3.322" {
		return fmt.Errorf("SDK conversation lacks pinned native success")
	}
	result.Outputs, _ = redactValue(result.Outputs, secrets).(map[string]any)
	// This is the existing maximum managed output bound, not a new allowance.
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(encoded) > DefaultWorkerLimits().MaxOutput {
		return fmt.Errorf("managed conversation terminal exceeds output limit")
	}
	resultDigest, err := managedResultDigest(result)
	if err != nil {
		return err
	}
	store, binding, err := managedCodexUsageOwner(ctx, run, inv)
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
	current, err := managedCaptureTaskAdmission(ctx, tx, binding, inv)
	if err != nil {
		return err
	}
	if provider == "claude-sdk" {
		fields, a, e := readManagedSDKAttempt(ctx, tx, inv)
		if e != nil {
			return e
		}
		if a.State != "active" || a.RunID != run || a.InvocationID != inv || a.Generation != binding.Generation || a.Owner != binding.Owner || !sameManagedTaskAdmission(a.Admission, current) {
			return fmt.Errorf("SDK conversation admission changed")
		}
		if err = validateManagedSDKResponseTotals(ctx, tx, inv, result); err != nil {
			return err
		}
		a.Terminal = &result
		a.State = "terminal"
		a.SessionID = result.SessionID
		a.TerminalSHA256 = resultDigest
		if err = putManagedSDKAttempt(ctx, tx, inv, fields, a); err != nil {
			return err
		}
	} else {
		key := "codex-response:" + response
		var raw []byte
		if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, inv, key).Scan(&raw); err != nil {
			return err
		}
		var row managedCodexUsageRecord
		if err = decodeJSONWithNumbers(raw, &row); err != nil {
			return err
		}
		batch, e := decodeManagedCodexBatch(row.Batch)
		if e != nil {
			return e
		}
		if len(batch.CallIDs) != 0 || batch.InvocationID != inv || batch.ThreadID != result.SessionID || !strings.Contains(result.ProviderVersion, "agent-workflow-managed-v1/"+batch.BuildID) {
			return fmt.Errorf("managed conversation final response identity changed")
		}
		if err = managedRecoveryProof(raw, batch, digest); err != nil {
			return err
		}
		fields, prior, e := managedCodexRecoveryRow(raw)
		if e != nil {
			return e
		}
		if prior != nil {
			return fmt.Errorf("managed conversation terminal already checkpointed")
		}
		value := &managedCodexRecovery{Admission: current, Batch: row.Batch, WitnessSHA256: digest, Generation: binding.Generation, Owner: binding.Owner, State: "terminal", Terminal: &result, TerminalSHA256: resultDigest}
		if err = settleManagedCodexRows(ctx, tx, inv, key); err != nil {
			return err
		}
		if err = putManagedCodexRecovery(ctx, tx, inv, key, fields, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Unknown nonworker native attempts remain held. No retry flag or changed
// logical/physical run may silently grant another provider attempt.
const managedTaskCoordinateOriginQuery = `SELECT CASE WHEN origin_id='' THEN id ELSE origin_id END FROM runs r WHERE r.id=? OR r.origin_id=?`
const managedTaskCoordinateLogicalQuery = managedTaskCoordinateOriginQuery + ` OR (?!='' AND json_extract(CAST(r.snapshot AS TEXT),'$.metadata.iteration_logical_run_id')=?)`
const managedTaskCoordinatePrefix = `WITH origins AS MATERIALIZED (`
const managedTaskCoordinateScope = `), requested AS (SELECT ? AS sdk,? AS codex,? AS job,? AS step,? AS iteration),
 scoped_runs AS MATERIALIZED (
 SELECT id FROM runs WHERE id IN (SELECT * FROM origins)
 UNION SELECT id FROM runs INDEXED BY runs_origin WHERE origin_id IN (SELECT * FROM origins)),
 coordinates AS MATERIALIZED (
 SELECT a.id,a.run_id FROM scoped_runs selected CROSS JOIN requested p CROSS JOIN attempts a
 WHERE a.run_id=selected.id AND a.job_id=p.job AND a.step_id=p.step AND a.iteration=p.iteration)
 SELECT u.observation_key,u.record,v.record,(SELECT status FROM attempts WHERE id=v.id),a.run_id
 FROM coordinates a CROSS JOIN usage_invocations v CROSS JOIN usage_observations u CROSS JOIN requested p
 WHERE v.id=a.id AND v.origin_id IN (SELECT * FROM origins) AND u.invocation_id=v.id AND u.observation_key IN (p.sdk,p.codex)
 UNION
 SELECT u.observation_key,u.record,v.record,(SELECT status FROM attempts WHERE id=v.id),a.run_id
 FROM coordinates a CROSS JOIN usage_observations u INDEXED BY usage_managed_sdk_parent CROSS JOIN usage_invocations v
 WHERE CAST(json_extract(CAST(u.record AS TEXT),'$.managed_sdk_attempt.original_admission.task.parent_id') AS TEXT)=a.id
 AND u.observation_key IN ('managed-sdk-attempt','codex-managed-admission') AND v.id=u.invocation_id AND v.origin_id IN (SELECT * FROM origins)
 UNION
 SELECT u.observation_key,u.record,v.record,(SELECT status FROM attempts WHERE id=v.id),a.run_id
 FROM coordinates a CROSS JOIN usage_observations u INDEXED BY usage_managed_codex_parent CROSS JOIN usage_invocations v
 WHERE CAST(json_extract(CAST(u.record AS TEXT),'$.managed_codex_admission.admission.task.parent_id') AS TEXT)=a.id
 AND u.observation_key IN ('managed-sdk-attempt','codex-managed-admission') AND v.id=u.invocation_id AND v.origin_id IN (SELECT * FROM origins)`
const managedTaskCoordinateQuery = managedTaskCoordinatePrefix + managedTaskCoordinateOriginQuery + managedTaskCoordinateScope
const managedTaskLogicalCoordinateQuery = managedTaskCoordinatePrefix + managedTaskCoordinateLogicalQuery + managedTaskCoordinateScope

func managedTaskCoordinateHold(ctx context.Context, tx *sql.Tx, run, job, step string, iteration int, logical string) error {
	origin := run
	if err := tx.QueryRowContext(ctx, `SELECT origin_id FROM runs WHERE id=?`, run).Scan(&origin); err != nil {
		return err
	}
	if origin == "" {
		origin = run
	}
	query := managedTaskCoordinateQuery
	args := []any{run, origin}
	if logical != "" {
		query = managedTaskLogicalCoordinateQuery
		args = append(args, logical, logical)
	}
	args = append(args, managedSDKAttemptKey, managedCodexAdmissionKey, job, step, iteration)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw, identityRaw []byte
		var status, originalRun string
		if err = rows.Scan(&key, &raw, &identityRaw, &status, &originalRun); err != nil {
			return err
		}
		var settled bool
		if key == managedCodexAdmissionKey {
			_, a, e := parseManagedCodexAdmission(raw)
			if e != nil {
				return e
			}
			settled = a.Admission != nil && a.Admission.Task != nil && a.State == "completed"
		} else {
			var row managedSDKAttemptRow
			if err = decodeJSONWithNumbers(raw, &row); err != nil {
				return err
			}
			settled = row.Attempt != nil && row.Attempt.Admission != nil && row.Attempt.Admission.Task != nil && row.Attempt.State == "settled"
		}
		var identity UsageIdentity
		if err = decodeJSONWithNumbers(identityRaw, &identity); err != nil {
			return err
		}
		if !settled || !identity.Finished || status != RunStatusSucceeded {
			independent, e := managedIndependentReusableTasks(ctx, tx, run, originalRun, origin)
			if e != nil {
				return e
			}
			if independent {
				continue
			}
			return fmt.Errorf("%w: original managed conversation attempt requires reconciliation before replay", ErrActionIntegrity)
		}
	}
	return rows.Err()
}

// Only engine-admitted reusable edges can distinguish sibling job lineages.
// Older snapshots without an edge remain conservative; caller metadata cannot
// label an unresolved original as an independent execution.
func managedReusableLineage(ctx context.Context, tx *sql.Tx, run, origin string) ([]string, string, error) {
	var path []string
	var logical string
	for depth := 0; run != origin && depth < maxReusableWorkflowDepth; depth++ {
		var raw, snapshot []byte
		var actualOrigin string
		if err := tx.QueryRowContext(ctx, `SELECT record,snapshot,origin_id FROM runs WHERE id=?`, run).Scan(&raw, &snapshot, &actualOrigin); err != nil {
			return nil, "", err
		}
		var child Run
		var snap Snapshot
		if err := decodeJSONWithNumbers(raw, &child); err != nil {
			return nil, "", err
		}
		if err := decodeJSONWithNumbers(snapshot, &snap); err != nil {
			return nil, "", err
		}
		if depth == 0 {
			logical = nativeString(snap.Metadata, "iteration_logical_run_id")
		}
		edge := snap.ReusableInvocation
		if child.ID != run || actualOrigin != origin || edge == nil {
			return nil, logical, nil
		}
		if edge.ParentRunID == "" || edge.JobID == "" || edge.Iteration < 1 || run != reusableChildID(edge.ParentRunID, edge.JobID, edge.Iteration) {
			return nil, "", fmt.Errorf("%w: reusable execution identity changed", ErrActionIntegrity)
		}
		var parent Run
		var parentSnapshot Snapshot
		if err := tx.QueryRowContext(ctx, `SELECT record,snapshot FROM runs WHERE id=?`, edge.ParentRunID).Scan(&raw, &snapshot); err != nil {
			return nil, "", err
		}
		if err := decodeJSONWithNumbers(raw, &parent); err != nil {
			return nil, "", err
		}
		if err := decodeJSONWithNumbers(snapshot, &parentSnapshot); err != nil {
			return nil, "", err
		}
		definition := parentSnapshot.Definitions[parent.WorkflowRef]
		if parent.ID != edge.ParentRunID || definition == nil {
			return nil, "", fmt.Errorf("%w: reusable parent definition missing", ErrActionIntegrity)
		}
		job, exists := definition.Jobs[edge.JobID]
		ref, err := CanonicalLocalRef(job.Uses)
		if !exists || err != nil || ref != child.WorkflowRef {
			return nil, "", fmt.Errorf("%w: reusable parent job changed", ErrActionIntegrity)
		}
		// Physical iterations of the same wrapper job share its replay hold.
		path = append(path, edge.JobID)
		run = edge.ParentRunID
	}
	if run != origin {
		return nil, "", fmt.Errorf("%w: reusable ancestry exceeds admitted bound", ErrActionIntegrity)
	}
	return path, logical, nil
}

func managedIndependentReusableTasks(ctx context.Context, tx *sql.Tx, run, original, origin string) (bool, error) {
	left, logical, err := managedReusableLineage(ctx, tx, run, origin)
	if err != nil || len(left) == 0 {
		return false, err
	}
	right, originalLogical, err := managedReusableLineage(ctx, tx, original, origin)
	if err != nil || len(right) == 0 || logical != "" && logical == originalLogical {
		return false, err
	}
	// Compare from the shared origin. An ancestor or identical lineage is not
	// independent; only a proven divergence between wrapper jobs is isolated.
	for i, j := len(left)-1, len(right)-1; i >= 0 && j >= 0; i, j = i-1, j-1 {
		if left[i] != right[j] {
			return true, nil
		}
	}
	return false, nil
}

func settleManagedTaskAttempt(ctx context.Context, tx *sql.Tx, a Attempt, result StepExecution) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE invocation_id=? AND observation_key IN (?,?)`, a.ID, managedSDKAttemptKey, managedCodexAdmissionKey).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if count != 1 {
		return fmt.Errorf("managed task has conflicting provider admissions")
	}
	var raw []byte
	var id UsageIdentity
	if err := tx.QueryRowContext(ctx, `SELECT record FROM usage_invocations WHERE id=?`, a.ID).Scan(&raw); err != nil {
		return err
	}
	if err := decodeJSONWithNumbers(raw, &id); err != nil {
		return err
	}
	if id.Kind != "task" || id.ID != a.ID || id.TaskAttemptID != a.ID || id.Finished {
		return fmt.Errorf("managed task original usage identity changed")
	}
	// Known charges survive failure, but failed/unknown native execution never
	// settles its marker or permits a retry.
	if result.Status != RunStatusSucceeded {
		return nil
	}
	var admission *managedCodexAdmission
	var terminal *SessionResult
	if id.Provider == "claude-sdk" {
		_, s, e := readManagedSDKAttempt(ctx, tx, a.ID)
		if e != nil {
			return e
		}
		if s.State != "terminal" {
			return fmt.Errorf("SDK task terminal missing")
		}
		admission = s.Admission
		terminal = s.Terminal
	} else if id.Provider == "codex-app-server" {
		r, e := managedRecoveryForInvocation(ctx, tx, a.ID)
		if e != nil {
			return e
		}
		if r == nil || r.State != "terminal" {
			return fmt.Errorf("managed task terminal missing")
		}
		admission = r.Admission
		terminal = r.Terminal
	} else {
		return fmt.Errorf("managed task provider changed")
	}
	if admission == nil || admission.Task == nil || terminal == nil {
		return fmt.Errorf("managed task terminal authority missing")
	}
	binding, ok := runtimeexec.BindingFrom(ctx)
	if !ok {
		f, exists := ctx.Value(fenceKey{}).(executionFence)
		if !exists {
			return fmt.Errorf("managed task completion lacks ownership")
		}
		binding = runtimeexec.Binding{RunID: f.id, Owner: f.owner, Generation: f.generation}
	}
	if id.RunID != binding.RunID {
		return fmt.Errorf("managed task completion origin changed")
	}
	current, err := managedTaskSnapshot(ctx, tx, binding, a.ID, *admission.Task)
	if err != nil {
		return err
	}
	if !sameManagedTaskAdmission(admission, current) {
		return fmt.Errorf("managed task original admission changed")
	}
	expected := cloneJSONMap(terminal.Outputs)
	actual := cloneJSONMap(result.Outputs)
	delete(actual, "queue_time_ms")
	left, _ := json.Marshal(expected)
	right, _ := json.Marshal(actual)
	if !bytes.Equal(left, right) {
		return fmt.Errorf("managed task completion output differs from native checkpoint")
	}
	if id.Provider == "codex-app-server" {
		return finishManagedCodexCheckpoint(ctx, tx, id, *terminal)
	}
	fields, s, err := readManagedSDKAttempt(ctx, tx, a.ID)
	if err != nil {
		return err
	}
	hash, err := managedResultDigest(*terminal)
	if err != nil || hash != s.TerminalSHA256 {
		return fmt.Errorf("SDK task checkpoint changed")
	}
	if err = validateManagedSDKResponseTotals(ctx, tx, a.ID, *terminal); err != nil {
		return err
	}
	if err = observeManagedSDKFinalTx(ctx, tx, id, *terminal); err != nil {
		return err
	}
	s.State = "settled"
	return putManagedSDKAttempt(ctx, tx, a.ID, fields, s)
}

func hasDurableAgentTurns(all map[string]*Workflow) bool {
	for _, w := range all {
		for _, j := range w.Jobs {
			for _, s := range lifecycleSteps(j.Steps) {
				if len(s.Conversation) > 0 || strings.HasPrefix(s.Uses, "agent/") {
					return true
				}
			}
		}
	}
	return false
}
