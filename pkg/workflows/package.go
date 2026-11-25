package workflows

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// PackageBinding retains source identity and the selected workflow, never
// workspace permissions or credentials. Installation paths are deliberately
// absent: recovery uses Assets and the controller's target workspace.
type PackageBinding struct {
	Version        int          `json:"version"`
	Name           string       `json:"name"`
	PackageVersion string       `json:"package_version"`
	EngineVersion  string       `json:"engine_version"`
	Skill          string       `json:"skill"`
	Workflow       string       `json:"workflow"`
	Digest         string       `json:"digest"`
	Assets         *AssetBundle `json:"assets,omitempty"`
}

// PackageSource is a validated in-memory closure for fresh admission only.
// Callers must coordinate capture+admission with exclusive state ownership;
// unreferenced asset staging can otherwise be collected by recovery cleanup.
type PackageSource struct {
	Binding PackageBinding
	Entries []AssetInput
}

// SamePackage compares admission identity without origin-specific storage paths.
func SamePackage(a, b *PackageBinding) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	x, y := *a, *b
	x.Assets, y.Assets = nil, nil
	return x == y
}

func (e *Executor) sourceRoot() string {
	if e.DefinitionRoot != "" {
		return e.DefinitionRoot
	}
	return e.WorkspaceDir
}

func (e *Executor) pinPackage(ctx context.Context, source *PackageSource) error {
	if source == nil {
		return nil
	}
	store := invocationStore(e.Store)
	if store == nil {
		return fmt.Errorf("package execution requires durable owned storage")
	}
	b := source.Binding
	canonical, err := CanonicalLocalRef(b.Workflow)
	if err != nil || canonical != b.Workflow || b.Version != 1 || b.Name == "" || b.PackageVersion == "" || b.EngineVersion == "" || b.Skill == "" || len(b.Digest) != 64 || !hexID(b.Digest) || b.Assets != nil {
		return fmt.Errorf("invalid package admission identity")
	}
	entries := make([]AssetInput, 0, len(source.Entries))
	e.packageFiles = make(map[string]AssetInput, len(source.Entries))
	for _, entry := range source.Entries {
		entry.Data = bytes.Clone(entry.Data)
		entries = append(entries, entry)
		e.packageFiles[entry.Path] = entry
	}
	b.Assets, err = store.CaptureAssetBundle(ctx, e.bundleOrigin, entries)
	if err != nil {
		return err
	}
	if b.Digest != b.Assets.ID {
		return fmt.Errorf("captured package digest differs from admitted bytes")
	}
	e.packageBinding = &b
	e.DefinitionsDir = ""
	e.DefinitionRoot, err = store.AssetBundlePath(b.Assets)
	return err
}

func (e *Executor) capturedAgent(reference string) (*AgentDefinition, error) {
	relative, err := filepath.Rel(e.DefinitionRoot, reference)
	if err != nil || !workflowPathStrictlyInside(e.DefinitionRoot, reference) {
		return nil, fmt.Errorf("package agent definition escapes source root")
	}
	name := filepath.ToSlash(relative)
	entry, ok := e.packageFiles[name]
	if ok && entry.Mode.IsDir() {
		var candidates []string
		for candidate, file := range e.packageFiles {
			if path.Dir(candidate) == name && !file.Mode.IsDir() && isYAMLDefinition(candidate) {
				candidates = append(candidates, candidate)
			}
		}
		if len(candidates) != 1 {
			return nil, fmt.Errorf("package agent directory requires exactly one YAML definition")
		}
		name = candidates[0]
		entry = e.packageFiles[name]
	}
	if !ok || !isYAMLDefinition(name) || strings.Contains(name, "\\") {
		return nil, fmt.Errorf("package agent %q requires a captured YAML definition; use agent-workflow agent convert", name)
	}
	d, err := ParseAgentDefinition(entry.Data)
	if err != nil {
		return nil, err
	}
	d.Source, d.Bytes = "package:"+name, string(entry.Data)
	return d, nil
}

