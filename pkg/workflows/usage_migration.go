package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func historyProvider(snapshot Snapshot, role string) (string, string) {
	var config map[string]any
	if decodeJSONWithNumbers(snapshot.Roles, &config) != nil {
		return "unknown", "unknown"
	}
	roles, _ := config["Agents"].(map[string]any)
	agent, _ := roles[role].(map[string]any)
	provider, model := nativeString(agent, "Provider"), nativeString(agent, "Model")
	if provider == "" {
		provider = "unknown"
	}
	if model == "" {
		model = "unknown"
	}
	return provider, model
}

func backfillUsage(ctx context.Context, tx *sql.Tx) error {
	type item struct {
		identity   UsageIdentity
		outputs    map[string]any
		incomplete bool
	}
	emit := func(candidate item) error {
		raw, err := json.Marshal(candidate.identity)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO usage_invocations(id,origin_id,record) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING", candidate.identity.ID, candidate.identity.RunID, raw)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			var priorRaw []byte
			if err = tx.QueryRowContext(ctx, "SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'", candidate.identity.ID).Scan(&priorRaw); err != nil {
				return err
			}
			var previous UsageObservation
			if err = decodeJSONWithNumbers(priorRaw, &previous); err != nil {
				return err
			}
			if previous.InputTokens != nil || candidate.outputs["usage"] == nil {
				return nil
			}
		}
		update := usageUpdate(candidate.outputs, true)
		update.Incomplete = update.Incomplete || candidate.incomplete
		if err = observeUsageTx(ctx, tx, candidate.identity, update); err != nil {
			return fmt.Errorf("historical usage normalization: %w", err)
		}
		return nil
	}

	rows, err := tx.QueryContext(ctx, "SELECT a.record,r.record,r.snapshot,r.origin_id FROM attempts a JOIN runs r ON r.id=a.run_id ORDER BY a.rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		var attemptRaw, runRaw, snapshotRaw []byte
		var origin string
		if err = rows.Scan(&attemptRaw, &runRaw, &snapshotRaw, &origin); err != nil {
			_ = rows.Close()
			return err
		}
		var attempt Attempt
		var run Run
		var snapshot Snapshot
		if err = decodeJSONWithNumbers(attemptRaw, &attempt); err != nil {
			_ = rows.Close()
			return err
		}
		if err = decodeJSONWithNumbers(runRaw, &run); err != nil {
			_ = rows.Close()
			return err
		}
		if err = decodeJSONWithNumbers(snapshotRaw, &snapshot); err != nil {
			_ = rows.Close()
			return err
		}
		role := ""
		workflow := snapshot.Definitions[run.WorkflowRef]
		if workflow != nil {
			for index, step := range workflow.Jobs[attempt.JobID].Steps {
				id := step.ID
				if id == "" {
					id = "step_" + strconv.Itoa(index+1)
				}
				if id == attempt.StepID && strings.HasPrefix(step.Uses, "agent/") {
					role = strings.TrimPrefix(step.Uses, "agent/")
					break
				}
			}
		}
		if role == "" && attempt.TaskOutputs == nil && attempt.Result.Outputs["usage"] == nil && nativeString(attempt.Result.Outputs, "provider") == "" {
			continue
		}
		provider, model := historyProvider(snapshot, role)
		taskID := attempt.TaskAttemptID
		if taskID == "" {
			taskID = attempt.ID
		}
		identity := UsageIdentity{ID: taskID, RunID: origin, TaskAttemptID: taskID, Kind: "task", Provider: provider, Model: model}
		outputs := attempt.TaskOutputs
		if outputs == nil {
			outputs = attempt.Result.Outputs
		}
		if err = emit(item{identity: identity, outputs: outputs, incomplete: attempt.Status == RunStatusRunning && !schemaPhase(attempt.Phase)}); err != nil {
			_ = rows.Close()
			return err
		}
		for index := 0; index < attempt.RepairCount || index < len(attempt.RepairOutputs); index++ {
			repair := identity
			repair.ID = taskID + "/legacy-repair/" + strconv.Itoa(index+1)
			repair.Kind = "repair"
			var outputs map[string]any
			if index < len(attempt.RepairOutputs) {
				outputs = attempt.RepairOutputs[index]
			}
			if err = emit(item{identity: repair, outputs: outputs, incomplete: outputs == nil}); err != nil {
				_ = rows.Close()
				return err
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, "SELECT m.record,w.record,r.snapshot,w.run_id FROM messages m JOIN workers w ON w.id=m.worker_id JOIN runs r ON r.id=w.run_id ORDER BY m.rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		var messageRaw, workerRaw, snapshotRaw []byte
		var origin string
		if err = rows.Scan(&messageRaw, &workerRaw, &snapshotRaw, &origin); err != nil {
			_ = rows.Close()
			return err
		}
		var message WorkerMessage
		var worker Worker
		var snapshot Snapshot
		if err = decodeJSONWithNumbers(messageRaw, &message); err != nil {
			_ = rows.Close()
			return err
		}
		if err = decodeJSONWithNumbers(workerRaw, &worker); err != nil {
			_ = rows.Close()
			return err
		}
		if err = decodeJSONWithNumbers(snapshotRaw, &snapshot); err != nil {
			_ = rows.Close()
			return err
		}
		if message.StartedAt.IsZero() && message.Status == RunStatusQueued {
			continue
		}
		provider, model := historyProvider(snapshot, worker.Role)
		identity := UsageIdentity{ID: message.ID, RunID: origin, WorkerID: worker.ID, Kind: "worker", Provider: provider, Model: model, SessionID: message.Result.SessionID}
		if identity.SessionID == "" {
			identity.SessionID = worker.SessionID
		}
		if err = emit(item{identity: identity, outputs: message.Result.Outputs, incomplete: message.Status == RunStatusRunning}); err != nil {
			_ = rows.Close()
			return err
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}

	return nil
}
