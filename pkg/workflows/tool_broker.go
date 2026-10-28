package workflows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

var ErrUncertainTool = errors.New("external tool outcome is uncertain; inspect before explicit recovery")

func (s *SQLiteStore) CheckToolEffects(ctx context.Context, run string) error {
	var unresolved int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tool_calls c JOIN runs r ON r.id=c.run_id WHERE c.run_id=? AND c.generation=r.generation AND c.status IN ('dispatched','uncertain')`, run).Scan(&unresolved)
	if err != nil {
		return err
	}
	if unresolved > 0 {
		return ErrUncertainTool
	}
	return nil
}

func (s *SQLiteStore) RecoverToolCalls(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT record FROM tool_calls WHERE status='dispatched' AND server!='engine'")
	if err != nil {
		return err
	}
	records := []ToolCallRecord{}
	for rows.Next() {
		var raw []byte
		var record ToolCallRecord
		if err = rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		if err = decodeJSONWithNumbers(raw, &record); err != nil {
			_ = rows.Close()
			return err
		}
		records = append(records, record)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, record := range records {
		record.Status = "uncertain"
		record.FailureClass = "needs_attention"
		record.Error = "execution owner stopped after external tool dispatch; response is unknown"
		raw, _ := json.Marshal(record)
		if _, err = s.db.ExecContext(ctx, "UPDATE tool_calls SET status='uncertain',record=? WHERE run_id=? AND invocation_id=? AND call_id=? AND status='dispatched'", raw, record.RunID, record.InvocationID, record.CallID); err != nil {
			return err
		}
	}
	return nil
}

type ToolCallRecord struct {
	RunID               string          `json:"run_id"`
	Generation          int64           `json:"execution_generation"`
	InvocationID        string          `json:"invocation_id"`
	CallID              string          `json:"call_id"`
	Server              string          `json:"server"`
	Tool                string          `json:"tool"`
	ArgumentFingerprint string          `json:"argument_fingerprint"`
	AuditArguments      any             `json:"arguments"`
	Status              string          `json:"status"`
	Result              json.RawMessage `json:"result,omitempty"`
	FailureClass        string          `json:"failure_class,omitempty"`
	Error               string          `json:"error,omitempty"`
	AdmittedAt          time.Time       `json:"admitted_at"`
	FinishedAt          time.Time       `json:"finished_at,omitempty"`
}

type ExternalToolDispatcher func(context.Context, ToolDefinition, json.RawMessage) (json.RawMessage, error)
type Broker struct {
	Store    *SQLiteStore
	Workers  *WorkerManager
	External ExternalToolDispatcher
	mu       sync.Mutex
	flights  map[string]*toolFlight
}
type toolFlight struct {
	fingerprint string
	done        chan struct{}
	result      json.RawMessage
	err         error
}
type toolBinding struct {
	broker     *Broker
	invocation ToolInvocation
	catalog    []ToolDefinition
}

func (b *Broker) Bind(ctx context.Context, in ToolInvocation) (ToolBinding, error) {
	if in.RunID == "" || in.InvocationID == "" || in.Generation <= 0 {
		return nil, fmt.Errorf("broker requires a durable invocation owner")
	}
	if err := b.checkOwner(ctx, in); err != nil {
		return nil, err
	}
	catalog := make([]ToolDefinition, 0)
	for _, d := range in.Catalog {
		if grantSubset([]ToolGrant{{Server: d.Server, Tool: d.Name}}, in.Grants) {
			d.InputSchema = append(json.RawMessage(nil), d.InputSchema...)
			catalog = append(catalog, d)
		}
	}
	in.Grants = append([]ToolGrant(nil), in.Grants...)
	secrets := make(map[string]string, len(in.Secrets))
	for key, value := range in.Secrets {
		secrets[key] = value
	}
	in.Secrets = secrets
	return &toolBinding{broker: b, invocation: in, catalog: catalog}, nil
}
func (t *toolBinding) Catalog() []ToolDefinition {
	out := append([]ToolDefinition(nil), t.catalog...)
	for index := range out {
		out[index].InputSchema = append(json.RawMessage(nil), out[index].InputSchema...)
	}
	return out
}

func (b *Broker) checkOwner(ctx context.Context, in ToolInvocation) error {
	var count int
	err := b.Store.db.QueryRowContext(ctx, "SELECT count(*) FROM runs r WHERE id=? AND generation=? AND owner=? AND status='running' AND EXISTS(SELECT 1 FROM executions e WHERE e.run_id=r.id AND e.generation=r.generation AND e.finalizing=0)", in.RunID, in.Generation, in.Owner).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("tool invocation owner is no longer active")
	}
	if in.ParentWorkerID != "" {
		var parent Worker
		parent, err = b.Workers.Get(ctx, in.RunID, in.ParentWorkerID)
		if err != nil {
			return err
		}
		if parent.Status != "running" {
			return fmt.Errorf("parent worker is not running")
		}
	}
	return nil
}

func (t *toolBinding) Call(ctx context.Context, req ToolRequest) (json.RawMessage, error) {
	if req.CallID == "" || len(req.CallID) > 256 || len(req.Arguments) > 1<<20 {
		return nil, fmt.Errorf("invalid broker call identity or arguments exceed limit")
	}
	var definition *ToolDefinition
	for i := range t.catalog {
		d := &t.catalog[i]
		if d.Server == req.Server && d.Name == req.Tool {
			definition = d
			break
		}
	}
	var args any
	decodeErr := decodeJSONWithNumbers(req.Arguments, &args)
	var denied error
	failureClass := "schema"
	if decodeErr != nil {
		args = string(req.Arguments)
		denied = fmt.Errorf("invalid tool arguments")
	}
	if definition == nil {
		denied = fmt.Errorf("tool is outside exact server/tool grants")
		failureClass = "policy"
		definition = &ToolDefinition{Server: req.Server, Name: req.Tool}
	}
	if denied == nil {
		if _, ok := args.(map[string]any); !ok {
			denied = fmt.Errorf("tool arguments must be an object")
		} else {
			denied = validateToolArguments(*definition, args)
		}
	}
	canonical, _ := json.Marshal(args)
	h := sha256.Sum256(append([]byte(req.Server+"\x00"+req.Tool+"\x00"), canonical...))
	fingerprint := hex.EncodeToString(h[:])
	key := t.invocation.RunID + "\x00" + t.invocation.InvocationID + "\x00" + req.CallID
	b := t.broker
	b.mu.Lock()
	if b.flights == nil {
		b.flights = map[string]*toolFlight{}
	}
	if flight := b.flights[key]; flight != nil {
		b.mu.Unlock()
		if flight.fingerprint != fingerprint {
			return nil, fmt.Errorf("provider call ID reused with conflicting content")
		}
		select {
		case <-flight.done:
			return append(json.RawMessage(nil), flight.result...), flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &toolFlight{fingerprint: fingerprint, done: make(chan struct{})}
	b.flights[key] = flight
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.flights, key); close(flight.done); b.mu.Unlock() }()
	flight.result, flight.err = t.dispatch(ctx, req, *definition, canonical, args, fingerprint, denied, failureClass)
	return append(json.RawMessage(nil), flight.result...), flight.err
}

func validateToolArguments(definition ToolDefinition, args any) error {
	var schema jsonschema.Schema
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return fmt.Errorf("invalid pinned tool schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("unsupported pinned tool schema: %w", err)
	}
	validationArgs, err := toolValidationValue(args)
	if err != nil {
		return err
	}
	if err = resolved.Validate(validationArgs); err != nil {
		return fmt.Errorf("tool arguments violate pinned schema: %w", err)
	}
	return nil
}

// The validator treats json.Number as a string for type checks. Preserve exact
// numbers for fingerprints/dispatch, and validate a separate numeric view.
// Reject decimal values that cannot round-trip instead of silently accepting a
// different integer or boundary value after float64 rounding.
func toolValidationValue(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		if len(value) > 1024 {
			return nil, fmt.Errorf("numeric tool argument exceeds supported precision")
		}
		if index := strings.IndexAny(string(value), "eE"); index >= 0 {
			exponent, err := strconv.Atoi(string(value)[index+1:])
			if err != nil || exponent < -1024 || exponent > 1024 {
				return nil, fmt.Errorf("numeric tool argument exceeds supported precision")
			}
		}
		exact, ok := new(big.Rat).SetString(string(value))
		if !ok {
			return nil, fmt.Errorf("invalid numeric tool argument")
		}
		if exact.IsInt() && exact.Num().IsInt64() {
			return exact.Num().Int64(), nil
		}
		number, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return nil, fmt.Errorf("numeric tool argument exceeds supported precision")
		}
		encoded, err := json.Marshal(number)
		if err != nil {
			return nil, err
		}
		rounded, ok := new(big.Rat).SetString(string(encoded))
		if !ok || rounded.Cmp(exact) != 0 {
			return nil, fmt.Errorf("numeric tool argument exceeds supported precision")
		}
		return number, nil
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			converted, err := toolValidationValue(item)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case []any:
		out := make([]any, len(value))
		for index, item := range value {
			converted, err := toolValidationValue(item)
			if err != nil {
				return nil, err
			}
			out[index] = converted
		}
		return out, nil
	default:
		return value, nil
	}
}

func (t *toolBinding) dispatch(ctx context.Context, req ToolRequest, d ToolDefinition, canonical json.RawMessage, args any, fingerprint string, denied error, failureClass string) (json.RawMessage, error) {
	b := t.broker
	in := t.invocation
	if err := b.checkOwner(ctx, in); err != nil {
		return nil, err
	}
	record := ToolCallRecord{RunID: in.RunID, Generation: in.Generation, InvocationID: in.InvocationID, CallID: req.CallID, Server: req.Server, Tool: req.Tool, ArgumentFingerprint: fingerprint, AuditArguments: redactValue(args, in.Secrets), Status: "dispatched", AdmittedAt: time.Now().UTC()}
	record.Server = Redact(record.Server, in.Secrets)
	record.Tool = Redact(record.Tool, in.Secrets)
	if denied != nil {
		record.Status = "denied"
		record.FailureClass = failureClass
		record.Error = Redact(denied.Error(), in.Secrets)
		record.FinishedAt = time.Now().UTC()
	}
	tx, err := b.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var owned int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM runs r WHERE id=? AND generation=? AND owner=? AND status='running' AND EXISTS(SELECT 1 FROM executions e WHERE e.run_id=r.id AND e.generation=r.generation AND e.finalizing=0)", in.RunID, in.Generation, in.Owner).Scan(&owned); err != nil {
		return nil, err
	}
	if owned != 1 {
		return nil, fmt.Errorf("tool invocation execution ownership changed before dispatch")
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, "SELECT record FROM tool_calls WHERE run_id=? AND invocation_id=? AND call_id=?", in.RunID, in.InvocationID, req.CallID).Scan(&raw)
	if err == nil {
		var previous ToolCallRecord
		if err = decodeJSONWithNumbers(raw, &previous); err != nil {
			return nil, err
		}
		if previous.ArgumentFingerprint != fingerprint {
			return nil, fmt.Errorf("provider call ID reused with conflicting content")
		}
		if previous.Status == "denied" {
			return nil, errors.New(previous.Error)
		}
		if previous.Status == "completed" {
			return previous.Result, nil
		}
		if previous.Server != "engine" {
			return nil, ErrUncertainTool
		}
		if err = checkEffectAdmission(ctx, tx, in.RunID); err != nil {
			return nil, err
		}
		// Engine operations reconcile their deterministic child/message IDs; they
		// never reserve a child's grant a second time after a lost response.
		record = previous
	} else if errors.Is(err, sql.ErrNoRows) {
		if err = checkEffectAdmission(ctx, tx, in.RunID); err != nil {
			return nil, err
		}
		var uncertain int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM tool_calls WHERE run_id=? AND generation=? AND status='uncertain'", in.RunID, in.Generation).Scan(&uncertain); err != nil {
			return nil, err
		}
		if uncertain > 0 {
			return nil, ErrUncertainTool
		}
		raw, err = json.Marshal(record)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO tool_calls VALUES(?,?,?,?,?,?,?,?,?)", in.RunID, in.Generation, in.InvocationID, req.CallID, record.Server, record.Tool, fingerprint, record.Status, raw)
		if err != nil {
			return nil, err
		}
		if err = addEvent(ctx, tx, in.RunID, "tool."+record.Status, record); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if denied != nil {
		return nil, errors.New(record.Error)
	}
	var result json.RawMessage
	dispatch := in.Dispatch
	if dispatch == nil {
		dispatch = b.External
	}
	if req.Server == "engine" {
		result, err = t.engineCall(ctx, req, canonical)
	} else if dispatch == nil {
		err = fmt.Errorf("external tool dispatcher is unavailable")
	} else {
		result, err = dispatch(ctx, d, canonical)
	}
	if err != nil {
		record.Status = "uncertain"
		record.FailureClass = "needs_attention"
		record.Error = Redact(err.Error(), in.Secrets)
		if req.Server == "engine" {
			record.Status = "failed"
			record.FailureClass = classifyFailure(err)
		}
	} else {
		if len(result) > 16<<20 {
			err = fmt.Errorf("tool result exceeds limit")
			record.Status = "uncertain"
			record.FailureClass = "needs_attention"
			record.Error = err.Error()
		} else {
			var decoded any
			if decodeErr := decodeJSONWithNumbers(result, &decoded); decodeErr != nil {
				err = fmt.Errorf("invalid tool result")
				record.Status = "uncertain"
				record.FailureClass = "needs_attention"
				record.Error = err.Error()
			} else {
				record.Result, _ = json.Marshal(redactValue(decoded, in.Secrets))
				record.Status = "completed"
			}
		}
	}
	record.FinishedAt = time.Now().UTC()
	raw, marshalErr := json.Marshal(record)
	if marshalErr != nil {
		return nil, marshalErr
	}
	finishCtx := context.WithoutCancel(ctx)
	tx, saveErr := b.Store.db.BeginTx(finishCtx, nil)
	if saveErr != nil {
		return nil, errors.Join(ErrUncertainTool, saveErr)
	}
	defer tx.Rollback()
	_, saveErr = tx.ExecContext(finishCtx, "UPDATE tool_calls SET status=?,record=? WHERE run_id=? AND invocation_id=? AND call_id=? AND fingerprint=?", record.Status, raw, in.RunID, in.InvocationID, req.CallID, fingerprint)
	if saveErr == nil {
		saveErr = addEvent(finishCtx, tx, in.RunID, "tool."+record.Status, record)
	}
	if saveErr == nil {
		saveErr = tx.Commit()
	}
	if saveErr != nil {
		return nil, errors.Join(ErrUncertainTool, saveErr)
	}
	if err != nil {
		if req.Server != "engine" {
			return nil, errors.Join(ErrUncertainTool, errors.New(record.Error))
		}
		return nil, errors.New(record.Error)
	}
	return record.Result, nil
}

// ToolCalls exposes unresolved effects without replaying them. Inspection is
// bounded and callers can retrieve later pages with the ordinary call identity.
func (s *SQLiteStore) ToolCalls(ctx context.Context, run string, unresolved bool) ([]ToolCallRecord, error) {
	query := "SELECT record FROM tool_calls WHERE run_id=?"
	if unresolved {
		query += " AND status IN ('dispatched','uncertain','failed')"
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY rowid LIMIT 1000", run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolCallRecord{}
	for rows.Next() {
		var raw []byte
		var record ToolCallRecord
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(raw, &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func EngineTools() []ToolDefinition {
	object := func(properties string, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` + properties + `},"required":[` + required + `]}`)
	}
	worker := `"worker_id":{"type":"string","minLength":1}`
	return []ToolDefinition{
		{Server: "engine", Name: "worker_spawn", Description: "Create a child worker under the current invocation's owner. A supplied initial prompt is durably enqueued.", InputSchema: object(`"role":{"type":"string","minLength":1},"turns":{"type":"integer","minimum":1},"prompt":{"type":"string","minLength":1}`, `"role"`)},
		{Server: "engine", Name: "worker_send", Description: "Durably enqueue a message to an owned child worker.", InputSchema: object(worker+`,"prompt":{"type":"string","minLength":1}`, `"worker_id","prompt"`)},
		{Server: "engine", Name: "worker_status", Description: "Inspect an owned child worker.", InputSchema: object(worker, `"worker_id"`)},
		{Server: "engine", Name: "worker_wait", Description: "Wait for an owned child's durable message result. Cancellation stops this wait.", InputSchema: object(worker+`,"message_id":{"type":"string","minLength":1}`, `"worker_id","message_id"`)},
		{Server: "engine", Name: "worker_interrupt", Description: "Interrupt an owned child's current turn.", InputSchema: object(worker, `"worker_id"`)},
		{Server: "engine", Name: "worker_close", Description: "Close an owned child and cancel its current turn.", InputSchema: object(worker, `"worker_id"`)},
	}
}

