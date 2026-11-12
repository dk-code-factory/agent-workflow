package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/controller"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResumeAfterTerminalOwnerRelease(t *testing.T) {
	const path = "/runs/fixture/resume?request_id=recover-2&retry_uncertain=true"
	for _, tc := range []struct {
		name      string
		first     string
		wantErr   string
		wantCalls int
	}{
		{name: "terminal_owner_still_active", first: "run is already active; no matching resume request", wantCalls: 2},
		{name: "changed_acknowledgement", first: "request_id reused with different content", wantErr: "request_id reused with different content", wantCalls: 1},
		{name: "other_failure", first: "storage unavailable", wantErr: "storage unavailable", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			client := &controller.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				status, body := http.StatusAccepted, `{"execution_generation":3,"status":"queued"}`
				if len(requests) == 1 {
					status = http.StatusConflict
					encoded, err := json.Marshal(map[string]string{"error": tc.first})
					if err != nil {
						return nil, err
					}
					body = string(encoded)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			run, err := resumeRun(ctx, client, path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected %q, got %v", tc.wantErr, err)
				}
			} else if err != nil || run.Generation != 3 || run.Status != "queued" {
				t.Fatalf("resume after owner release: run=%+v, err=%v", run, err)
			}
			if len(requests) != tc.wantCalls {
				t.Fatalf("expected %d requests, got %v", tc.wantCalls, requests)
			}
			for _, request := range requests {
				if request != "POST "+path {
					t.Fatalf("resume identity or acknowledgement changed: %s", request)
				}
			}
		})
	}
}

func TestResumeOwnerReleaseWaitHasDeadline(t *testing.T) {
	client := &controller.Client{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusConflict, Body: io.NopCloser(strings.NewReader(`{"error":"run is already active; no matching resume request"}`)), Header: make(http.Header)}, nil
	})}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := resumeRun(ctx, client, "/runs/fixture/resume?request_id=recover-2&retry_uncertain=true"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("owner release wait did not respect deadline: %v", err)
	}
}

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
