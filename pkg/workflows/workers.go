package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dkropachev/agent-workflow/pkg/process"
	"sync"
	"time"
)

type WorkerAccess struct {
	Container bool   `json:"container"`
	Writable  bool   `json:"writable"`
	Workspace string `json:"workspace,omitempty"`
}

type Worker struct {
	Access       WorkerAccess  `json:"access"`
	MaxQueued    int           `json:"max_queued"`
	MaxDepth     int           `json:"max_depth"`
	TurnDuration time.Duration `json:"turn_duration"`
	MaxOutput    int           `json:"max_output"`
	ID           string        `json:"id"`
	RunID        string        `json:"run_id"`
	ParentID     string        `json:"parent_id,omitempty"`
	Role         string        `json:"role"`
	Status       string        `json:"status"`
	SessionID    string        `json:"session_id,omitempty"`
	Tools        []string      `json:"tools"`
	Remaining    int           `json:"remaining_turns"`
	Depth        int           `json:"depth"`
	Error        string        `json:"error,omitempty"`
}
type WorkerMessage struct {
	QueueTimeMS int64         `json:"queue_time_ms,omitempty"`
	ID          string        `json:"id"`
	WorkerID    string        `json:"worker_id"`
	RequestID   string        `json:"request_id"`
	Sequence    int           `json:"sequence"`
	Status      string        `json:"status"`
	Prompt      string        `json:"prompt"`
	Result      SessionResult `json:"result,omitempty"`
	Error       string        `json:"error,omitempty"`
	EnqueuedAt  time.Time     `json:"enqueued_at"`
	StartedAt   time.Time     `json:"started_at,omitempty"`
	FinishedAt  time.Time     `json:"finished_at,omitempty"`
}
type WorkerSpec struct {
	RunID    string   `json:"run_id"`
	ParentID string   `json:"parent_id,omitempty"`
	Role     string   `json:"role"`
	Tools    []string `json:"tools,omitempty"`
	Turns    int      `json:"turns,omitempty"`
}
type WorkerLimits struct {
	MaxWorkers   int
	MaxDepth     int
	MaxQueued    int
	MaxTurns     int
	TurnDuration time.Duration
	MaxOutput    int
}

func DefaultWorkerLimits() WorkerLimits {
	return WorkerLimits{MaxWorkers: 64, MaxDepth: 8, MaxQueued: 64, MaxTurns: 32, TurnDuration: 30 * time.Minute, MaxOutput: 16 << 20}
}

// WorkerFactory resolves the admitted role snapshot and credentials, then applies
// the requested tool subset. It must reject policies the backend cannot enforce.
type WorkerFactory func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error)
type WorkerManager struct {
	Store      *SQLiteStore
	Factory    WorkerFactory
	Slots      chan struct{}
	Limits     WorkerLimits
	mu         sync.Mutex
	active     map[string]context.CancelFunc
	activeRuns map[string]string
	wg         sync.WaitGroup
}

