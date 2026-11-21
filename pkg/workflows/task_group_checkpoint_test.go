package workflows

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestTaskGroupCheckpointObservesChangedBytesAndRejectsInvalidRecords(t *testing.T) {
	store := taskStateStore(t)
	run, ctx := taskStateRun(t, store, "original")
	for i := 0; i < 2; i++ {
		if group, err := store.checkpointTaskGroup(ctx, run.ID); err != nil || group != "original" {
			t.Fatal(group, err)
		}
	}
	var record, snapshot []byte
	if err := store.db.QueryRowContext(ctx, "SELECT record,snapshot FROM runs WHERE id=?", run.ID).Scan(&record, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, record, snapshot, group string
		failure                       bool
	}{
		{"updated group", string(record), `{"version":1,"metadata":{"task_group":"modified"}}`, "modified", false},
		{"ungrouped", string(record), `{"version":1}`, "", false},
		{"unsupported result", `{"version":2}`, string(snapshot), "", true},
		{"unsupported snapshot", string(record), `{"version":2}`, "", true},
		{"malformed result", `{`, string(snapshot), "", true},
		{"malformed snapshot", string(record), `{`, "", true},
		{"invalid result field", `{"version":1,"jobs":false}`, string(snapshot), "", true},
		{"invalid snapshot field", string(record), `{"version":1,"definitions":false}`, "", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if _, err := store.db.ExecContext(ctx, "UPDATE runs SET record=?,snapshot=? WHERE id=?", []byte(scenario.record), []byte(scenario.snapshot), run.ID); err != nil {
				t.Fatal(err)
			}
			group, err := store.checkpointTaskGroup(ctx, run.ID)
			if (err != nil) != scenario.failure || group != scenario.group {
				t.Fatal(group, err)
			}
		})
	}
	if _, err := store.checkpointTaskGroup(ctx, randomID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing run reused another checkpoint", err)
	}
}

func TestTaskGroupCheckpointConcurrentRunsKeepTheirOwnGroups(t *testing.T) {
	store := taskStateStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		want := fmt.Sprintf("group-%d", i)
		run, ctx := taskStateRun(t, store, want)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				if group, err := store.checkpointTaskGroup(ctx, run.ID); err != nil || group != want {
					t.Error(group, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestTaskGroupCheckpointBoundsRetainedMemory(t *testing.T) {
	store := taskStateStore(t)
	run, ctx := taskStateRun(t, store, "large")
	run.Error = strings.Repeat("x", (4<<20)+1)
	record, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE runs SET record=? WHERE id=?", record, run.ID); err != nil {
		t.Fatal(err)
	}
	if group, err := store.checkpointTaskGroup(ctx, run.ID); err != nil || group != "large" {
		t.Fatal(group, err)
	}
	if len(store.taskGroupCache.record)+len(store.taskGroupCache.snapshot) > 4<<20 {
		t.Fatal("oversized checkpoint retained in the bounded cache")
	}
}
