package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type UsageIdentity struct {
	ID            string `json:"invocation_id"`
	RunID         string `json:"origin_run_id"`
	TaskAttemptID string `json:"task_attempt_id,omitempty"`
	WorkerID      string `json:"worker_id,omitempty"`
	Kind          string `json:"kind"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	SessionID     string `json:"session_id,omitempty"`
	Finished      bool   `json:"finished"`
}
type UsageUpdate struct {
	Key                        string
	Provider, Model, SessionID string
	Usage                      map[string]any
	CostUSD                    any
	Final                      bool
	Incomplete                 bool
}
type UsageObservation struct {
	DeltaInputTokens       *int64    `json:"delta_input_tokens,omitempty"`
	DeltaCachedInputTokens *int64    `json:"delta_cached_input_tokens,omitempty"`
	DeltaOutputTokens      *int64    `json:"delta_output_tokens,omitempty"`
	CounterReset           bool      `json:"counter_reset,omitempty"`
	InputTokens            *int64    `json:"input_tokens"`
	CachedInputTokens      *int64    `json:"cached_input_tokens"`
	OutputTokens           *int64    `json:"output_tokens"`
	CostUSD                *float64  `json:"cost_usd"`
	Scope                  string    `json:"counter_scope"`
	Final                  bool      `json:"final"`
	Complete               bool      `json:"complete"`
	ObservedAt             time.Time `json:"observed_at"`
}
type UsageSummary struct {
	Overflow              bool     `json:"overflow,omitempty"`
	InputTokens           *int64   `json:"input_tokens"`
	CachedInputTokens     *int64   `json:"cached_input_tokens"`
	OutputTokens          *int64   `json:"output_tokens"`
	CostUSD               *float64 `json:"cost_usd"`
	KnownCostUSD          *float64 `json:"known_cost_usd"`
	CostComplete          bool     `json:"cost_complete"`
	Complete              bool     `json:"complete"`
	Invocations           int      `json:"invocations"`
	IncompleteInvocations int      `json:"incomplete_invocations"`
}
type InvocationUsage struct {
	UsageIdentity
	Observations map[string]UsageObservation `json:"observations"`
}
type UsageReport struct {
	Summary         UsageSummary      `json:"summary"`
	ByProviderModel []UsageGroup      `json:"by_provider_model"`
	Invocations     []InvocationUsage `json:"invocations"`
}
type UsageGroup struct {
	Provider string       `json:"provider"`
	Model    string       `json:"model"`
	Summary  UsageSummary `json:"summary"`
}

func tokenValue(value any) *int64 {
	var n int64
	switch v := value.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= math.MaxInt64 || v != math.Trunc(v) {
			return nil
		}
		n = int64(v)
	case json.Number:
		var err error
		n, err = v.Int64()
		if err != nil {
			return nil
		}
	default:
		return nil
	}
	if n < 0 {
		return nil
	}
	return &n
}
func costValue(value any) *float64 {
	var n float64
	var err error
	switch v := value.(type) {
	case float64:
		n = v
	case json.Number:
		n, err = v.Float64()
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	default:
		return nil
	}
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return &n
}
func firstToken(values map[string]any, names ...string) *int64 {
	for _, name := range names {
		if n := tokenValue(values[name]); n != nil {
			return n
		}
	}
	return nil
}

func NormalizeUsage(update UsageUpdate) UsageObservation {
	usage := update.Usage
	scope := "invocation"
	if total, ok := usage["total"].(map[string]any); ok {
		usage = total
		scope = "session"
	} else if last, ok := usage["last"].(map[string]any); ok {
		usage = last
	}
	input := firstToken(usage, "input_tokens", "inputTokens", "input")
	cached := firstToken(usage, "cache_read_input_tokens", "cached_input_tokens", "cachedInputTokens")
	output := firstToken(usage, "output_tokens", "outputTokens", "output")
	if strings.Contains(update.Provider, "claude") && input != nil {
		n := *input
		for _, value := range []*int64{cached, firstToken(usage, "cache_creation_input_tokens")} {
			if value != nil {
				if *value > math.MaxInt64-n {
					input = nil
					break
				}
				n += *value
			}
		}
		if input != nil {
			input = &n
		}
	}
	return UsageObservation{InputTokens: input, CachedInputTokens: cached, OutputTokens: output, CostUSD: costValue(update.CostUSD), Scope: scope, Final: update.Final, Complete: update.Final && !update.Incomplete && input != nil && output != nil, ObservedAt: time.Now().UTC()}
}

func (s *SQLiteStore) BeginUsage(ctx context.Context, identity UsageIdentity) error {
	if identity.ID == "" || identity.RunID == "" {
		return fmt.Errorf("usage requires durable invocation identity")
	}
	if identity.Provider == "" {
		identity.Provider = "unknown"
	}
	if identity.Model == "" {
		identity.Model = "unknown"
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO usage_invocations(id,origin_id,record) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING", identity.ID, identity.RunID, data)
	if err != nil {
		return err
	}
	var existing []byte
	if err = s.db.QueryRowContext(ctx, "SELECT record FROM usage_invocations WHERE id=?", identity.ID).Scan(&existing); err != nil {
		return err
	}
	var admitted UsageIdentity
	if err = decodeJSONWithNumbers(existing, &admitted); err != nil {
		return err
	}
	if admitted.RunID != identity.RunID || admitted.Kind != identity.Kind || admitted.WorkerID != identity.WorkerID || admitted.TaskAttemptID != identity.TaskAttemptID {
		return fmt.Errorf("usage invocation identity reused with conflicting ownership")
	}
	return nil
}
func (s *SQLiteStore) ObserveUsage(ctx context.Context, identity UsageIdentity, update UsageUpdate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = observeUsageTx(ctx, tx, identity, update); err != nil {
		return err
	}
	return tx.Commit()
}
func observeUsageTx(ctx context.Context, tx *sql.Tx, identity UsageIdentity, update UsageUpdate) error {
	if update.Final {
		update.Key = "final"
	} else if update.Key == "" {
		update.Key = "counter"
	}
	var err error

	var raw []byte
	if err = tx.QueryRowContext(ctx, "SELECT record FROM usage_invocations WHERE id=? AND origin_id=?", identity.ID, identity.RunID).Scan(&raw); err != nil {
		return err
	}
	var current UsageIdentity
	if err = decodeJSONWithNumbers(raw, &current); err != nil {
		return err
	}
	if update.Provider != "" {
		current.Provider = update.Provider
	}
	if update.Model != "" {
		current.Model = update.Model
	}
	if update.SessionID != "" {
		current.SessionID = update.SessionID
	}
	if update.Final {
		current.Finished = true
	}
	update.Provider = current.Provider
	observation := NormalizeUsage(update)
	if observation.Scope == "session" && current.SessionID != "" {
		scope := current.Provider + "\x00" + current.WorkerID + "\x00" + current.SessionID
		var previous UsageObservation
		var counter []byte
		counterErr := tx.QueryRowContext(ctx, "SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?", current.RunID, scope).Scan(&counter)
		if counterErr == nil {
			if err = decodeJSONWithNumbers(counter, &previous); err != nil {
				return err
			}
		} else if !errors.Is(counterErr, sql.ErrNoRows) {
			return counterErr
		}
		delta := func(now, before *int64) *int64 {
			if now == nil {
				return nil
			}
			value := *now
			if before != nil {
				if value < *before {
					observation.CounterReset = true
				} else {
					value -= *before
				}
			}
			return &value
		}
		observation.DeltaInputTokens = delta(observation.InputTokens, previous.InputTokens)
		observation.DeltaCachedInputTokens = delta(observation.CachedInputTokens, previous.CachedInputTokens)
		observation.DeltaOutputTokens = delta(observation.OutputTokens, previous.OutputTokens)
		observation.CounterReset = observation.CounterReset || previous.CounterReset
		var priorRaw []byte
		if err = tx.QueryRowContext(ctx, "SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?", identity.ID, update.Key).Scan(&priorRaw); err == nil {
			var prior UsageObservation
			if err = decodeJSONWithNumbers(priorRaw, &prior); err != nil {
				return err
			}
			if !addTokens(&observation.DeltaInputTokens, prior.DeltaInputTokens) {
				observation.CounterReset = true
			}
			if !addTokens(&observation.DeltaCachedInputTokens, prior.DeltaCachedInputTokens) {
				observation.CounterReset = true
			}
			if !addTokens(&observation.DeltaOutputTokens, prior.DeltaOutputTokens) {
				observation.CounterReset = true
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		counter, err = json.Marshal(observation)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO usage_session_counters VALUES(?,?,?) ON CONFLICT(origin_id,scope_key) DO UPDATE SET record=excluded.record", current.RunID, scope, counter); err != nil {
			return err
		}
	}
	data, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_observations(invocation_id,observation_key,record) VALUES(?,?,?) ON CONFLICT(invocation_id,observation_key) DO UPDATE SET record=excluded.record`, identity.ID, update.Key, data); err != nil {
		return err
	}
	raw, err = json.Marshal(current)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE usage_invocations SET record=? WHERE id=?", raw, identity.ID); err != nil {
		return err
	}
	return nil
}

