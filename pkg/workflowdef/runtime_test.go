package workflowdef

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type testHost struct {
	mu      sync.Mutex
	calls   []Call
	state   *State
	invoke  func(context.Context, Call) (Reply, error)
	saveErr error
}

func (h *testHost) Save(_ context.Context, state *State) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = state
	return h.saveErr
}
func (h *testHost) Invoke(ctx context.Context, call Call) (Reply, error) {
	h.mu.Lock()
	h.calls = append(h.calls, call)
	h.mu.Unlock()
	if h.invoke != nil {
		return h.invoke(ctx, call)
	}
	return Reply{Value: map[string]any{"text": call.Prompt}}, nil
}
func definition(t *testing.T, source string) *Definition {
	t.Helper()
	d, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func execute(t *testing.T, source string, h *testHost) (any, error) {
	t.Helper()
	return (&Runtime{Definition: definition(t, source), Host: h}).Run(context.Background(), "run", map[string]any{}, nil)
}

func TestRuntimeControlAndBindings(t *testing.T) {
	d := definition(t, `{"version":"0.2","steps":[
  {"id":"p","parallel":[{"id":"a","value":1},{"id":"b","value":2}]},
  {"id":"branch","if":{"path":"$.steps.b","equals":2},"then":[{"value":"$.steps.a"}]},
  {"id":"cycle","repeat":3,"initial":{"number":7},"steps":[{"id":"number","value":"$.previous.number"}]},
  {"id":"items","each":"$.input.items","maxConcurrency":2,"steps":[{"value":"$.item.value"}]}
 ],"return":{"parallel":"$.steps.p","branch":"$.steps.branch","cycle":"$.steps.cycle","items":"$.steps.items"}}`)
	h := &testHost{}
	value, err := (&Runtime{Definition: d, Host: h}).Run(context.Background(), "run", map[string]any{"items": []any{"x", "y"}}, nil)
	want := map[string]any{"parallel": []any{float64(1), float64(2)}, "branch": float64(1), "cycle": float64(7), "items": []any{"x", "y"}}
	if err != nil || !reflect.DeepEqual(value, want) {
		t.Fatalf("value=%#v error=%v", value, err)
	}
	if len(h.calls) != 0 {
		t.Fatal("data-only flow dispatched work")
	}
}

func TestRuntimeFallbackAndObserver(t *testing.T) {
	h := &testHost{invoke: func(_ context.Context, c Call) (Reply, error) {
		if c.Role == "broken" {
			return Reply{}, &Failure{Code: "provider.failed", Message: "boom", EffectState: "completed"}
		}
		if c.Role == "observer" {
			return Reply{}, errors.New("notification failed")
		}
		return Reply{Value: map[string]any{"text": c.Prompt}}, nil
	}}
	value, err := execute(t, `{"version":"0.2","steps":[{"id":"answer","agent":"broken","prompt":"do work","onError":[{"agent":"helper","prompt":"Handle {{ $.error.message }}"}],"onSuccess":[{"agent":"observer","prompt":"report"}]}],"return":"$.steps.answer"}`, h)
	if err != nil || object(value)["text"] != "Handle boom" {
		t.Fatalf("%#v %v", value, err)
	}
	if len(h.calls) != 3 {
		t.Fatalf("calls %d", len(h.calls))
	}
	if h.state.Plans["root/steps/0/onSuccess/error"] == nil {
		t.Fatal("observer failure not retained")
	}
}

func TestRuntimeAbortAndHardFailuresBypassHandlers(t *testing.T) {
	for _, hardFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(hardFailure), func(t *testing.T) {
			h := &testHost{invoke: func(_ context.Context, c Call) (Reply, error) {
				if hardFailure {
					return Reply{}, &Failure{Code: "state.ownership", Message: "lost owner", Hard: true, EffectState: "unknown"}
				}
				return Reply{}, errors.New("failed")
			}}
			step := `{"agent":"broken","prompt":"task","onError":[{"fail":true}]}`
			_, err := execute(t, `{"version":"0.2","steps":[`+step+`],"onError":[{"agent":"forbidden","prompt":"must not run"}]}`, h)
			if err == nil || len(h.calls) != 1 {
				t.Fatalf("error=%v calls=%d", err, len(h.calls))
			}
		})
	}
}

