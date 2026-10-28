package containerapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

func ownedFixture(t *testing.T) (Owner, string) {
	t.Helper()
	dir := t.TempDir()
	runtime := filepath.Join(dir, "podman")
	script := `#!/bin/sh
set -eu
cd "$(dirname "$0")"
case "$1 $2" in
 "info --format") test ! -e fail-info; cat info.json ;;
 "system connection") printf '[]' ;;
 "container inspect") test ! -e fail-inspect; cat inspect.json ;;
 "ps --all") test ! -e fail-list; cat listing ;;
 "rm --force") printf '%s\n' "$3" >> removed; test ! -e fail-remove; if test ! -e keep-after-remove; then rm -f inspect.json; : > listing; fi ;;
 *) exit 99 ;;
esac
`
	if err := os.WriteFile(runtime, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"info.json": `{"host":{"hostname":"original"},"store":{"graphRoot":"/store"}}`, "listing": ""} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id := strings.Repeat("a", 64)
	object := []map[string]any{{"Id": id, "Name": "/owned", "Config": map[string]any{"Labels": map[string]string{"invocation": "retained", "workflow": "expected"}}}}
	data, _ := json.Marshal(object)
	if err := os.WriteFile(filepath.Join(dir, "inspect.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := Identity(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	return Owner{Runtime: runtime, Identity: identity, Name: "owned", ID: id, Labels: map[string]string{"invocation": "retained", "workflow": "expected"}}, dir
}

func TestRemoveRequiresExactStoreIDAndOwnership(t *testing.T) {
	for _, scenario := range []string{"owned", "store-changed", "wrong-id", "wrong-label", "missing-labels", "api-unavailable", "absence-unproved", "remove-unconfirmed"} {
		t.Run(scenario, func(t *testing.T) {
			owner, dir := ownedFixture(t)
			write := func(name, value string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "store-changed":
				write("info.json", `{"host":{"hostname":"original"},"store":{"graphRoot":"/replacement"}}`)
			case "wrong-id":
				owner.ID = strings.Repeat("b", 64)
			case "wrong-label":
				owner.Labels["invocation"] = "another"
			case "missing-labels":
				owner.Labels = nil
			case "api-unavailable":
				write("fail-info", "")
			case "absence-unproved":
				write("fail-inspect", "")
				write("fail-list", "")
			case "remove-unconfirmed":
				write("keep-after-remove", "")
			}
			err := Remove(context.Background(), owner)
			if scenario == "owned" {
				if err != nil {
					t.Fatal(err)
				}
				removed, err := os.ReadFile(filepath.Join(dir, "removed"))
				if err != nil || strings.TrimSpace(string(removed)) != owner.ID {
					t.Fatalf("removed %q: %v", removed, err)
				}
			} else {
				if err == nil {
					t.Fatal("unproved cleanup succeeded")
				}
				if scenario != "remove-unconfirmed" {
					if _, err := os.Stat(filepath.Join(dir, "removed")); !os.IsNotExist(err) {
						t.Fatal("removed without ownership proof")
					}
				}
			}
		})
	}
}

func TestFailedInspectionIsNotAbsenceWhenListingStillContainsOwnedObject(t *testing.T) {
	owner, dir := ownedFixture(t)
	for _, listing := range []string{owner.ID + " owned\n", owner.ID + " renamed\n"} {
		if err := os.WriteFile(filepath.Join(dir, "fail-inspect"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "listing"), []byte(listing), 0600); err != nil {
			t.Fatal(err)
		}
		_, exists, err := Inspect(context.Background(), owner)
		if err == nil || !exists {
			t.Fatalf("failed inspect became absence: exists=%v err=%v", exists, err)
		}
	}
}

func TestUnavailableRetainedRuntimeNeverFallsBackToHost(t *testing.T) {
	owner, dir := ownedFixture(t)
	owner.Network = &network.Reference{Name: "missing-recovery-test", Identity: strings.Repeat("a", 32), Generation: 1}
	owner.Network.Endpoint = network.Endpoint(owner.Network.Name, owner.Network.Identity)
	if err := Remove(context.Background(), owner); err == nil {
		t.Fatal("unavailable retained runtime accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "removed")); !os.IsNotExist(err) {
		t.Fatal("cleanup fell back to host")
	}
}
