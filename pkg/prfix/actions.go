package prfix

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type Runtime struct {
	BeforeCheckpoint     func(string, ActionOutput) error
	Root                 string
	Store                *workflows.SQLiteStore
	Workers              *workflows.WorkerManager
	Provider             Provider
	Now                  func() time.Time
	PollInterval         time.Duration
	publicationContended func()
	observationContext   context.Context
}
type ActionInput struct {
	Worker     string   `json:"worker,omitempty"`
	Receipt    *Receipt `json:"receipt,omitempty"`
	Plan       string   `json:"plan"`
	Occurrence string   `json:"occurrence,omitempty"`
	Claim      string   `json:"claim,omitempty"`
	Cycle      string   `json:"cycle,omitempty"`
	Attempt    int      `json:"attempt,omitempty"`
	Round      int      `json:"round,omitempty"`
	Payload    string   `json:"payload,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Intent     string   `json:"intent,omitempty"`
}
type IntentRef struct {
	ID string `json:"id"`
}
type ActionOutput struct {
	Round    int         `json:"round"`
	Receipt  *Receipt    `json:"receipt,omitempty"`
	Claim    string      `json:"claim"`
	Invoke   bool        `json:"invoke"`
	Settled  bool        `json:"settled"`
	Recheck  bool        `json:"recheck"`
	Cycle    string      `json:"cycle"`
	Worker   string      `json:"worker"`
	Prompt   string      `json:"prompt"`
	Complete bool        `json:"complete"`
	Ready    bool        `json:"ready"`
	Report   string      `json:"report"`
	Items    []IntentRef `json:"items"`
	Repair   bool        `json:"repair"`
	Deferred bool        `json:"deferred"`
	State    string      `json:"state"`
}

func output() ActionOutput { return ActionOutput{Items: []IntentRef{}} }
func (r *Runtime) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}
func (r *Runtime) provider() Provider {
	if r.Provider != nil {
		return r.Provider
	}
	return &GitHub{Root: r.Root}
}
func (r *Runtime) ledger() (*Ledger, error) {
	if r.observationContext != nil {
		return openLedgerReadOnly(r.observationContext, r.Root)
	}
	return OpenLedger(r.Root)
}

func (r *Runtime) observer(ctx context.Context) *Runtime {
	observer := *r
	observer.observationContext = ctx
	return &observer
}
func (r *Runtime) plan(in ActionInput) (Plan, error) {
	root := filepath.Join(r.Root, ".agent-workflow", "pr-fix", "artifacts")
	abs, e := filepath.Abs(in.Plan)
	if e != nil {
		return Plan{}, e
	}
	rel, e := filepath.Rel(root, abs)
	if e != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return Plan{}, fmt.Errorf("plan outside application-owned artifacts")
	}
	p, e := ReadPlan(abs)
	if e != nil {
		return p, e
	}
	b, e := os.ReadFile(abs)
	if e != nil {
		return p, e
	}
	if filepath.Base(abs) != digest(json.RawMessage(b))+".json" {
		return p, fmt.Errorf("plan artifact identity mismatch")
	}
	return p, nil
}
func (r *Runtime) Register(registry *workflows.ActionRegistry) error {
	names := []string{"review_admit", "cycle", "claim", "worker_spawn", "worker_send", "worker_wait", "finish", "status", "commit", "intents", "publish", "ci", "reserve", "completion", "receipt", "candidates", "ci_classify", "close", "worker_recover", "worker_adopt", "worker_acknowledge", "turn_state", "check_prepare", "review_request_inspect", "review_request_dispose", "reply_inspect"}
	for _, name := range names {
		name := name
		effect := workflows.ActionExternalMutation
		if name == "cycle" || name == "worker_wait" || name == "status" || name == "intents" || name == "completion" || name == "candidates" || name == "ci" || name == "ci_classify" || name == "turn_state" || name == "check_prepare" || name == "review_request_inspect" || name == "reply_inspect" {
			effect = workflows.ActionReadOnly
		}
		spec := workflows.ActionSpec[ActionInput, ActionOutput]{Implementation: Implementation + "/" + name, Effect: effect}
		spec.Bindings = func(ctx context.Context, ac workflows.ActionContext, in ActionInput) (json.RawMessage, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			p, e := r.plan(in)
			if e != nil {
				return nil, e
			}
			return json.Marshal(map[string]any{"plan": digest(p), "logical_run": p.LogicalRun, "repository": p.Identity.Repository, "root": r.Root})
		}
		spec.Handler = func(ctx context.Context, ac workflows.ActionContext, in ActionInput) (workflows.ActionResult[ActionOutput], error) {
			ctx = context.WithValue(ctx, liveObservationKey{}, &liveObservation{})
			out, e := r.handle(ctx, name, ac, in)
			if e != nil {
				return workflows.ActionResult[ActionOutput]{Output: out}, e
			}
			if r.BeforeCheckpoint != nil {
				if e = r.BeforeCheckpoint(name, out); e != nil {
					return workflows.ActionResult[ActionOutput]{Output: out}, e
				}
			}
			path, e := r.saveAction(name, ac, in, out)
			return workflows.ActionResult[ActionOutput]{Output: out, Evidence: jsonBytes(map[string]string{"artifact": path}), Receipt: jsonBytes(map[string]string{"artifact": path})}, e
		}
		spec.ValidateCheckpoint = func(ctx context.Context, ac workflows.ActionContext, cp workflows.ActionCheckpoint[ActionInput, ActionOutput]) error {
			r := r.observer(ctx)
			if err := ctx.Err(); err != nil {
				return err
			}
			var proof struct {
				Artifact     string `json:"artifact"`
				ResultDigest string `json:"result_digest"`
			}
			if e := json.Unmarshal(cp.Result.Evidence, &proof); e != nil {
				return e
			}
			if proof.ResultDigest != "" {
				if proof.ResultDigest != digest(cp.Result.Output) {
					return fmt.Errorf("reconciled result fingerprint differs")
				}
				if name == "publish" {
					p, e := r.plan(cp.Input)
					if e != nil {
						return e
					}
					l, e := r.ledger()
					if e != nil {
						return e
					}
					i, e := l.Intent(ctx, p.LogicalRun, cp.Input.Intent)
					if e == nil && cp.Result.Output.Deferred {
						e = r.verifyReviewRequestDisposition(ctx, l, i)
						l.Close()
						return e
					}
					l.Close()
					if e != nil {
						return e
					}
					receipt := cp.Result.Output.Receipt
					if receipt == nil || receipt.ID != i.ID || receipt.PR != i.PR.URL || receipt.Head != i.Head {
						return fmt.Errorf("reconciled publication receipt mismatch")
					}
				}
				return nil
			}
			if proof.Artifact != r.actionPath(name, ac, cp.Input) {
				return fmt.Errorf("action evidence identity differs")
			}
			var saved ActionOutput
			if e := readJSON(proof.Artifact, &saved); e != nil {
				return e
			}
			if digest(saved) != digest(cp.Result.Output) {
				return fmt.Errorf("action checkpoint evidence differs")
			}
			return nil
		}
		spec.Reconcile = func(ctx context.Context, ac workflows.ActionContext, cp workflows.ActionCheckpoint[ActionInput, ActionOutput]) (workflows.ActionReconciliation[ActionOutput], error) {
			if err := ctx.Err(); err != nil {
				return workflows.ActionReconciliation[ActionOutput]{Outcome: "unknown"}, err
			}
			path := r.actionPath(name, ac, cp.Input)
			var out ActionOutput
			if e := readJSON(path, &out); e == nil {
				proof := jsonBytes(map[string]string{"artifact": path})
				return workflows.ActionReconciliation[ActionOutput]{Outcome: "completed", Proof: proof, Result: workflows.ActionResult[ActionOutput]{Output: out, Evidence: proof, Receipt: proof}}, nil
			} else if !errors.Is(e, os.ErrNotExist) {
				return workflows.ActionReconciliation[ActionOutput]{Outcome: "unknown"}, e
			}
			state, out, e := r.reconcile(ctx, name, ac, cp.Input)
			proof := jsonBytes(map[string]any{"operation": ac.OperationKey, "observation": state, "result_digest": digest(out)})
			return workflows.ActionReconciliation[ActionOutput]{Outcome: state, Proof: proof, Result: workflows.ActionResult[ActionOutput]{Output: out, Evidence: proof, Receipt: proof}}, e
		}
		if e := workflows.RegisterAction(registry, "prfix_"+name, spec); e != nil {
			return e
		}
	}
	return nil
}
func jsonBytes(v any) json.RawMessage {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func (r *Runtime) actionPath(name string, ac workflows.ActionContext, in ActionInput) string {
	return filepath.Join(r.Root, ".agent-workflow", "pr-fix", "operations", digest([]any{name, ac.OperationKey, in})+".json")
}
func (r *Runtime) saveAction(name string, ac workflows.ActionContext, in ActionInput, out ActionOutput) (string, error) {
	path := r.actionPath(name, ac, in)
	return path, writeImmutable(path, jsonBytes(out))
}
func (r *Runtime) origin(ctx context.Context, ac workflows.ActionContext) (string, error) {
	if r.Store == nil {
		return "", fmt.Errorf("engine store unavailable")
	}
	run, snap, e := r.Store.Load(ctx, ac.RunID)
	if e != nil {
		return "", e
	}
	if v, ok := snap.Metadata["origin_run_id"].(string); ok && v != "" {
		return v, nil
	}
	if run.OriginID != "" {
		return run.OriginID, nil
	}
	return run.ID, nil
}

type liveObservationKey struct{}
type liveObservation struct {
	Fingerprint string
	Snapshot    Snapshot
	Err         error
	Ready       bool
}

// One action accepts one complete observation. Repeated validation within that
// action shares its evidence; every subsequent action observes the live PR again.
func (r *Runtime) live(ctx context.Context, p Plan) (Snapshot, error) {
	cache, _ := ctx.Value(liveObservationKey{}).(*liveObservation)
	if cache != nil && cache.Ready && cache.Fingerprint == p.Fingerprint {
		return cache.Snapshot, cache.Err
	}
	s, e := r.captureLive(ctx, p)
	if cache != nil {
		cache.Fingerprint = p.Fingerprint
		cache.Snapshot = s
		cache.Err = e
		cache.Ready = true
	}
	return s, e
}
func (r *Runtime) captureLive(ctx context.Context, p Plan) (Snapshot, error) {
	l, e := r.ledger()
	if e != nil {
		return Snapshot{}, e
	}
	currentPlan, e := l.Plan(ctx, p.LogicalRun)
	l.Close()
	if e != nil {
		return Snapshot{}, e
	}
	if currentPlan.Fingerprint != p.Fingerprint {
		return Snapshot{}, fmt.Errorf("execution plan has been superseded; use the retained replacement admission")
	}
	s, e := capture(ctx, r.Root, r.provider(), p)
	if e != nil {
		return s, e
	}
	current, e := resolveSelection(ctx, r.Root, p.Identity, p.Upstream, p.Options, s.Paths)
	if e != nil {
		return s, e
	}
	// Preferences may refresh with affected paths. Selection or governing policy
	// drift requires new deterministic preparation, never an agent-created route.
	expected := p.Selection
	expected.Preferences = nil
	current.Preferences = nil
	if digest(current) != digest(expected) {
		return s, fmt.Errorf("selected configuration changed; reconcile and prepare a replacement execution under logical run %s", p.LogicalRun)
	}
	return s, nil
}
func (r *Runtime) handle(ctx context.Context, name string, ac workflows.ActionContext, in ActionInput) (ActionOutput, error) {
	p, e := r.plan(in)
	if e != nil {
		return output(), e
	}
	switch name {
	case "candidates":
		candidates, e := r.candidates(ctx, p)
		if e != nil {
			return output(), e
		}
		out := output()
		out.Invoke = len(candidates) > 0
		out.Report, e = artifact(r.Root, map[string]any{"candidates": candidates, "applicable": out.Invoke, "reason": "Verification requires current original reviewer candidates; an empty set is applicability evidence, not execution coverage."})
		return out, e
	case "cycle":
		if _, e := r.live(ctx, p); e != nil {
			return output(), e
		}
		engine, e := r.origin(ctx, ac)
		if e != nil {
			return output(), e
		}
		out := output()
		// Every materialized pass has a stable engine operation key. Changed-
		// input replacements and fresh noncacheable passes get distinct cycles;
		// resumed actions and same-cycle retries retain their original identity.
		out.Cycle = "cycle_" + digest([]string{p.LogicalRun, engine, ac.OperationKey})[:32]
		return out, nil
	case "review_admit":
		return r.reviewAdmission(ctx, p, in)
	case "claim":
		return r.claim(ctx, p, ac, in)
	case "worker_recover":
		return r.workerRecover(ctx, p, ac, in)
	case "worker_adopt":
		return r.workerAdopt(ctx, p, in)
	case "worker_acknowledge":
		_, out, e := r.workerAcknowledge(ctx, p, ac, in, true)
		return out, e
	case "turn_state":
		return r.turnState(ctx, p, in)
	case "check_prepare":
		return r.prepareCheck(ctx, p, in)
	case "worker_spawn":
		return r.workerSpawn(ctx, p, in)
	case "worker_send":
		return r.workerSend(ctx, p, in)
	case "worker_wait":
		return r.workerWait(ctx, p, in)
	case "finish":
		return r.finish(ctx, p, in)
	case "status":
		return r.status(ctx, p, in)
	case "commit":
		return r.commit(ctx, p, in)
	case "intents":
		return r.intents(ctx, p, in.Kind)
	case "publish":
		return r.publish(ctx, p, in)
	case "ci_classify":
		return r.classifyCI(ctx, p, in)
	case "ci":
		return r.ci(ctx, p, in)
	case "reserve":
		l, e := r.ledger()
		if e != nil {
			return output(), e
		}
		defer l.Close()
		scope := "0"
		var selected string
		err := l.db.QueryRowContext(ctx, "SELECT operation FROM reservations WHERE run=? AND kind='review' ORDER BY rowid DESC LIMIT 1", p.LogicalRun).Scan(&selected)
		if err == nil {
			scope = selected
		} else if !errors.Is(err, sql.ErrNoRows) {
			return output(), err
		}
		return output(), l.Reserve(ctx, p.LogicalRun, in.Kind, scope, ac.OperationKey, "selected CI repair")
	case "completion":
		return r.completion(ctx, p, in)
	case "close":
		return r.close(ctx, p, ac)
	case "receipt":
		if in.Receipt == nil {
			return output(), fmt.Errorf("publication receipt required")
		}
		receipt := *in.Receipt
		l, e := r.ledger()
		if e != nil {
			return output(), e
		}
		defer l.Close()
		i, e := l.Intent(ctx, p.LogicalRun, in.Intent)
		if e != nil {
			return output(), e
		}
		return r.completeEffect(ctx, l, i, receipt)
	case "reply_inspect":
		return r.replyInspect(ctx, p, in)
	case "review_request_inspect":
		return r.reviewRequestInspect(ctx, p, in)
	case "review_request_dispose":
		return r.reviewRequestDispose(ctx, p, ac, in)
	}
	return output(), fmt.Errorf("unregistered PR-fix operation")
}

func jsonObject(v any) map[string]any {
	b := jsonBytes(v)
	var out map[string]any
	if e := json.Unmarshal(b, &out); e != nil {
		panic(e)
	}
	return out
}
