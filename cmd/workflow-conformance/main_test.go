package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConformanceRefusesCIAndImplicitLiveExecution(t *testing.T) {
	before := os.Args
	defer func() { os.Args = before }()
	os.Args = []string{"workflow-conformance", "--allow-live", "--scenario", "must-not-read"}
	t.Setenv("CI", "") // presence, including an empty value, refuses execution
	if err := run(); err == nil || !strings.Contains(err.Error(), "CI") {
		t.Fatal("CI reached scenario effects", err)
	}
	_ = os.Unsetenv("CI")
	os.Args = []string{"workflow-conformance", "--scenario", "must-not-read"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "--allow-live") {
		t.Fatal("generic tests/requests authorized live calls", err)
	}
}

func TestCommittedScenariosHaveImplementedDriversAndAssertions(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "conformance", "scenarios", "*.json"))
	if err != nil || len(paths) < 12 {
		t.Fatal(paths, err)
	}
	drivers := map[string]bool{}
	for _, name := range []string{"agent", "structured", "session", "interrupt", "managed-canary", "nested", "container", "container-owner-loss", "external-uncertain", "workflow-cli", "workflow-mcp", "publication"} {
		drivers[name] = true
	}
	assertions := map[string]bool{}
	for _, name := range []string{"scenario_completed", "private_workspace", "usage_observed", "workflow_succeeded", "response_marker", "session_context_retained", "interruption_retains_session", "structured_schema_valid", "native_canaries_suppressed", "nested_workers_at_capacity_one", "container_cleanup_confirmed", "external_mcp_call_recorded", "uncertain_effect_not_replayed", "owner_loss_quarantined", "owner_loss_cleanup_confirmed", "one_durable_publication"} {
		assertions[name] = true
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var spec manifest
		if err = json.Unmarshal(data, &spec); err != nil {
			t.Fatal(path, err)
		}
		if !drivers[spec.Driver] || spec.ID == "" || len(spec.Assertions) == 0 {
			t.Fatal("scenario is not runnable", path)
		}
		for _, assertion := range spec.Assertions {
			if !assertions[assertion] {
				t.Fatal("scenario has an unimplemented assertion", path, assertion)
			}
		}
	}
}

func TestEvidenceLocationRejectsCheckoutAndSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateEvidenceLocation(filepath.Join(checkout, "private", "new")); err == nil {
		t.Fatal("evidence accepted inside checkout")
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(checkout, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateEvidenceLocation(filepath.Join(alias, "new")); err == nil {
		t.Fatal("checkout symlink bypassed evidence isolation")
	}
	if err := validateEvidenceLocation(filepath.Join(filepath.VolumeName(root)+string(os.PathSeparator), "nonexistent-private-evidence-test")); err != nil {
		t.Fatal(err)
	}
}
