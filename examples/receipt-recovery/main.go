// Command receipt-recovery demonstrates caller-driven registered-action recovery
// using only a local controller, SQLite, and a deliberately non-idempotent fake.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	_ "modernc.org/sqlite"
)

type input struct{}
type output struct {
	Receipt int64 `json:"receipt"`
}

type resumeEvidence struct {
	RequestID           string `json:"request_id"`
	RetryUncertain      bool   `json:"retry_uncertain"`
	Generation          int64  `json:"generation"`
	DuplicateGeneration int64  `json:"duplicate_generation"`
	AcceptedStatus      string `json:"accepted_status"`
	TerminalStatus      string `json:"terminal_status"`
	Conflict            string `json:"changed_acknowledgement_conflict"`
}
type evidence struct {
	Scenario              string                    `json:"scenario"`
	RunID                 string                    `json:"run_id"`
	Status                string                    `json:"status"`
	OriginalCounter       int                       `json:"original_counter"`
	Counter               int                       `json:"counter"`
	CheckpointValidations int32                     `json:"checkpoint_validations"`
	Resumes               []resumeEvidence          `json:"resumes"`
	Attempts              []workflows.ActionAttempt `json:"action_attempts"`
	Events                []workflows.Event         `json:"events"`
	NegativeControl       string                    `json:"negative_control"`
}

// The fake has no idempotency constraint: replaying a lost response really does
// increment the external counter twice. It is separate from the engine journal.
type counter struct{ db *sql.DB }

func openCounter(path string) (*counter, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS effects (receipt INTEGER PRIMARY KEY, operation_key TEXT NOT NULL, attempt_id TEXT NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	return &counter{db}, nil
}
func (c *counter) increment(ctx context.Context, op workflows.ActionContext) (int64, error) {
	r, err := c.db.ExecContext(ctx, "INSERT INTO effects(operation_key,attempt_id) VALUES(?,?)", op.OperationKey, op.AttemptID)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}
func (c *counter) count(ctx context.Context) (n int, err error) {
	err = c.db.QueryRowContext(ctx, "SELECT count(*) FROM effects").Scan(&n)
	return
}
func (c *counter) result(ctx context.Context, op workflows.ActionContext) (workflows.ActionResult[output], error) {
	var receipt int64
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT count(*),coalesce(max(receipt),0) FROM effects WHERE operation_key=?", op.OperationKey).Scan(&n, &receipt); err != nil {
		return workflows.ActionResult[output]{}, err
	}
	if n != 1 {
		return workflows.ActionResult[output]{}, fmt.Errorf("expected exactly one external effect, got %d", n)
	}
	proof, err := json.Marshal(map[string]any{"operation_key": op.OperationKey, "receipt": receipt})
	return workflows.ActionResult[output]{Output: output{receipt}, Evidence: proof, Receipt: proof}, err
}

// Model a lost admission acknowledgement after the real SQLite commit. The
// engine sees failure before invoking the mutation, while admission remains.
type admissionLoss struct {
	*workflows.SQLiteStore
	lose bool
}

func (s *admissionLoss) AdmitAction(ctx context.Context, a workflows.ActionAttempt) error {
	if err := s.SQLiteStore.AdmitAction(ctx, a); err != nil {
		return err
	}
	if s.lose {
		s.lose = false
		return errors.New("injected lost admission acknowledgement before handler")
	}
	return nil
}