func (t *toolBinding) engineCall(ctx context.Context, req ToolRequest, args json.RawMessage) (json.RawMessage, error) {
	m := t.broker.Workers
	if m == nil {
		return nil, fmt.Errorf("worker tools unavailable")
	}
	var input struct {
		Role      string `json:"role"`
		Turns     int    `json:"turns"`
		Prompt    string `json:"prompt"`
		WorkerID  string `json:"worker_id"`
		MessageID string `json:"message_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	in := t.invocation
	h := sha256.Sum256([]byte(in.RunID + "\x00" + in.InvocationID + "\x00" + req.CallID))
	stable := hex.EncodeToString(h[:16])
	if req.Tool == "worker_spawn" {
		spec := WorkerSpec{RunID: in.RunID, ParentID: in.ParentWorkerID, Role: input.Role, Turns: input.Turns, StableID: stable, CreatorInvocation: in.InvocationID, Owner: in.Owner}
		if in.ParentWorkerID == "" {
			spec.Boundary = &in
		}
		worker, err := m.Spawn(ctx, spec)
		if err != nil {
			return nil, err
		}

		out := map[string]any{"worker": worker}
		if input.Prompt != "" {
			message, err := m.Send(ctx, in.RunID, worker.ID, stable+"-initial", input.Prompt)
			if err != nil {
				return nil, err
			}
			out["message"] = message
		}
		return json.Marshal(out)
	}
	worker, err := m.Get(ctx, in.RunID, input.WorkerID)
	if err != nil {
		return nil, err
	}
	if in.ParentWorkerID != "" && worker.ParentID != in.ParentWorkerID && worker.ID != in.ParentWorkerID {
		return nil, fmt.Errorf("worker is outside invocation parent ownership")
	}
	// A workflow invocation may control only its own deterministic children.
	if in.ParentWorkerID == "" && (worker.ParentID != "" || worker.CreatorInvocation != in.InvocationID) {
		return nil, fmt.Errorf("nested worker belongs to another parent")
	}
	switch req.Tool {
	case "worker_send":
		message, err := m.Send(ctx, in.RunID, worker.ID, stable, input.Prompt)
		if err != nil {
			return nil, err
		}
		return json.Marshal(message)
	case "worker_status":
		return json.Marshal(worker)
	case "worker_wait":
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			var raw []byte
			if err = m.Store.db.QueryRowContext(ctx, "SELECT record FROM messages WHERE id=? AND worker_id=?", input.MessageID, worker.ID).Scan(&raw); err != nil {
				return nil, err
			}
			var message WorkerMessage
			if err = decodeJSONWithNumbers(raw, &message); err != nil {
				return nil, err
			}
			if message.Status != RunStatusQueued && message.Status != RunStatusRunning {
				return raw, nil
			}
			if err = t.broker.checkOwner(ctx, in); err != nil {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
		}
	case "worker_interrupt", "worker_close":
		if err = m.Interrupt(ctx, in.RunID, worker.ID, req.Tool == "worker_close"); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"id": worker.ID, "operation": req.Tool})
	default:
		return nil, fmt.Errorf("unknown engine tool")
	}
}