func (e *Executor) restorePackage(ctx context.Context, run string, snap Snapshot) error {
	if snap.Package == nil {
		return nil
	}
	store := invocationStore(e.Store)
	if store == nil {
		return fmt.Errorf("retained package requires durable owned storage")
	}
	if err := store.verifyPackageAssets(ctx, run, snap); err != nil {
		return err
	}
	e.packageBinding = snap.Package
	root, err := store.AssetBundlePath(snap.Package.Assets)
	e.DefinitionRoot = root
	return err
}

func (e *Executor) packageContext() map[string]any {
	if e.packageBinding == nil {
		return nil
	}
	return map[string]any{"root": e.DefinitionRoot, "name": e.packageBinding.Name, "version": e.packageBinding.PackageVersion, "digest": e.packageBinding.Digest}
}

func (s *SQLiteStore) linkPackageAssets(ctx context.Context, tx *sql.Tx, run *Run, snap Snapshot) error {
	if snap.Package == nil {
		return nil
	}
	b := snap.Package.Assets
	if b == nil || b.OriginID != originID(run, snap) || b.ID != snap.Package.Digest {
		return fmt.Errorf("retained package ownership/identity changed")
	}
	if err := s.VerifyAssetBundle(b); err != nil {
		return err
	}
	raw, _ := json.Marshal(b)
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO helper_assets(origin_id,id,manifest) VALUES(?,?,?)", b.OriginID, b.ID, raw); err != nil {
		return err
	}
	var retained []byte
	if err := tx.QueryRowContext(ctx, "SELECT manifest FROM helper_assets WHERE origin_id=? AND id=?", b.OriginID, b.ID).Scan(&retained); err != nil {
		return err
	}
	if !bytes.Equal(raw, retained) {
		return fmt.Errorf("immutable package asset manifest changed")
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO run_helper_assets(run_id,asset_origin,asset_id) VALUES(?,?,?)", run.ID, b.OriginID, b.ID)
	return err
}

func (s *SQLiteStore) verifyPackageAssets(ctx context.Context, run string, snap Snapshot) error {
	if snap.Package == nil {
		return nil
	}
	b := snap.Package.Assets
	if b == nil || b.ID != snap.Package.Digest || snap.Package.Version != 1 {
		return fmt.Errorf("retained package identity missing or changed")
	}
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT a.manifest FROM helper_assets a JOIN run_helper_assets r ON r.asset_origin=a.origin_id AND r.asset_id=a.id WHERE r.run_id=? AND a.origin_id=? AND a.id=?`, run, b.OriginID, b.ID).Scan(&raw); err != nil {
		return fmt.Errorf("retained package asset reference missing: %w", err)
	}
	expected, _ := json.Marshal(b)
	if !bytes.Equal(raw, expected) {
		return fmt.Errorf("retained package asset reference changed")
	}
	var missing string
	err := s.db.QueryRowContext(ctx, `SELECT r.id FROM runs r WHERE (r.id=? OR r.origin_id=?) AND NOT EXISTS (SELECT 1 FROM run_helper_assets a WHERE a.run_id=r.id AND a.asset_origin=? AND a.asset_id=?) LIMIT 1`, run, run, b.OriginID, b.ID).Scan(&missing)
	if err == nil {
		return fmt.Errorf("retained package asset reference missing for run %s", missing)
	}
	if err != sql.ErrNoRows {
		return err
	}
	return s.VerifyAssetBundle(b)
}

// PackageAssets returns the verified retained resource manifest and root for a
// run. Consumers must keep its existing run reference live while using the path.
func (s *SQLiteStore) PackageAssets(ctx context.Context, run string) (*PackageBinding, string, error) {
	_, snap, err := s.Load(ctx, run)
	if err != nil {
		return nil, "", err
	}
	if snap.Package == nil {
		return nil, "", fmt.Errorf("run has no admitted package")
	}
	if err := s.verifyPackageAssets(ctx, run, snap); err != nil {
		return nil, "", err
	}
	root, err := s.AssetBundlePath(snap.Package.Assets)
	if err != nil {
		return nil, "", err
	}
	return snap.Package, filepath.Clean(root), nil
}