func register(c *counter, store *workflows.SQLiteStore, scenario string, original bool, validations *atomic.Int32, binding string) (*workflows.ActionRegistry, error) {
	registry := &workflows.ActionRegistry{}
	err := workflows.RegisterAction(registry, "increment", workflows.ActionSpec[input, output]{
		Implementation: "receipt-recovery-fixture-v1", Effect: workflows.ActionExternalMutation,
		Bindings: func(context.Context, workflows.ActionContext, input) (json.RawMessage, error) {
			return json.Marshal(map[string]string{"counter": binding})
		},
		Handler: func(ctx context.Context, op workflows.ActionContext, _ input) (workflows.ActionResult[output], error) {
			if scenario == "no_effect" {
				// A new mutation must observe the committed absence proof, not merely a
				// successful lookup or an in-memory Reconcile return value.
				attempts, err := store.ActionAttempts(ctx, op.RunID)
				if err != nil {
					return workflows.ActionResult[output]{}, err
				}
				if len(attempts) != 2 || attempts[0].State != "no_effect" || len(attempts[0].Reconciliations) != 1 || len(attempts[0].Reconciliations[0].Proof) == 0 || attempts[0].Context.OperationKey != op.OperationKey || attempts[0].Context.AttemptID == op.AttemptID {
					return workflows.ActionResult[output]{}, errors.New("new attempt preceded committed no-effect proof")
				}
			}
			if _, err := c.increment(ctx, op); err != nil {
				return workflows.ActionResult[output]{}, err
			}
			if original {
				return workflows.ActionResult[output]{}, errors.New("injected response loss after durable external increment")
			}
			return c.result(ctx, op)
		},
		ValidateCheckpoint: func(ctx context.Context, op workflows.ActionContext, cp workflows.ActionCheckpoint[input, output]) error {
			validations.Add(1)
			expected, err := c.result(ctx, op)
			if err != nil {
				return err
			}
			if cp.Result.Output != expected.Output || string(cp.Result.Evidence) != string(expected.Evidence) || string(cp.Result.Receipt) != string(expected.Receipt) {
				return errors.New("recovered output, evidence or receipt differs from fake provider")
			}
			return nil
		},
		Reconcile: func(ctx context.Context, op workflows.ActionContext, _ workflows.ActionCheckpoint[input, output]) (workflows.ActionReconciliation[output], error) {
			switch scenario {
			case "unknown":
				return workflows.ActionReconciliation[output]{Outcome: "unknown"}, nil
			case "lookup_error":
				return workflows.ActionReconciliation[output]{}, errors.New("injected receipt lookup unavailable")
			case "completed":
				result, err := c.result(ctx, op)
				return workflows.ActionReconciliation[output]{Outcome: "completed", Proof: result.Receipt, Result: result}, err
			case "no_effect":
				n, err := c.count(ctx)
				if err != nil {
					return workflows.ActionReconciliation[output]{}, err
				}
				if n != 0 {
					return workflows.ActionReconciliation[output]{}, errors.New("fake provider cannot prove absence")
				}
				proof, err := json.Marshal(map[string]any{"operation_key": op.OperationKey, "counter": n, "authoritative": true})
				return workflows.ActionReconciliation[output]{Outcome: "no_effect", Proof: proof}, err
			default:
				return workflows.ActionReconciliation[output]{}, errors.New("unknown fixture scenario")
			}
		},
	})
	return registry, err
}

type host struct {
	client *controller.Client
	cancel context.CancelFunc
	done   chan error
}

