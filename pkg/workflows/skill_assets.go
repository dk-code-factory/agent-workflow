package workflows

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func skillBundles(definitions map[string]*Workflow) map[string]*AssetBundle {
	out := map[string]*AssetBundle{}
	for _, w := range definitions {
		for _, job := range w.Jobs {
			_, _ = mapSteps(job.Steps, "", func(step Step) (Step, error) {
				if step.Skill != nil && step.Skill.Binding.Assets != nil {
					bundle := step.Skill.Binding.Assets
					out[bundle.OriginID+"/"+bundle.ID] = bundle
				}
				return step, nil
			})
		}
	}
	return out
}

func (s *SQLiteStore) linkSkillAssets(ctx context.Context, tx *sql.Tx, run *Run, snap Snapshot) error {
	for _, bundle := range skillBundles(snap.Definitions) {
		if bundle.OriginID != originID(run, snap) {
			return fmt.Errorf("skill asset originating owner changed")
		}
		if err := s.VerifyAssetBundle(bundle); err != nil {
			return err
		}
		raw, _ := json.Marshal(bundle)
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO helper_assets(origin_id,id,manifest) VALUES(?,?,?)", bundle.OriginID, bundle.ID, raw); err != nil {
			return err
		}
		var retained []byte
		if err := tx.QueryRowContext(ctx, "SELECT manifest FROM helper_assets WHERE origin_id=? AND id=?", bundle.OriginID, bundle.ID).Scan(&retained); err != nil {
			return err
		}
		if !bytes.Equal(raw, retained) {
			return fmt.Errorf("immutable skill asset manifest changed")
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO run_helper_assets(run_id,asset_origin,asset_id) VALUES(?,?,?)", run.ID, bundle.OriginID, bundle.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) verifySkillAssets(ctx context.Context, run string, snap Snapshot) error {
	for _, bundle := range skillBundles(snap.Definitions) {
		var retained []byte
		if err := s.db.QueryRowContext(ctx, `SELECT a.manifest FROM helper_assets a JOIN run_helper_assets r ON r.asset_origin=a.origin_id AND r.asset_id=a.id WHERE r.run_id=? AND a.origin_id=? AND a.id=?`, run, bundle.OriginID, bundle.ID).Scan(&retained); err != nil {
			return fmt.Errorf("retained skill asset reference missing: %w", err)
		}
		raw, _ := json.Marshal(bundle)
		if !bytes.Equal(raw, retained) {
			return fmt.Errorf("retained skill asset manifest changed")
		}
		var missing string
		err := s.db.QueryRowContext(ctx, `SELECT r.id FROM runs r WHERE (r.id=? OR r.origin_id=?) AND NOT EXISTS (SELECT 1 FROM run_helper_assets a WHERE a.run_id=r.id AND a.asset_origin=? AND a.asset_id=?) LIMIT 1`, run, run, bundle.OriginID, bundle.ID).Scan(&missing)
		if err == nil {
			return fmt.Errorf("retained skill reference missing for run %s", missing)
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err = s.VerifyAssetBundle(bundle); err != nil {
			return err
		}
	}
	return nil
}

// SkillAssets verifies the run-owned immutable binding before materialization.
func (s *SQLiteStore) SkillAssets(ctx context.Context, run string, plan *SkillInvocation) (string, error) {
	_, snap, err := s.Load(ctx, run)
	if err != nil {
		return "", err
	}
	if plan == nil || plan.ID != skillPlanID(plan) {
		return "", fmt.Errorf("skill invocation identity changed")
	}
	found := false
	for _, w := range snap.Definitions {
		for _, job := range w.Jobs {
			_, _ = mapSteps(job.Steps, "", func(step Step) (Step, error) {
				if step.Skill != nil && step.Skill.ID == plan.ID {
					found = true
				}
				return step, nil
			})
		}
	}
	if !found {
		return "", fmt.Errorf("skill invocation is outside admitted run")
	}
	if err = s.verifySkillAssets(ctx, run, snap); err != nil {
		return "", err
	}
	return s.AssetBundlePath(plan.Binding.Assets)
}

// Verify original native-consumption receipts before a recovered run is claimed.
func (s *SQLiteStore) verifySkillContinuations(ctx context.Context, run string) error {
	workers, err := (&WorkerManager{Store: s}).List(ctx, run)
	if err != nil {
		return err
	}
	for _, worker := range workers {
		if err := worker.validateSkillContinuation(); err != nil {
			return fmt.Errorf("worker %s: %w", worker.ID, err)
		}
	}
	return nil
}