func TestRuntimeReplacementMustMeetOriginalContract(t *testing.T) {
	h := &testHost{invoke: func(context.Context, Call) (Reply, error) { return Reply{}, errors.New("failed") }}
	_, err := execute(t, `{"version":"0.2","steps":[{"agent":"a","prompt":"task","output":{"type":"integer"},"onError":[{"value":"invalid"}]}]}`, h)
	if err == nil {
		t.Fatal("invalid replacement accepted")
	}
}

func TestRuntimeCustomBreakers(t *testing.T) {
	for _, policy := range []string{`{"agent":"supervisor"}`, `{"run":["breaker"]}`} {
		for _, response := range []string{`{"allow":true}`, `{"allow":false,"reason":"pause"}`, `{"allow":"yes"}`, `{"allow":true,"extra":1}`} {
			t.Run(policy+response, func(t *testing.T) {
				h := &testHost{invoke: func(_ context.Context, c Call) (Reply, error) {
					if c.Role == "worker" {
						return Reply{Value: "done"}, nil
					}
					return Reply{Value: map[string]any{"text": response, "stdout": response}}, nil
				}}
				_, err := execute(t, `{"version":"0.2","steps":[{"agent":"worker","prompt":"task","breaker":`+policy+`}]}`, h)
				allowed := response == `{"allow":true}`
				if (err == nil) != allowed {
					t.Fatalf("response=%s err=%v", response, err)
				}
				want := 1
				if allowed {
					want = 2
				}
				if len(h.calls) != want {
					t.Fatalf("checker failed open: %d calls", len(h.calls))
				}
			})
		}
	}
}

func TestRuntimeStaticBreakerAndBudget(t *testing.T) {
	h := &testHost{invoke: func(context.Context, Call) (Reply, error) { return Reply{}, errors.New("failed") }}
	_, err := execute(t, `{"version":"0.2","steps":[{"repeat":5,"steps":[{"agent":"worker","prompt":"task","breaker":{"failures":2,"cooldownMs":30000},"onError":[{"value":"fallback"}]}]}]}`, h)
	if err != nil || len(h.calls) != 2 {
		t.Fatalf("calls=%d error=%v", len(h.calls), err)
	}
	h = &testHost{}
	_, err = execute(t, `{"version":"0.2","limits":{"maxTaskAttempts":1},"steps":[{"agent":"worker","prompt":"task","breaker":{"agent":"supervisor"}}]}`, h)
	if err == nil || len(h.calls) != 1 {
		t.Fatalf("budget exceeded: calls=%d err=%v", len(h.calls), err)
	}
}