func (s *SQLiteStore) Usage(ctx context.Context, run string) (UsageReport, error) {
	report := UsageReport{Invocations: []InvocationUsage{}, Summary: UsageSummary{Complete: true, CostComplete: true}}
	rows, err := s.db.QueryContext(ctx, `SELECT i.record,o.observation_key,o.record FROM usage_invocations i LEFT JOIN usage_observations o ON o.invocation_id=i.id WHERE i.origin_id=? ORDER BY i.rowid,o.rowid`, run)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	indices := map[string]int{}
	for rows.Next() {
		var raw, observation []byte
		var key *string
		if err = rows.Scan(&raw, &key, &observation); err != nil {
			return report, err
		}
		var identity UsageIdentity
		if err = decodeJSONWithNumbers(raw, &identity); err != nil {
			return report, err
		}
		index, ok := indices[identity.ID]
		if !ok {
			index = len(report.Invocations)
			indices[identity.ID] = index
			report.Invocations = append(report.Invocations, InvocationUsage{UsageIdentity: identity, Observations: map[string]UsageObservation{}})
		}
		if key != nil {
			var value UsageObservation
			if err = decodeJSONWithNumbers(observation, &value); err != nil {
				return report, err
			}
			report.Invocations[index].Observations[*key] = value
		}
	}
	if err = rows.Err(); err != nil {
		return report, err
	}
	report.Summary = SummarizeUsage(report.Invocations)
	groups := map[string][]InvocationUsage{}
	for _, invocation := range report.Invocations {
		key := invocation.Provider + "\x00" + invocation.Model
		groups[key] = append(groups[key], invocation)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := groups[key]
		report.ByProviderModel = append(report.ByProviderModel, UsageGroup{Provider: values[0].Provider, Model: values[0].Model, Summary: SummarizeUsage(values)})
	}
	return report, nil
}

