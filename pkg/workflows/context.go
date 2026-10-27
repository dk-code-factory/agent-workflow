package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

func decodeJSONWithNumbers(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

type Delivery struct {
	Channel, ChatID, TopicID, ThreadTS, MessageID, ReplyToMessageID string
	ReplyHandles                                                    map[string]string
}

type StepExecution struct {
	FailureClass string         `json:"failure_class,omitempty"`
	AttemptID    string         `json:"attempt_id,omitempty"`
	StartedAt    time.Time      `json:"started_at,omitempty"`
	FinishedAt   time.Time      `json:"finished_at,omitempty"`
	ID           string         `json:"id"`
	Status       string         `json:"status"`
	Outputs      map[string]any `json:"outputs,omitempty"`
	Error        string         `json:"error,omitempty"`
}

type JobIteration struct {
	Number  int                      `json:"number"`
	Status  string                   `json:"status"`
	Outputs map[string]any           `json:"outputs,omitempty"`
	Steps   map[string]StepExecution `json:"steps,omitempty"`
	Error   string                   `json:"error,omitempty"`
}

type JobExecution struct {
	ChildRunID   string                   `json:"child_run_id,omitempty"`
	ChildJobs    map[string]JobExecution  `json:"child_jobs,omitempty"`
	Items        []IterationItemResult    `json:"items,omitempty"`
	Complete     bool                     `json:"complete,omitempty"`
	FailureClass string                   `json:"failure_class,omitempty"`
	Iterations   []JobIteration           `json:"iterations,omitempty"`
	ID           string                   `json:"id"`
	Status       string                   `json:"status"`
	Outputs      map[string]any           `json:"outputs,omitempty"`
	Steps        map[string]StepExecution `json:"steps,omitempty"`
	Error        string                   `json:"error,omitempty"`
}

type AgentRequest struct {
	ObserveUsage   func(UsageUpdate) error
	Tools          *InvocationTools
	Generation     int64
	ParentWorkerID string
	RunID          string
	InvocationID   string
	RepairOnly     bool
	AgentID        string
	Model          string
	Prompt         string
	Workspace      string
	Output         *AgentOutputContract
}

type AgentRunner interface {
	RunAgent(context.Context, AgentRequest) (map[string]any, error)
}

func cloneJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = cloneJSONValue(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneJSONValue(v)
		}
		return out
	default:
		return v
	}
}

func nativeString(m map[string]any, key string) string { v, _ := m[key].(string); return v }
func nativeInt(m map[string]any, key string, fallback int) int {
	switch v := m[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return int(n)
		}
		return fallback
	default:
		return fallback
	}
}

// SessionRunner is the live-provider contract. Compatibility adapters report
// their limitations explicitly; callers must not infer resumability from IDs.
type ProviderCapabilities struct {
	Sessions         bool `json:"sessions"`
	Resume           bool `json:"resume"`
	Interrupt        bool `json:"interrupt"`
	StructuredOutput bool `json:"structured_output"`
	ManagedPolicy    bool `json:"managed_policy"`
}
type ProviderEvent struct {
	Type      string         `json:"type"`
	SessionID string         `json:"session_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}
type SessionRequest struct {
	AgentRequest
	SessionID string
}
type SessionResult struct {
	ProviderStatus  string         `json:"provider_status,omitempty"`
	ProviderVersion string         `json:"provider_version,omitempty"`
	SessionID       string         `json:"session_id"`
	Status          string         `json:"status"`
	Outputs         map[string]any `json:"outputs,omitempty"`
}
type SessionRunner interface {
	Capabilities(string) (ProviderCapabilities, error)
	RunTurn(context.Context, SessionRequest, func(ProviderEvent)) (SessionResult, error)
}