func TestRuntimeConversationSessionsAndTools(t *testing.T) {
	sessions := map[string]string{}
	h := &testHost{invoke: func(ctx context.Context, c Call) (Reply, error) {
		if !c.Persistent {
			t.Fatal("conversation lost persistence")
		}
		if previous := sessions[c.Role]; previous != "" && c.SessionID != previous {
			t.Fatalf("lost session for %s", c.Role)
		}
		session := "session-" + c.Role
		sessions[c.Role] = session
		if err := c.SessionCreated(session); err != nil {
			return Reply{}, err
		}
		var err error
		switch c.Role {
		case "writer":
			args := json.RawMessage(`{"to":"reviewer","message":"review draft"}`)
			_, err = c.ToolCall(ctx, "m1", "send", args)
			if err == nil {
				_, err = c.ToolCall(ctx, "m1", "send", args)
			} // acknowledgement replay must not duplicate delivery
			if _, spoof := c.ToolCall(ctx, "spoof", "send", json.RawMessage(`{"to":"outsider","message":"no"}`)); spoof == nil {
				t.Fatal("undeclared recipient accepted")
			}
			if _, spoof := c.ToolCall(ctx, "finish", "finish", json.RawMessage(`{"text":"no"}`)); spoof == nil {
				t.Fatal("unauthorized finisher accepted")
			}
		case "reviewer":
			_, err = c.ToolCall(ctx, "done", "finish", json.RawMessage(`{"text":"approved"}`))
		}
		return Reply{Value: map[string]any{"text": "turn ended"}, SessionID: session}, err
	}}
	value, err := execute(t, `{"version":"0.2","steps":[{"conversation":["writer","reviewer"],"finishBy":"reviewer","prompt":"write and review"}]}`, h)
	if err != nil || object(value)["text"] != "approved" {
		t.Fatalf("%#v %v", value, err)
	}
	c := h.state.Conversations["root/steps/0"]
	if c.Messages != 1 || c.Turns != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestRuntimeRecoveryRetainsOriginalSession(t *testing.T) {
	h := &testHost{invoke: func(ctx context.Context, c Call) (Reply, error) {
		if c.Role == "worker" && c.ToolCall == nil {
			if err := c.SessionCreated("original"); err != nil {
				return Reply{}, err
			}
			return Reply{SessionID: "original"}, errors.New("task failed")
		}
		if c.Role == "helper" {
			_, err := c.ToolCall(ctx, "help", "send", json.RawMessage(`{"to":"worker","message":"try the smaller fix"}`))
			return Reply{SessionID: "helper"}, err
		}
		if c.SessionID != "original" {
			t.Fatalf("original session lost: %q", c.SessionID)
		}
		_, err := c.ToolCall(ctx, "done", "finish", json.RawMessage(`{"ok":true}`))
		return Reply{SessionID: "original"}, err
	}}
	value, err := execute(t, `{"version":"0.2","steps":[{"agent":"worker","prompt":"task","output":{"type":"object","properties":{"ok":{"const":true}},"required":["ok"]},"onError":[{"recover":"helper"}]}]}`, h)
	if err != nil || object(value)["ok"] != true || len(h.calls) != 3 {
		t.Fatalf("%#v %v calls=%d", value, err, len(h.calls))
	}
}

func TestRuntimeConversationFailures(t *testing.T) {
	for _, mode := range []string{"stall", "invalid", "turns"} {
		t.Run(mode, func(t *testing.T) {
			h := &testHost{invoke: func(ctx context.Context, c Call) (Reply, error) {
				switch mode {
				case "invalid":
					_, _ = c.ToolCall(ctx, "finish", "finish", json.RawMessage(`{"oops":1}`))
				case "turns":
					to := "a"
					if c.Role == "a" {
						to = "b"
					}
					args, _ := json.Marshal(map[string]any{"to": to, "message": "again"})
					_, _ = c.ToolCall(ctx, "send", "send", args)
				}
				return Reply{SessionID: c.Role}, nil
			}}
			_, err := execute(t, `{"version":"0.2","steps":[{"conversation":["a","b"],"start":"b","finishBy":"b","prompt":"task","maxTurns":2}]}`, h)
			if err == nil {
				t.Fatal("conversation incorrectly succeeded")
			}
			if len(h.calls) > 2 {
				t.Fatal("turn budget exceeded")
			}
		})
	}
}

func TestRuntimeCheckpointAndResume(t *testing.T) {
	source := `{"version":"0.2","steps":[{"id":"first","agent":"a","prompt":"first"},{"agent":"a","prompt":"second"}]}`
	h := &testHost{invoke: func(_ context.Context, c Call) (Reply, error) {
		if c.Prompt == "second" {
			return Reply{}, errors.New("fail")
		}
		return Reply{Value: "saved"}, nil
	}}
	d := definition(t, source)
	rt := &Runtime{Definition: d, Host: h}
	if _, err := rt.Run(context.Background(), "run", nil, nil); err == nil {
		t.Fatal("expected failure")
	}
	state := h.state
	before := len(h.calls)
	if _, err := (&Runtime{Definition: d, Host: h, State: state}).Run(context.Background(), "run", nil, nil); err == nil || len(h.calls) != before {
		t.Fatal("failed call replayed without authorization")
	}
	h.invoke = func(_ context.Context, c Call) (Reply, error) {
		if c.Prompt != "second" {
			t.Fatal("completed call repeated")
		}
		return Reply{Value: "fixed"}, nil
	}
	if _, err := (&Runtime{Definition: d, Host: h, State: state, RetryFailed: true}).Run(context.Background(), "run", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Runtime{Definition: d, Host: h, State: state}).Run(context.Background(), "different-run", nil, nil); err == nil {
		t.Fatal("wrong checkpoint accepted")
	}
}

func TestRuntimeStorageFailurePreventsEffects(t *testing.T) {
	h := &testHost{saveErr: errors.New("disk full")}
	_, err := execute(t, `{"version":"0.2","steps":[{"agent":"a","prompt":"task"}]}`, h)
	if err == nil || len(h.calls) != 0 {
		t.Fatal("effect started without durable admission")
	}
}

func TestConversationIntegrityFailureCannotBeHiddenByProvider(t *testing.T) {
	h := &testHost{invoke: func(ctx context.Context, c Call) (Reply, error) {
		_, err := c.ToolCall(ctx, "id", "send", json.RawMessage(`{"to":"b","message":"first"}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.ToolCall(ctx, "id", "send", json.RawMessage(`{"to":"b","message":"changed"}`))
		if err == nil {
			t.Fatal("changed request accepted")
		}
		// A provider might treat a tool error as ordinary text and return success.
		return Reply{SessionID: "session-a"}, nil
	}}
	_, err := execute(t, `{"version":"0.2","steps":[{"conversation":["a","b"],"finishBy":"b","prompt":"task","onError":[{"value":"must not recover"}]}]}`, h)
	if err == nil || !NeedsAttention(err) {
		t.Fatalf("integrity failure was swallowed: %v", err)
	}
}

func TestRuntimeNestedParallelSharesConcurrencyLimit(t *testing.T) {
	var mu sync.Mutex
	active, maximum := 0, 0
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	h := &testHost{invoke: func(_ context.Context, c Call) (Reply, error) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		started <- struct{}{}
		<-release
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		return Reply{Value: c.Prompt}, nil
	}}
	d := definition(t, `{"version":"0.2","limits":{"maxConcurrency":2},"steps":[{"parallel":[{"parallel":[{"agent":"a","prompt":"one"},{"agent":"a","prompt":"two"}]},{"parallel":[{"agent":"a","prompt":"three"},{"agent":"a","prompt":"four"}]}]}]}`)
	done := make(chan error, 1)
	go func() {
		_, err := (&Runtime{Definition: d, Host: h}).Run(context.Background(), "run", nil, nil)
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("parallel execution stalled")
		}
	}
	select {
	case <-started:
		t.Error("nested parallel dispatched beyond the shared limit")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	err := <-done
	if err != nil || maximum != 2 || len(h.calls) != 4 {
		t.Fatalf("concurrency=%d calls=%d err=%v", maximum, len(h.calls), err)
	}
}

func TestBindingLiteralCardinalityAndSinglePass(t *testing.T) {
	env := map[string]any{"input": map[string]any{"text": "{{ $.missing }}", "list": []any{1, 2}, "null": nil}}
	if got, err := Prompt("{{ $.input.text }}", env); err != nil || got != "{{ $.missing }}" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := Resolve(map[string]any{"literal": "$.missing"}, env); err != nil || got != "$.missing" {
		t.Fatalf("%#v %v", got, err)
	}
	if _, err := Select("$.input.list[*]", env, false); err == nil || !strings.Contains(err.Error(), "cardinality") {
		t.Fatal("multiple matches accepted")
	}
	if got, err := Select("$.input.null", env, false); err != nil || got != nil {
		t.Fatal("matched null treated as missing")
	}
}