func (m *WorkerManager) Init(ctx context.Context) error {
	if m.Factory == nil {
		return fmt.Errorf("worker factory is required")
	}
	defaults := DefaultWorkerLimits()
	if m.Limits.MaxWorkers == 0 {
		m.Limits.MaxWorkers = defaults.MaxWorkers
	}
	if m.Limits.MaxDepth == 0 {
		m.Limits.MaxDepth = defaults.MaxDepth
	}
	if m.Limits.MaxQueued == 0 {
		m.Limits.MaxQueued = defaults.MaxQueued
	}
	if m.Limits.MaxTurns == 0 {
		m.Limits.MaxTurns = defaults.MaxTurns
	}
	if m.Limits.TurnDuration == 0 {
		m.Limits.TurnDuration = defaults.TurnDuration
	}
	if m.Limits.MaxOutput == 0 {
		m.Limits.MaxOutput = defaults.MaxOutput
	}
	if m.Limits.MaxWorkers < 1 || m.Limits.MaxDepth < 1 || m.Limits.MaxQueued < 1 || m.Limits.MaxTurns < 1 || m.Limits.TurnDuration < 0 || m.Limits.MaxOutput < 1 {
		return fmt.Errorf("worker limits must be positive")
	}
	if m.Slots != nil && cap(m.Slots) == 0 {
		return fmt.Errorf("worker execution capacity must be positive")
	}
	if m.Slots == nil {
		m.Slots = make(chan struct{}, 4)
	}
	m.active = map[string]context.CancelFunc{}
	m.activeRuns = map[string]string{}
	_, err := m.Store.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workers(id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,parent_id TEXT NOT NULL,role TEXT NOT NULL,status TEXT NOT NULL,record BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS messages(id TEXT PRIMARY KEY,worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,request_id TEXT NOT NULL,sequence INTEGER NOT NULL,status TEXT NOT NULL,record BLOB NOT NULL,UNIQUE(worker_id,request_id),UNIQUE(worker_id,sequence));`)
	if err != nil {
		return err
	}
	// Acknowledged queued messages remain queued. A turn interrupted by owner loss
	// requires explicit acknowledgement before later mailbox entries can execute.
	workers, err := m.List(ctx, "")
	if err != nil {
		return err
	}
	for _, w := range workers {
		if w.Status == RunStatusRunning {
			w.Status = RunStatusNeedsAttention
			w.Error = "owner stopped during provider turn"
			if err = m.saveWorker(ctx, w); err != nil {
				return err
			}
		}
	}
	return nil
}
func (m *WorkerManager) saveWorker(ctx context.Context, w Worker) error {
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = m.Store.db.ExecContext(ctx, "UPDATE workers SET status=?,record=? WHERE id=?", w.Status, b, w.ID)
	return err
}
func (m *WorkerManager) Get(ctx context.Context, run, id string) (Worker, error) {
	var b []byte
	var w Worker
	err := m.Store.db.QueryRowContext(ctx, "SELECT record FROM workers WHERE id=? AND run_id=?", id, run).Scan(&b)
	if err != nil {
		return w, err
	}
	err = decodeJSONWithNumbers(b, &w)
	return w, err
}
func (m *WorkerManager) List(ctx context.Context, run string) ([]Worker, error) {
	rows, err := m.Store.db.QueryContext(ctx, "SELECT record FROM workers WHERE run_id=? OR ?='' ORDER BY rowid", run, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Worker{}
	for rows.Next() {
		var b []byte
		var w Worker
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(b, &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func subset(child, parent []string) bool {
	set := map[string]bool{}
	for _, p := range parent {
		set[p] = true
	}
	for _, c := range child {
		if !set[c] {
			return false
		}
	}
	return true
}
func (m *WorkerManager) Spawn(ctx context.Context, spec WorkerSpec) (Worker, error) {
	run, snapshot, err := m.Store.Load(ctx, spec.RunID)
	if err != nil {
		return Worker{}, err
	}
	if origin := nativeString(snapshot.Metadata, "origin_run_id"); origin != "" && origin != spec.RunID {
		return Worker{}, fmt.Errorf("workers belong to originating run %s", origin)
	}
	if run.Status != RunStatusRunning {
		return Worker{}, fmt.Errorf("workers require an active originating run")
	}
	runner, tools, _, err := m.Factory(ctx, spec.RunID, spec.Role, spec.Tools)
	if err != nil {
		return Worker{}, err
	}
	caps, err := runner.Capabilities(spec.Role)
	if err != nil {
		return Worker{}, err
	}
	if !caps.Sessions || !caps.Resume {
		return Worker{}, fmt.Errorf("role backend does not support durable sessions")
	}
	if !caps.ManagedPolicy {
		return Worker{}, fmt.Errorf("role backend cannot enforce engine-owned worker policy")
	}
	access := WorkerAccess{Writable: true}
	if source, ok := runner.(interface {
		WorkerAccess(string) (WorkerAccess, error)
	}); ok {
		access, err = source.WorkerAccess(spec.Role)
		if err != nil {
			return Worker{}, err
		}
	}
	policy := m.Limits
	if source, ok := runner.(interface {
		WorkerLimits(string) (WorkerLimits, error)
	}); ok {
		roleLimits, err := source.WorkerLimits(spec.Role)
		if err != nil {
			return Worker{}, err
		}
		policy = capWorkerLimits(policy, roleLimits)
	}
	turns := spec.Turns
	if turns == 0 {
		turns = policy.MaxTurns
	}
	if turns < 1 || turns > policy.MaxTurns {
		return Worker{}, fmt.Errorf("worker turn budget exceeds policy")
	}
	tx, err := m.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return Worker{}, err
	}
	defer tx.Rollback()
	var total int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM workers WHERE run_id=?", spec.RunID).Scan(&total); err != nil {
		return Worker{}, err
	}
	if total >= m.Limits.MaxWorkers {
		return Worker{}, fmt.Errorf("worker limit exhausted")
	}
	var roleTotal int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM workers WHERE run_id=? AND role=?", spec.RunID, spec.Role).Scan(&roleTotal); err != nil {
		return Worker{}, err
	}
	if roleTotal >= policy.MaxWorkers {
		return Worker{}, fmt.Errorf("role worker limit exhausted")
	}
	depth := 0
	if spec.ParentID != "" {
		var b []byte
		var parent Worker
		if err = tx.QueryRowContext(ctx, "SELECT record FROM workers WHERE id=? AND run_id=?", spec.ParentID, spec.RunID).Scan(&b); err != nil {
			return Worker{}, fmt.Errorf("parent is outside originating run")
		}
		if err = decodeJSONWithNumbers(b, &parent); err != nil {
			return Worker{}, err
		}
		if parent.Status == "closed" {
			return Worker{}, fmt.Errorf("parent is closed")
		}
		if parent.Access.Container && (!access.Container || (!parent.Access.Writable && access.Writable) || parent.Access.Workspace != access.Workspace) {
			return Worker{}, fmt.Errorf("child execution boundary exceeds parent policy")
		}
		depth = parent.Depth + 1
		policy = capWorkerLimits(policy, WorkerLimits{MaxDepth: parent.MaxDepth, MaxQueued: parent.MaxQueued, TurnDuration: parent.TurnDuration, MaxOutput: parent.MaxOutput})
		if depth > policy.MaxDepth || !subset(tools, parent.Tools) || turns > parent.Remaining {
			return Worker{}, fmt.Errorf("child exceeds parent permissions, depth, or remaining budget")
		}
		parent.Remaining -= turns
		b, _ = json.Marshal(parent)
		if _, err = tx.ExecContext(ctx, "UPDATE workers SET record=? WHERE id=?", b, parent.ID); err != nil {
			return Worker{}, err
		}
	}
	w := Worker{Access: access, MaxQueued: policy.MaxQueued, MaxDepth: policy.MaxDepth, TurnDuration: policy.TurnDuration, MaxOutput: policy.MaxOutput, ID: randomID(), RunID: spec.RunID, ParentID: spec.ParentID, Role: spec.Role, Status: "idle", Tools: tools, Remaining: turns, Depth: depth}
	b, _ := json.Marshal(w)
	if _, err = tx.ExecContext(ctx, "INSERT INTO workers VALUES(?,?,?,?,?,?)", w.ID, w.RunID, w.ParentID, w.Role, w.Status, b); err != nil {
		return w, err
	}
	if err = addEvent(ctx, tx, w.RunID, "worker.spawned", w); err != nil {
		return w, err
	}
	return w, tx.Commit()
}
func (m *WorkerManager) Send(ctx context.Context, run, id, requestID, prompt string) (WorkerMessage, error) {
	if requestID == "" || len(requestID) > 128 || len(prompt) > 1<<20 {
		return WorkerMessage{}, fmt.Errorf("request_id required (max 128 bytes); prompt limit is 1 MiB")
	}
	w, err := m.Get(ctx, run, id)
	if err != nil {
		return WorkerMessage{}, err
	}
	_, _, secrets, err := m.Factory(ctx, run, w.Role, w.Tools)
	if err != nil {
		return WorkerMessage{}, err
	}
	if Redact(prompt, secrets) != prompt {
		return WorkerMessage{}, fmt.Errorf("mailbox prompts must use credential references, not secret values")
	}
	tx, err := m.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkerMessage{}, err
	}
	defer tx.Rollback()
	var old []byte
	err = tx.QueryRowContext(ctx, "SELECT record FROM messages WHERE worker_id=? AND request_id=?", id, requestID).Scan(&old)
	if err == nil {
		var msg WorkerMessage
		if err = decodeJSONWithNumbers(old, &msg); err != nil {
			return msg, err
		}
		if msg.Prompt != prompt {
			return msg, fmt.Errorf("request_id reused with different content")
		}
		return msg, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkerMessage{}, err
	}
	var status string
	if err = tx.QueryRowContext(ctx, "SELECT status FROM workers WHERE id=? AND run_id=?", id, run).Scan(&status); err != nil {
		return WorkerMessage{}, err
	}
	if status == "closed" {
		return WorkerMessage{}, fmt.Errorf("worker is closed")
	}
	var queued, sequence int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE worker_id=? AND status='queued'", id).Scan(&queued); err != nil {
		return WorkerMessage{}, err
	}
	queueLimit := w.MaxQueued
	if queueLimit == 0 {
		queueLimit = m.Limits.MaxQueued
	}
	if queued >= queueLimit {
		return WorkerMessage{}, fmt.Errorf("mailbox limit exhausted")
	}
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(sequence),0)+1 FROM messages WHERE worker_id=?", id).Scan(&sequence); err != nil {
		return WorkerMessage{}, err
	}
	msg := WorkerMessage{ID: randomID(), WorkerID: id, RequestID: requestID, Sequence: sequence, Status: RunStatusQueued, Prompt: prompt, EnqueuedAt: time.Now().UTC()}
	b, _ := json.Marshal(msg)
	if _, err = tx.ExecContext(ctx, "INSERT INTO messages VALUES(?,?,?,?,?,?)", msg.ID, id, requestID, sequence, msg.Status, b); err != nil {
		return msg, err
	}
	if err = addEvent(ctx, tx, run, "worker.message_enqueued", map[string]any{"worker_id": id, "message_id": msg.ID, "sequence": sequence}); err != nil {
		return msg, err
	}
	return msg, tx.Commit()
}
func (m *WorkerManager) Messages(ctx context.Context, run, id string) ([]WorkerMessage, error) {
	if _, err := m.Get(ctx, run, id); err != nil {
		return nil, err
	}
	rows, err := m.Store.db.QueryContext(ctx, "SELECT record FROM messages WHERE worker_id=? ORDER BY sequence", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerMessage{}
	for rows.Next() {
		var b []byte
		var msg WorkerMessage
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(b, &msg); err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}
func (m *WorkerManager) Serve(ctx context.Context) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	defer func() {
		m.mu.Lock()
		for _, cancel := range m.active {
			cancel()
		}
		m.mu.Unlock()
		m.wg.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		workers, err := m.List(ctx, "")
		if err != nil {
			continue
		}
		for _, w := range workers {
			if w.Status != "idle" {
				continue
			}
			m.mu.Lock()
			_, active := m.active[w.ID]
			if active {
				m.mu.Unlock()
				continue
			}
			duration := w.TurnDuration
			if duration <= 0 {
				duration = m.Limits.TurnDuration
			}
			turnCtx, cancel := context.WithTimeout(ctx, duration)
			m.active[w.ID] = cancel
			m.activeRuns[w.ID] = w.RunID
			m.wg.Add(1)
			m.mu.Unlock()
			go func(w Worker) {
				defer m.wg.Done()
				defer cancel()
				defer func() { m.mu.Lock(); delete(m.active, w.ID); delete(m.activeRuns, w.ID); m.mu.Unlock() }()
				m.turn(turnCtx, w)
			}(w)
		}
	}
}
func (m *WorkerManager) turn(ctx context.Context, w Worker) {
	ctx, cancelTurn := context.WithCancel(ctx)
	defer cancelTurn()
	var progressErr error
	messages, err := m.Messages(ctx, w.RunID, w.ID)
	if err != nil {
		return
	}
	var msg WorkerMessage
	found := false
	for _, candidate := range messages {
		if candidate.Status == RunStatusRunning {
			return
		}
		if candidate.Status == RunStatusQueued {
			msg = candidate
			found = true
			break
		}
	}
	if !found {
		return
	}
	select {
	case m.Slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-m.Slots }()
	runner, _, secrets, err := m.Factory(ctx, w.RunID, w.Role, w.Tools)
	if err != nil {
		w.Status = RunStatusNeedsAttention
		w.Error = Redact(err.Error(), secrets)
		_ = m.saveWorker(context.WithoutCancel(ctx), w)
		return
	}
	tx, err := m.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	var b []byte
	err = tx.QueryRowContext(ctx, "SELECT record FROM workers WHERE id=? AND status='idle'", w.ID).Scan(&b)
	if err != nil {
		_ = tx.Rollback()
		return
	}
	if err = decodeJSONWithNumbers(b, &w); err != nil {
		_ = tx.Rollback()
		return
	}
	if w.Remaining <= 0 {
		_ = tx.Rollback()
		w.Status = RunStatusNeedsAttention
		w.Error = "worker turn budget exhausted"
		_ = m.saveWorker(context.WithoutCancel(ctx), w)
		return
	}
	var runStatus string
	if err = tx.QueryRowContext(ctx, "SELECT status FROM runs WHERE id=?", w.RunID).Scan(&runStatus); err != nil || runStatus != RunStatusRunning {
		_ = tx.Rollback()
		return
	}
	w.Remaining--
	w.Status = RunStatusRunning
	msg.Status = RunStatusRunning
	msg.StartedAt = time.Now().UTC()
	msg.QueueTimeMS = msg.StartedAt.Sub(msg.EnqueuedAt).Milliseconds()
	b, _ = json.Marshal(w)
	_, err = tx.ExecContext(ctx, "UPDATE workers SET status=?,record=? WHERE id=?", w.Status, b, w.ID)
	if err == nil {
		b, _ = json.Marshal(msg)
		_, err = tx.ExecContext(ctx, "UPDATE messages SET status=?,record=? WHERE id=? AND status='queued'", msg.Status, b, msg.ID)
	}
	if err == nil {
		err = addEvent(ctx, tx, w.RunID, "worker.turn_admitted", map[string]string{"worker_id": w.ID, "message_id": msg.ID})
	}
	if err != nil {
		_ = tx.Rollback()
		return
	}
	if err = tx.Commit(); err != nil {
		return
	}
	var eventMu sync.Mutex
	callbacksClosed := false
	expectedSession := w.SessionID
	result, turnErr := runner.RunTurn(ctx, SessionRequest{AgentRequest: AgentRequest{RunID: w.RunID, InvocationID: msg.ID, AgentID: w.Role, Prompt: msg.Prompt}, SessionID: w.SessionID}, func(event ProviderEvent) {
		eventMu.Lock()
		defer eventMu.Unlock()
		if callbacksClosed {
			return
		}
		if event.SessionID != "" {
			if expectedSession != "" && event.SessionID != expectedSession {
				progressErr = fmt.Errorf("provider changed session identity during a turn")
				cancelTurn()
				return
			}
			expectedSession = event.SessionID
		}

		event.Data, _ = redactValue(event.Data, secrets).(map[string]any)
		eventCtx := context.WithoutCancel(ctx)
		tx, err := m.Store.db.BeginTx(eventCtx, nil)
		if err != nil {
			progressErr = err
			cancelTurn()
			return
		}
		defer tx.Rollback()
		var b []byte
		var current Worker
		if err = tx.QueryRowContext(eventCtx, "SELECT record FROM workers WHERE id=?", w.ID).Scan(&b); err == nil {
			err = decodeJSONWithNumbers(b, &current)
		}
		if err == nil && event.SessionID != "" {
			current.SessionID = event.SessionID
			w.SessionID = event.SessionID
			b, _ = json.Marshal(current)
			_, err = tx.ExecContext(eventCtx, "UPDATE workers SET record=? WHERE id=?", b, w.ID)
		}
		if err == nil {
			err = addEvent(eventCtx, tx, w.RunID, "worker.progress", map[string]any{"worker_id": w.ID, "event": event})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			progressErr = err
			cancelTurn()
		}

	})
	eventMu.Lock()
	callbacksClosed = true
	eventMu.Unlock()
	if progressErr != nil {
		turnErr = progressErr
	}
	if result.SessionID != "" {
		if expectedSession != "" && result.SessionID != expectedSession {
			turnErr = fmt.Errorf("provider returned an unexpected resumed session identity")
		} else {
			w.SessionID = result.SessionID
		}
	}
	result.Outputs, _ = redactValue(result.Outputs, secrets).(map[string]any)
	encoded, _ := json.Marshal(result)
	outputLimit := w.MaxOutput
	if outputLimit == 0 {
		outputLimit = m.Limits.MaxOutput
	}
	if len(encoded) > outputLimit {
		result.Outputs = nil
		turnErr = fmt.Errorf("worker output exceeds limit")
	}
	msg.Result = result
	msg.FinishedAt = time.Now().UTC()
	msg.Status = RunStatusSucceeded
	if turnErr != nil || result.Status != RunStatusSucceeded {
		msg.Status = RunStatusFailed
		msg.Error = Redact(fmt.Sprint(turnErr), secrets)
	}
	if ctx.Err() != nil {
		msg.Status = "interrupted"
		msg.Error = ctx.Err().Error()
	}
	w.Status = "idle"
	w.Error = msg.Error
	tx, err = m.Store.db.BeginTx(context.Background(), nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	var current Worker
	if err = tx.QueryRow("SELECT record FROM workers WHERE id=?", w.ID).Scan(&b); err != nil {
		return
	}
	if err = decodeJSONWithNumbers(b, &current); err != nil {
		return
	}
	current.SessionID = w.SessionID
	current.Error = w.Error
	if current.Status != "closed" {
		current.Status = "idle"
		if progressErr != nil || errors.Is(turnErr, process.ErrCleanup) || (turnErr != nil && ctx.Err() == nil) {
			current.Status = RunStatusNeedsAttention
		}
	}
	w = current
	b, _ = json.Marshal(w)
	if _, err = tx.Exec("UPDATE workers SET status=?,record=? WHERE id=?", w.Status, b, w.ID); err != nil {
		return
	}
	b, _ = json.Marshal(msg)
	if _, err = tx.Exec("UPDATE messages SET status=?,record=? WHERE id=?", msg.Status, b, msg.ID); err != nil {
		return
	}
	if err = addEvent(context.Background(), tx, w.RunID, "worker.turn_completed", msg); err != nil {
		return
	}
	_ = tx.Commit()
}
func (m *WorkerManager) Interrupt(ctx context.Context, run, id string, closeWorker bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Execution cancellation must work even when storage is unavailable.
	if m.activeRuns[id] == run {
		if cancel := m.active[id]; cancel != nil {
			cancel()
		}
	}
	w, err := m.Get(ctx, run, id)
	if err != nil {
		return err
	}
	if closeWorker {
		w.Status = "closed"
		if err = m.saveWorker(ctx, w); err != nil {
			return err
		}
	}
	return nil
}
func (m *WorkerManager) CancelRun(ctx context.Context, run string) error {
	m.mu.Lock()
	for id, origin := range m.activeRuns {
		if origin == run {
			m.active[id]()
		}
	}
	m.mu.Unlock()
	workers, err := m.List(ctx, run)
	if err != nil {
		return err
	}
	for _, w := range workers {
		if err = m.Interrupt(ctx, run, w.ID, true); err != nil {
			return err
		}
	}
	return nil
}

func (m *WorkerManager) Resume(ctx context.Context, run, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, active := m.active[id]; active {
		return fmt.Errorf("worker is active")
	}
	w, err := m.Get(ctx, run, id)
	if err != nil {
		return err
	}
	if w.Status != RunStatusNeedsAttention {
		return fmt.Errorf("worker does not need recovery")
	}
	messages, err := m.Messages(ctx, run, id)
	if err != nil {
		return err
	}
	tx, err := m.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, msg := range messages {
		if msg.Status == RunStatusRunning {
			msg.Status = "interrupted"
			msg.Error = "owner stopped; explicitly acknowledged without replaying this turn"
			b, _ := json.Marshal(msg)
			if _, err = tx.ExecContext(ctx, "UPDATE messages SET status=?,record=? WHERE id=?", msg.Status, b, msg.ID); err != nil {
				return err
			}
		}
	}
	w.Status = "idle"
	w.Error = ""
	b, _ := json.Marshal(w)
	if _, err = tx.ExecContext(ctx, "UPDATE workers SET status=?,record=? WHERE id=?", w.Status, b, w.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func capWorkerLimits(parent, child WorkerLimits) WorkerLimits {
	min := func(a, b int) int {
		if b > 0 && b < a {
			return b
		}
		return a
	}
	parent.MaxWorkers = min(parent.MaxWorkers, child.MaxWorkers)
	parent.MaxDepth = min(parent.MaxDepth, child.MaxDepth)
	parent.MaxQueued = min(parent.MaxQueued, child.MaxQueued)
	parent.MaxTurns = min(parent.MaxTurns, child.MaxTurns)
	parent.MaxOutput = min(parent.MaxOutput, child.MaxOutput)
	if child.TurnDuration > 0 && child.TurnDuration < parent.TurnDuration {
		parent.TurnDuration = child.TurnDuration
	}
	return parent
}