func startHost(ctx context.Context, root string, store *workflows.SQLiteStore, registry *workflows.ActionRegistry, lose bool) (*host, error) {
	owner, cancel := context.WithCancel(ctx)
	dir := filepath.Join(root, "engine")
	executor := &workflows.Executor{WorkspaceDir: root, Store: &admissionLoss{SQLiteStore: store, lose: lose}, ApplicationActions: registry}
	server := &controller.Server{Dir: dir, Store: store, Executor: executor, MaxRuns: 1, RunTimeout: 15 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := &host{client: controller.NewClient(dir), cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- server.Serve(owner) }()
	ready, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	for {
		if err := h.client.Call(ready, "GET", "/health", nil, nil); err == nil {
			return h, nil
		}
		select {
		case err := <-h.done:
			cancel()
			return nil, fmt.Errorf("controller startup: %w", err)
		case <-ready.Done():
			return nil, errors.Join(ready.Err(), h.close())
		case <-time.After(time.Millisecond):
		}
	}
}
func (h *host) close() error {
	h.cancel()
	h.client.HTTP.CloseIdleConnections()
	select {
	case err := <-h.done:
		return err
	case <-time.After(10 * time.Second):
		return errors.New("controller shutdown exceeded ten seconds")
	}
}
func waitRun(ctx context.Context, client *controller.Client, id string, generation int64) (workflows.Run, error) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		var run workflows.Run
		if err := client.Call(wait, "GET", "/runs/"+id, nil, &run); err != nil {
			return run, err
		}
		if run.Generation == generation && run.Status != "queued" && run.Status != "running" {
			return run, nil
		}
		select {
		case <-wait.Done():
			return run, fmt.Errorf("waiting for generation %d: %w", generation, wait.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func resumeRun(ctx context.Context, client *controller.Client, path string) (workflows.Run, error) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		var run workflows.Run
		err := client.Call(wait, "POST", path, nil, &run)
		if err == nil || err.Error() != "controller: run is already active; no matching resume request" {
			return run, err
		}
		// Terminal status can precede execution-owner release. Retry only this
		// admission rejection, keeping the request identity and acknowledgement.
		select {
		case <-wait.Done():
			return run, fmt.Errorf("waiting for execution owner release: %w", wait.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func runScenario(ctx context.Context, root, scenario string) (ev evidence, err error) {
	ev.Scenario = scenario
	if err = os.MkdirAll(filepath.Join(root, "workflows"), 0700); err != nil {
		return ev, err
	}
	if err = os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte("jobs: {mutate: {steps: [{id: increment, uses: action/increment}]}}\n"), 0600); err != nil {
		return ev, err
	}
	counterPath := filepath.Join(root, "external.sqlite")
	c, err := openCounter(counterPath)
	if err != nil {
		return ev, err
	}
	defer func() {
		if c != nil {
			err = errors.Join(err, c.db.Close())
		}
	}()
	store, err := workflows.OpenSQLite(filepath.Join(root, "engine"))
	if err != nil {
		return ev, err
	}
	defer func() {
		if store != nil {
			err = errors.Join(err, store.Close())
		}
	}()
	var validations atomic.Int32
	registry, err := register(c, store, scenario, true, &validations, counterPath)
	if err != nil {
		return ev, err
	}
	h, err := startHost(ctx, root, store, registry, scenario == "no_effect")
	if err != nil {
		return ev, err
	}
	defer func() {
		if h != nil {
			err = errors.Join(err, h.close())
		}
	}()
	var admitted workflows.Run
	if err = h.client.Call(ctx, "POST", "/runs", controller.StartRequest{RequestID: "original", Ref: "workflows/main.yml"}, &admitted); err != nil {
		return ev, err
	}
	ev.RunID = admitted.ID
	original, err := waitRun(ctx, h.client, admitted.ID, admitted.Generation)
	if err != nil {
		return ev, err
	}
	if original.Status != workflows.RunStatusNeedsAttention {
		return ev, fmt.Errorf("lost response status: %s", original.Status)
	}
	ev.OriginalCounter, err = c.count(ctx)
	if err != nil {
		return ev, err
	}
	if err = h.close(); err != nil {
		h = nil
		return ev, err
	}
	h = nil
	// Reopen both SQLite databases and reconstruct the embedding host. Recovery
	// cannot rely on the original action closure or an in-memory receipt.
	if err = store.Close(); err != nil {
		store = nil
		return ev, err
	}
	store = nil
	if err = c.db.Close(); err != nil {
		return ev, err
	}
	c, err = openCounter(counterPath)
	if err != nil {
		return ev, err
	}
	store, err = workflows.OpenSQLite(filepath.Join(root, "engine"))
	if err != nil {
		return ev, err
	}
	registry, err = register(c, store, scenario, false, &validations, counterPath)
	if err != nil {
		return ev, err
	}
	h, err = startHost(ctx, root, store, registry, false)
	if err != nil {
		return ev, err
	}
	retries := []bool{false}
	if scenario == "unknown" || scenario == "lookup_error" {
		retries = append(retries, true)
	}
	for i, retry := range retries {
		requestID := fmt.Sprintf("recover-%d", i+1)
		path := fmt.Sprintf("/runs/%s/resume?request_id=%s&retry_uncertain=%t", ev.RunID, requestID, retry)
		accepted, resumeErr := resumeRun(ctx, h.client, path)
		if resumeErr != nil {
			return ev, resumeErr
		}
		terminal, e := waitRun(ctx, h.client, ev.RunID, accepted.Generation)
		if e != nil {
			return ev, e
		}
		ev.Status = terminal.Status
		var duplicate workflows.Run
		if err = h.client.Call(ctx, "POST", path, nil, &duplicate); err != nil {
			return ev, err
		}
		var conflict workflows.Run
		changed := fmt.Sprintf("/runs/%s/resume?request_id=%s&retry_uncertain=%t", ev.RunID, requestID, !retry)
		conflictErr := h.client.Call(ctx, "POST", changed, nil, &conflict)
		if conflictErr == nil {
			return ev, errors.New("changed resume acknowledgement was accepted")
		}
		ev.Resumes = append(ev.Resumes, resumeEvidence{RequestID: requestID, RetryUncertain: retry, Generation: accepted.Generation, DuplicateGeneration: duplicate.Generation, AcceptedStatus: accepted.Status, TerminalStatus: terminal.Status, Conflict: conflictErr.Error()})
	}
	if err = h.close(); err != nil {
		h = nil
		return ev, err
	}
	h = nil
	ev.Counter, err = c.count(ctx)
	if err != nil {
		return ev, err
	}
	ev.Attempts, err = store.ActionAttempts(ctx, ev.RunID)
	if err != nil {
		return ev, err
	}
	ev.Events, err = store.Events(ctx, ev.RunID, 0)
	if err != nil {
		return ev, err
	}
	ev.CheckpointValidations = validations.Load()
	if err = checkEvidence(ev); err != nil {
		return ev, err
	}
	// Deliberately replay a lost-response operation in a separate fake database.
	// Keep the recovered scenario's counter intact for independent inspection.
	if scenario != "no_effect" {
		negative, openErr := openCounter(filepath.Join(root, "negative-control.sqlite"))
		if openErr != nil {
			return ev, openErr
		}
		defer func() { err = errors.Join(err, negative.db.Close()) }()
		if _, err = negative.increment(ctx, ev.Attempts[0].Context); err != nil {
			return ev, err
		}
		if _, err = negative.increment(ctx, ev.Attempts[0].Context); err != nil {
			return ev, err
		}
		bad := ev
		bad.Counter, err = negative.count(ctx)
		if err != nil {
			return ev, err
		}
		rejected := checkEvidence(bad)
		if rejected == nil {
			return ev, errors.New("negative control accepted a duplicate effect")
		}
		ev.NegativeControl = rejected.Error()
	}
	return ev, nil
}

func checkEvidence(ev evidence) error {
	if ev.Counter != 1 {
		return fmt.Errorf("external counter must stay exactly one, got %d", ev.Counter)
	}
	if ev.RunID == "" || len(ev.Attempts) == 0 || len(ev.Resumes) == 0 {
		return errors.New("missing retained identities")
	}
	original := ev.Attempts[0]
	if original.Context.RunID != ev.RunID || original.Context.OperationKey == "" || original.Context.AttemptID == "" {
		return errors.New("missing original operation identity")
	}
	for _, attempt := range ev.Attempts {
		if attempt.Context.RunID != ev.RunID || attempt.Context.AttemptID != attempt.Attempt.ID || attempt.Context.Attempt != attempt.Attempt.Number {
			return errors.New("action context differs from physical journal identity")
		}
	}
	for _, r := range ev.Resumes {
		if r.Generation <= original.Context.Generation || r.DuplicateGeneration != r.Generation || r.AcceptedStatus != workflows.RunStatusQueued || !strings.Contains(r.Conflict, "request_id reused with different content") {
			return errors.New("resume identity or acknowledgement was not preserved")
		}
		matches := 0
		for _, event := range ev.Events {
			if event.Type != "run.resume_admitted" {
				continue
			}
			var value struct {
				Generation int64  `json:"execution_generation"`
				RequestID  string `json:"request_id"`
				Retry      bool   `json:"retry_uncertain"`
			}
			if err := json.Unmarshal(event.Data, &value); err != nil {
				return err
			}
			if value.RequestID == r.RequestID {
				matches++
				if value.Generation != r.Generation || value.Retry != r.RetryUncertain {
					return errors.New("resume admission evidence differs from request")
				}
			}
		}
		if matches != 1 {
			return fmt.Errorf("resume %s had %d admissions", r.RequestID, matches)
		}
	}
	switch ev.Scenario {
	case "unknown", "lookup_error":
		if ev.Status != workflows.RunStatusNeedsAttention || ev.OriginalCounter != 1 || len(ev.Attempts) != 1 || original.State != "admitted" || len(original.Reconciliations) != 2 || len(ev.Resumes) != 2 || ev.Resumes[0].RetryUncertain || !ev.Resumes[1].RetryUncertain {
			return errors.New("uncertain action was replayed or its observations lost")
		}
		for i, observation := range original.Reconciliations {
			if observation.Generation != ev.Resumes[i].Generation || ev.Resumes[i].TerminalStatus != workflows.RunStatusNeedsAttention {
				return errors.New("reconciliation generation lost")
			}
			if ev.Scenario == "unknown" && observation.Outcome != "unknown" {
				return errors.New("unknown observation changed")
			}
			if ev.Scenario == "lookup_error" && !strings.Contains(observation.Error, "injected receipt lookup unavailable") {
				return errors.New("lookup error observation lost")
			}
		}
	case "completed":
		if ev.Status != workflows.RunStatusSucceeded || ev.OriginalCounter != 1 || len(ev.Attempts) != 1 || original.State != "completed" || len(original.Reconciliations) != 1 || original.Reconciliations[0].Outcome != "completed" || len(original.Reconciliations[0].Proof) == 0 || len(original.Result.Receipt) == 0 || ev.CheckpointValidations == 0 {
			return errors.New("recovered completion lacks validated evidence or replayed the action")
		}
		if len(ev.Resumes) != 1 || ev.Resumes[0].TerminalStatus != workflows.RunStatusSucceeded || original.Reconciliations[0].Generation != ev.Resumes[0].Generation {
			return errors.New("completion recovery generation lost")
		}
		type receiptProof struct {
			OperationKey string `json:"operation_key"`
			Receipt      int64  `json:"receipt"`
		}
		var proof, receipt receiptProof
		if err := json.Unmarshal(original.Reconciliations[0].Proof, &proof); err != nil {
			return err
		}
		if err := json.Unmarshal(original.Result.Receipt, &receipt); err != nil {
			return err
		}
		if proof.OperationKey != original.Context.OperationKey || proof.Receipt <= 0 || proof != receipt {
			return errors.New("completion proof belongs to a different operation or receipt")
		}
	case "no_effect":
		if ev.Status != workflows.RunStatusSucceeded || ev.OriginalCounter != 0 || len(ev.Attempts) != 2 || original.State != "no_effect" || len(original.Reconciliations) != 1 || original.Reconciliations[0].Outcome != "no_effect" || len(original.Reconciliations[0].Proof) == 0 {
			return errors.New("absence proof or new attempt missing")
		}
		next := ev.Attempts[1]
		if len(ev.Resumes) != 1 || ev.Resumes[0].TerminalStatus != workflows.RunStatusSucceeded || next.State != "completed" || next.Context.OperationKey != original.Context.OperationKey || next.Context.AttemptID == original.Context.AttemptID || next.Context.Attempt != original.Context.Attempt+1 || next.Context.Generation != ev.Resumes[0].Generation || next.Context.Generation <= original.Context.Generation || original.Reconciliations[0].Generation != next.Context.Generation {
			return errors.New("retry lost logical or physical identity")
		}
		var proof struct {
			OperationKey  string `json:"operation_key"`
			Counter       int    `json:"counter"`
			Authoritative bool   `json:"authoritative"`
		}
		if err := json.Unmarshal(original.Reconciliations[0].Proof, &proof); err != nil {
			return err
		}
		if proof.OperationKey != original.Context.OperationKey || proof.Counter != 0 || !proof.Authoritative {
			return errors.New("absence proof belongs to a different operation or lacks authority")
		}
		var proofSequence, admissionSequence int64
		for _, event := range ev.Events {
			if event.Type != "action.no_effect" && event.Type != "action.admitted" {
				continue
			}
			var attempt workflows.ActionAttempt
			if err := json.Unmarshal(event.Data, &attempt); err != nil {
				return err
			}
			if event.Type == "action.no_effect" && attempt.Context.AttemptID == original.Context.AttemptID {
				proofSequence = event.Sequence
			}
			if event.Type == "action.admitted" && attempt.Context.AttemptID == next.Context.AttemptID {
				admissionSequence = event.Sequence
			}
		}
		if proofSequence == 0 || admissionSequence <= proofSequence {
			return errors.New("retry admission preceded committed absence proof")
		}
	default:
		return errors.New("unsupported scenario")
	}
	return nil
}

func run(ctx context.Context, root string) ([]evidence, error) {
	var results []evidence
	for _, scenario := range []string{"unknown", "lookup_error", "completed", "no_effect"} {
		ev, err := runScenario(ctx, filepath.Join(root, scenario), scenario)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", scenario, err)
		}
		results = append(results, ev)
	}
	return results, nil
}
func main() {
	outputDir := flag.String("output", "", "retain databases and evidence in a new subdirectory here (default: disposable temporary storage)")
	flag.Parse()
	if err := command(*outputDir, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func command(outputDir string, w io.Writer) error {
	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0700); err != nil {
			return err
		}
	}
	root, err := os.MkdirTemp(outputDir, "receipt-recovery-")
	if err != nil {
		return err
	}
	if outputDir == "" {
		defer os.RemoveAll(root)
	} else {
		fmt.Fprintln(os.Stderr, "retained fixture:", root)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	results, err := run(ctx, root)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if outputDir != "" {
		if err = os.WriteFile(filepath.Join(root, "evidence.json"), append(data, '\n'), 0600); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
