//go:build linux || darwin

package process

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFrozenContainerLifetimeCannotStartWork(t *testing.T) {
	root := t.TempDir()
	lease := filepath.Join(root, "lease")
	marker := filepath.Join(root, "must-not-start")
	if err := os.WriteFile(lease, []byte(`{"token":"fixture","sequence":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := runContainerWorkload(context.Background(), lease, "fixture", []string{"sh", "-c", `touch "$1"`, "sh", marker}, 100*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("frozen controller record launched work")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("work ran without a live controller")
	}
}

func TestContainerLifetimeLossStopsActiveWork(t *testing.T) {
	root := t.TempDir()
	lease := filepath.Join(root, "lease")
	marker := filepath.Join(root, "started")
	write := func(sequence uint64) {
		data, _ := json.Marshal(lifetimeObservation{Token: "fixture", Sequence: sequence})
		if err := os.WriteFile(lease+".partial", data, 0600); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(lease+".partial", lease); err != nil {
			t.Error(err)
		}
	}
	write(1)
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		var sequence uint64 = 2
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				write(sequence)
				sequence++
			}
		}
	}()
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_, err := runContainerWorkload(ctx, lease, "fixture", []string{"sh", "-c", `echo yes > "$1"; sleep 30`, "sh", marker}, 150*time.Millisecond, 10*time.Millisecond)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			close(stop)
			<-writerDone
			t.Fatal("workload did not start")
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	<-writerDone
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lost lifetime was treated as successful completion")
		}
	case <-time.After(time.Second):
		t.Fatal("work survived controller lifetime loss")
	}
}