func addTokens(target **int64, value *int64) bool {
	if value == nil {
		return true
	}
	if *target == nil {
		zero := int64(0)
		*target = &zero
	}
	if **target > math.MaxInt64-*value {
		return false
	} else {
		**target += *value
	}
	return true
}
func maxTokens(a, b *int64) *int64 {
	if a == nil {
		return b
	}
	if b == nil || *a >= *b {
		return a
	}
	return b
}
func SummarizeUsage(invocations []InvocationUsage) UsageSummary {
	out := UsageSummary{Complete: true, CostComplete: true, Invocations: len(invocations)}
	countersUnknown := false
	knownCost := float64(0)
	costOverflow := false
	add := func(target **int64, value *int64) {
		if !addTokens(target, value) {
			out.Overflow = true
			out.Complete = false
			countersUnknown = true
		}
	}
	for _, invocation := range invocations {
		selected := UsageObservation{}
		hasFinal := false
		for _, value := range invocation.Observations {
			if value.Final {
				selected = value
				hasFinal = true
				break
			}
		}
		if !hasFinal || !selected.Complete {
			partial := UsageObservation{Scope: "invocation"}
			for _, value := range invocation.Observations {
				if value.Final {
					continue
				}
				if value.Scope == "session" {
					partial.InputTokens = maxTokens(partial.InputTokens, value.InputTokens)
					partial.CachedInputTokens = maxTokens(partial.CachedInputTokens, value.CachedInputTokens)
					partial.OutputTokens = maxTokens(partial.OutputTokens, value.OutputTokens)
					partial.Scope = "session"
				} else {
					add(&partial.InputTokens, value.InputTokens)
					add(&partial.CachedInputTokens, value.CachedInputTokens)
					add(&partial.OutputTokens, value.OutputTokens)
				}
			}
			selected.InputTokens = maxTokens(selected.InputTokens, partial.InputTokens)
			selected.CachedInputTokens = maxTokens(selected.CachedInputTokens, partial.CachedInputTokens)
			selected.OutputTokens = maxTokens(selected.OutputTokens, partial.OutputTokens)
			if selected.Scope == "" {
				selected.Scope = partial.Scope
			}
		}
		incomplete := !selected.Complete || !invocation.Finished
		if incomplete {
			out.Complete = false
			out.IncompleteInvocations++
		}
		if selected.CostUSD == nil {
			out.CostComplete = false
		} else {
			next := knownCost + *selected.CostUSD
			if math.IsInf(next, 0) {
				costOverflow = true
				out.Overflow = true
				out.CostComplete = false
			} else {
				knownCost = next
			}
		}
		session := false
		delta := UsageObservation{}
		for _, value := range invocation.Observations {
			if value.Scope != "session" {
				continue
			}
			session = true
			add(&delta.InputTokens, value.DeltaInputTokens)
			add(&delta.CachedInputTokens, value.DeltaCachedInputTokens)
			add(&delta.OutputTokens, value.DeltaOutputTokens)
			if value.CounterReset {
				countersUnknown = true
				if !incomplete {
					out.IncompleteInvocations++
					incomplete = true
				}
				out.Complete = false
			}
		}
		if session {
			add(&out.InputTokens, delta.InputTokens)
			add(&out.CachedInputTokens, delta.CachedInputTokens)
			add(&out.OutputTokens, delta.OutputTokens)
		} else {
			add(&out.InputTokens, selected.InputTokens)
			add(&out.CachedInputTokens, selected.CachedInputTokens)
			add(&out.OutputTokens, selected.OutputTokens)
		}
	}
	if countersUnknown {
		out.InputTokens = nil
		out.CachedInputTokens = nil
		out.OutputTokens = nil
	}
	if !costOverflow {
		out.KnownCostUSD = &knownCost
		if out.CostComplete {
			out.CostUSD = &knownCost
		}
	}
	return out
}

func usageUpdate(outputs map[string]any, final bool) UsageUpdate {
	usage, _ := outputs["usage"].(map[string]any)
	incomplete, _ := outputs["usage_incomplete"].(bool)
	return UsageUpdate{Key: "final", Provider: nativeString(outputs, "provider"), Model: nativeString(outputs, "model"), SessionID: nativeString(outputs, "session_id"), Usage: usage, CostUSD: outputs["cost_usd"], Final: final, Incomplete: incomplete}
}

func repairUsageID(attempt string, count int) string {
	return attempt + "/repair/" + strconv.Itoa(count)
}
