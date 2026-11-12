package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReceiptRecoveryMatrix(t *testing.T) {
	for _, scenario := range []string{"unknown", "lookup_error", "completed", "no_effect"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ev, err := runScenario(ctx, t.TempDir(), scenario)
			if err != nil {
				t.Fatal(err)
			}
			if err = checkEvidence(ev); err != nil {
				t.Fatal(err)
			}
			if scenario != "no_effect" && !strings.Contains(ev.NegativeControl, "got 2") {
				t.Fatal("duplicate-effect negative control missing", ev.NegativeControl)
			}
			b, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			var restored evidence
			if err = json.Unmarshal(b, &restored); err != nil {
				t.Fatal(err)
			}
			if err = checkEvidence(restored); err != nil {
				t.Fatal("serialized evidence lost proof", err)
			}
		})
	}
}

func TestRetainedCommandEvidence(t *testing.T) {
	parent := t.TempDir()
	sentinel := filepath.Join(parent, "unrelated.txt")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := command(parent, &stdout); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(parent, "receipt-recovery-*"))
	if err != nil || len(paths) != 1 {
		t.Fatal("expected exactly one owned fixture directory", paths, err)
	}
	data, err := os.ReadFile(filepath.Join(paths[0], "evidence.json"))
	if err != nil || !bytes.Equal(data, stdout.Bytes()) {
		t.Fatal("retained evidence differs from stdout", err)
	}
	var retained []evidence
	if err = json.Unmarshal(data, &retained); err != nil || len(retained) != 4 {
		t.Fatal("incomplete matrix evidence", err)
	}
	for _, ev := range retained {
		if err = checkEvidence(ev); err != nil {
			t.Fatal(err)
		}
		c, err := openCounter(filepath.Join(paths[0], ev.Scenario, "external.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		n, countErr := c.count(context.Background())
		closeErr := c.db.Close()
		if countErr != nil || closeErr != nil || n != ev.Counter {
			t.Fatal("retained counter does not match evidence", n, countErr, closeErr)
		}
	}
	data, err = os.ReadFile(sentinel)
	if err != nil || string(data) != "preserve" {
		t.Fatal("unrelated output-parent file changed", err)
	}
}
