package clirunner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type skillChildSDKWriter struct {
	http.ResponseWriter
	id string
}

func (w skillChildSDKWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w skillChildSDKWriter) Write(p []byte) (int, error) {
	_, err := w.ResponseWriter.Write(bytes.ReplaceAll(p, []byte("msg_broker_fixture"), []byte(w.id)))
	return len(p), err
}

// Exercise the maintained bridge and locked native SDK. Direct bridge cases
// test protocol negatives; engine cases exercise public admission, durable
// permits, original completion and recovery against the same loopback gateway.
func TestSkillChildBridgeAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires installed locked SDK; local fake API only")
	}
	for _, scenario := range []string{"foreground", "engine", "engine-dot-resources", "socket-cut-parent", "socket-cut-child", "engine-cut-child", "socket-cut-final-parent", "engine-cut-final-parent", "background-missing", "background-true", "builtin", "override", "isolation-override", "resume-override", "nested", "forbidden-read", "forbidden-tool", "child-model-mismatch", "second-call", "exhaustion", "lost-permit-ack"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			plugin := filepath.Join(root, "admitted")
			resource := filepath.Join(plugin, "skills/probe/reference.txt")
			for name, text := range map[string]string{".claude-plugin/plugin.json": `{"name":"aw-probe","version":"1.0.0"}`, "skills/probe/SKILL.md": "---\nname: probe\ndescription: Admitted child fixture.\n---\nADMITTED_CHILD_SKILL_NATIVE_139\n", "skills/probe/reference.txt": "ADMITTED_CHILD_RESOURCE_NATIVE_139\n"} {
				p := filepath.Join(plugin, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			child := workflows.SkillChild{Name: "review", Type: "aw-child-review", Prompt: "Read the retained reference and return the full result.", Description: "Admitted deterministic child", Tools: []string{"Read"}, Resources: []string{"reference.txt"}, Model: "claude-sonnet-4-6", Effort: "high", MaxTurns: 2, MaxCalls: 1}
			plan := &workflows.SkillInvocation{Version: 1, ID: "original-plan", Binding: workflows.SkillBinding{Version: 1, Name: "probe", Entry: "skills/probe/SKILL.md", Assets: &workflows.AssetBundle{}}, Arguments: "exact", Model: child.Model, ResolvedEffort: "high", Profile: claudeSkillChildProfile, Children: []workflows.SkillChild{child}, ContinuationRequiresEvidence: true, EffectiveTools: []string{"Read"}}
			var parentCalls, childCalls, permits atomic.Int32
			var wrongEffort, hostile atomic.Bool
			var sawSkill, sawResource atomic.Bool
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "count_tokens") {
					fmt.Fprint(w, `{"input_tokens":16}`)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					fmt.Fprint(w, `{"data":[],"has_more":false}`)
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				raw, _ := json.Marshal(payload)
				if strings.Contains(string(raw), "HOSTILE_") {
					hostile.Store(true)
				}
				if strings.Contains(string(raw), "ADMITTED_CHILD_SKILL_NATIVE_139") {
					sawSkill.Store(true)
				}
				if strings.Contains(string(raw), "ADMITTED_CHILD_RESOURCE_NATIVE_139") {
					sawResource.Store(true)
				}
				config, _ := payload["output_config"].(map[string]any)
				if config["effort"] != "high" {
					wrongEffort.Store(true)
				}
				system, _ := json.Marshal(payload["system"])
				isChild := strings.Contains(string(system), "Use only the explicitly retained resource files.")
				if isChild {
					tools, _ := payload["tools"].([]any)
					if len(tools) != 1 {
						t.Error("child native tool set differs", payload["tools"])
					} else if tool, _ := tools[0].(map[string]any); tool["name"] != "Read" {
						t.Error("child regained undeclared tool", tool)
					}
				}
				n := parentCalls.Add(1)
				if isChild {
					parentCalls.Add(-1)
					n = childCalls.Add(1)
				}
				if scenario == "socket-cut-parent" && !isChild && n == 1 || (scenario == "socket-cut-child" || scenario == "engine-cut-child") && isChild && n == 1 || (scenario == "socket-cut-final-parent" || scenario == "engine-cut-final-parent") && !isChild && n == 3 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				if parentCalls.Load()+childCalls.Load() > 6 {
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"fixture bound"}}`)
					return
				}
				model, _ := payload["model"].(string)
				if isChild && scenario == "child-model-mismatch" {
					model = "foreign-model"
				}
				block := map[string]any{"type": "text", "text": "SYNTHETIC_PARENT_TERMINAL_139"}
				stop := "end_turn"
				if isChild {
					requestedResource := resource
					if strings.HasPrefix(scenario, "engine") {
						if blocks, ok := payload["system"].([]any); ok {
							for _, block := range blocks {
								if item, ok := block.(map[string]any); ok {
									if text, ok := item["text"].(string); ok {
										for _, line := range strings.Split(text, "\n") {
											if filepath.IsAbs(line) && strings.HasSuffix(line, "/skills/probe/reference.txt") {
												requestedResource = line
											}
										}
									}
								}
							}
						}
					}
					if n == 1 || scenario == "exhaustion" {
						tool, input := "Read", map[string]any{"file_path": requestedResource}
						if scenario == "forbidden-read" {
							input["file_path"] = filepath.Join(root, "foreign.txt")
						}
						if scenario == "nested" {
							tool = "Agent"
							input = map[string]any{"subagent_type": child.Type, "description": child.Description, "prompt": child.Prompt, "run_in_background": false}
						}
						if scenario == "forbidden-tool" {
							tool = "Bash"
							input = map[string]any{"command": "touch FORBIDDEN_NATIVE_CHILD_EXEC"}
						}
						block = map[string]any{"type": "tool_use", "id": fmt.Sprintf("child-read-%d", n), "name": tool, "input": input}
						stop = "tool_use"
					} else {
						block["text"] = "FULL_ORIGINAL_CHILD_OUTPUT_139\nSecond original output line: 42."
					}
				} else if n == 1 {
					block = map[string]any{"type": "tool_use", "id": "original-skill", "name": "Skill", "input": map[string]any{"skill": "aw-probe:probe", "args": "exact"}}
					stop = "tool_use"
				} else if n == 2 || n == 3 && scenario == "second-call" {
					args := map[string]any{"subagent_type": child.Type, "description": child.Description, "prompt": child.Prompt, "run_in_background": false}
					switch scenario {
					case "background-missing":
						delete(args, "run_in_background")
					case "background-true":
						args["run_in_background"] = true
					case "builtin":
						args["subagent_type"] = "general-purpose"
					case "override":
						args["model"] = "haiku"
					case "isolation-override":
						args["isolation"] = "worktree"
					case "resume-override":
						args["resume"] = "foreign-child"
					}
					block = map[string]any{"type": "tool_use", "id": fmt.Sprintf("parent-agent-%d", n), "name": "Agent", "input": args}
					stop = "tool_use"
				}
				writeSDKMessage(skillChildSDKWriter{w, fmt.Sprintf("msg_%t_%d", isChild, n)}, model, []map[string]any{block}, stop)
			}))
			defer gateway.Close()
			ledger := &workflows.SkillEvidence{}
			var evidenceErr error
			authorize := func(_ context.Context, o workflows.SkillChildObservation) error {
				if err := ledger.InvocationComplete(plan); err != nil {
					return err
				}
				if permits.Add(1) != 1 {
					return fmt.Errorf("original allowance consumed")
				}
				if err := ledger.ObserveChild(plan, o); err != nil {
					return err
				}
				if scenario == "lost-permit-ack" {
					return fmt.Errorf("lost original durable permit acknowledgement")
				}
				return nil
			}
			request := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{Skill: plan, SkillRoot: plugin, AuthorizeSkillChild: authorize}}
			skill, err := skillBridgeInput(request)
			if err != nil {
				t.Fatal(err)
			}
			bridge, err := filepath.Abs("../../bridge/claude/bridge.mjs")
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "engine") {
				t.Setenv("CHILD_ENGINE_FIXTURE_KEY", "synthetic-loopback-only")
				runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {Provider: "claude-sdk", Bridge: bridge, Model: plan.Model, Tools: []string{"Read"}, Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "CHILD_ENGINE_FIXTURE_KEY"}}}}}
				cutStage := ""
				if strings.HasPrefix(scenario, "engine-cut-") {
					cutStage = strings.TrimPrefix(scenario, "engine-cut-")
				}
				runSkillChildEngineFixture(t, root, plugin, runner, child, cutStage, scenario == "engine-dot-resources")
				if cutStage != "" {
					wantChild := int32(1)
					if cutStage == "final-parent" {
						wantChild = 2
					}
					if parentCalls.Load() < 2 || parentCalls.Load() > 3 || cutStage == "final-parent" && parentCalls.Load() != 3 || childCalls.Load() != wantChild {
						t.Fatal("unknown child attempt retried", parentCalls.Load(), childCalls.Load())
					}
					return
				}
				if parentCalls.Load() != 4 || childCalls.Load() != 2 || !sawSkill.Load() || !sawResource.Load() || hostile.Load() || wrongEffort.Load() {
					t.Fatal("engine native controls differ", parentCalls.Load(), childCalls.Load(), sawSkill.Load(), sawResource.Load())
				}
				return
			}
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "node", bridge)
			command.Dir = root
			state := filepath.Join(root, "state")
			if err = os.MkdirAll(filepath.Join(state, "home"), 0700); err != nil {
				t.Fatal(err)
			}
			command.Env = append(providerEnvironment(""), "HOME="+filepath.Join(state, "home"), "CLAUDE_CONFIG_DIR="+state, "ANTHROPIC_API_KEY=synthetic-loopback-only", "ANTHROPIC_BASE_URL="+gateway.URL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "ENABLE_CLAUDEAI_MCP_SERVERS=false", "MAX_STRUCTURED_OUTPUT_RETRIES=0", "CLAUDE_CODE_MAX_RETRIES=0")
			for _, base := range []string{filepath.Join(root, ".claude"), state, filepath.Join(state, "home/.claude")} {
				p := filepath.Join(base, "agents", child.Type+".md")
				if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(p, []byte("---\nname: "+child.Type+"\ndescription: HOSTILE_CHILD_CATALOG\n---\nHOSTILE_CHILD_BODY"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic, original bytes.Buffer
			command.Stderr = &diagnostic
			cleanup, err := process.Supervise(command)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			observe := func(event workflows.ProviderEvent) {
				data, _ := json.Marshal(event.Data)
				if event.Type == "skill_evidence" {
					var o workflows.SkillObservation
					if e := json.Unmarshal(data, &o); e != nil {
						evidenceErr = e
					} else if e = ledger.Observe(plan, o); e != nil {
						evidenceErr = e
					}
				}
				if event.Type == "skill_attempt_uncertainty" {
					var o workflows.SkillAttemptUncertainty
					if e := json.Unmarshal(data, &o); e != nil {
						evidenceErr = e
					} else if e = ledger.ObserveUncertainAttempt(plan, o); e != nil {
						evidenceErr = e
					}
				}
				if event.Type == "skill_child_evidence" {
					var o workflows.SkillChildObservation
					if e := json.Unmarshal(data, &o); e != nil {
						evidenceErr = e
					} else if e = ledger.ObserveChild(plan, o); e != nil {
						evidenceErr = e
					}
				}
			}
			result, protocolErr := bridgeTurn(ctx, io.TeeReader(stdout, &original), &bridgeWriter{writer: stdin}, map[string]any{"workspace": root, "model": plan.Model, "prompt": "Use the admitted skill and explicit child.", "tools": []string{"Read", "Skill", "Agent"}, "skill": skill}, nil, observe, cleanup, authorize)
			stdin.Close()
			waitErr := command.Wait()
			t.Logf("scenario=%s parentAPI=%d childAPI=%d permits=%d protocol=%v exit=%v ledger=%v stderr=%s original=%s", scenario, parentCalls.Load(), childCalls.Load(), permits.Load(), protocolErr, waitErr, ledger.Complete(plan), diagnostic.String(), original.String())
			if hostile.Load() || wrongEffort.Load() || ctx.Err() != nil || evidenceErr != nil {
				t.Fatal("native control mismatch", hostile.Load(), wrongEffort.Load(), ctx.Err(), evidenceErr)
			}
			if _, statErr := os.Stat(filepath.Join(root, "FORBIDDEN_NATIVE_CHILD_EXEC")); !os.IsNotExist(statErr) {
				t.Fatal("unadmitted native shell executed", statErr)
			}
			if strings.HasPrefix(scenario, "socket-cut-") {
				wantParent, wantChild, wantPermit := int32(1), int32(0), int32(0)
				if scenario == "socket-cut-child" {
					wantParent, wantChild, wantPermit = parentCalls.Load(), 1, 1
					if wantParent < 2 || wantParent > 3 {
						t.Fatal("unknown attempt dispatched extra parent requests", wantParent)
					}
				}
				if scenario == "socket-cut-final-parent" {
					wantParent, wantChild, wantPermit = 3, 2, 1
					raw, _ := result.Outputs["native_child_result"].(map[string]any)
					update, err := workflows.SkillChildUsage(raw)
					usage, _ := update.Usage["total"].(map[string]any)
					if err == nil || !update.Incomplete || fmt.Sprint(usage["input_tokens"]) != "64" || fmt.Sprint(usage["output_tokens"]) != "20" {
						t.Fatal("known child partials discarded", result.Outputs, update, err)
					}
				}
				if protocolErr == nil || parentCalls.Load() != wantParent || childCalls.Load() != wantChild || permits.Load() != wantPermit || ledger.Complete(plan) == nil || result.Outputs["usage_incomplete"] != true {
					t.Fatal("unknown native attempt settled or retried", result, protocolErr, parentCalls.Load(), childCalls.Load(), permits.Load())
				}
				return
			}
			if scenario == "lost-permit-ack" {
				if protocolErr == nil || childCalls.Load() != 0 || permits.Load() != 1 {
					t.Fatal("lost permit acknowledgement dispatched child", result, protocolErr, childCalls.Load())
				}
				return
			}
			if protocolErr != nil || waitErr != nil {
				t.Fatal(result, protocolErr, waitErr, diagnostic.String())
			}
			if scenario == "foreground" {
				if parentCalls.Load() != 3 || childCalls.Load() != 2 || permits.Load() != 1 || ledger.Complete(plan) != nil || !sawSkill.Load() || !sawResource.Load() {
					t.Fatal("missing original foreground child completion", result, ledger.Complete(plan))
				}
				raw, _ := result.Outputs["native_child_result"].(map[string]any)
				update, e := workflows.SkillChildUsage(raw)
				total, _ := update.Usage["total"].(map[string]any)
				if e != nil || fmt.Sprint(total["input_tokens"]) != "80" || fmt.Sprint(total["output_tokens"]) != "25" {
					t.Fatal("native child usage omitted", update, e)
				}
			} else if scenario != "second-call" && ledger.Complete(plan) == nil {
				t.Fatal("denied/exhausted child completed original ledger", scenario)
			}
			if scenario == "background-missing" || scenario == "background-true" || scenario == "builtin" || scenario == "override" || scenario == "isolation-override" || scenario == "resume-override" {
				if childCalls.Load() != 0 || permits.Load() != 0 {
					t.Fatal("unadmitted child dispatched", scenario, childCalls.Load(), permits.Load())
				}
			}
			if scenario == "nested" || scenario == "forbidden-read" || scenario == "exhaustion" || scenario == "forbidden-tool" || scenario == "child-model-mismatch" || scenario == "second-call" {
				if childCalls.Load() != 2 || permits.Load() != 1 {
					t.Fatal("child controls did not bound execution", scenario, childCalls.Load(), permits.Load())
				}
			}
		})
	}
}

func runSkillChildEngineFixture(t *testing.T, root, plugin string, runner *Runner, child workflows.SkillChild, cutStage string, dotResources bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`on: {workflow_call: {inputs: {selected: {type: skill, required: true}}}}
jobs:
  work:
    steps:
      - id: selected
        uses: agent/run
        with:
          agent: reviewer.yaml
          prompt: Use the admitted skill and one child.
          skill:
            input: selected
            arguments: exact
            resources: [reference.txt]
            evidence: {invocation: required, children: [review]}
            children:
              - {name: review, prompt: %q, description: %q, tools: [Read], resources: [reference.txt], model: inherit, max-turns: 2, max-calls: 1, background: false}
      - id: continuation
        uses: agent/run
        with:
          agent: '${{ steps.selected.outputs.agent }}'
          prompt: Continue the original task without another child.
`, child.Prompt, child.Description)
	if dotResources {
		source = strings.ReplaceAll(source, "resources: [reference.txt]", "resources: [./reference.txt]")
	}
	for name, text := range map[string]string{"main.yml": source, "reviewer.yaml": "name: reviewer\ndescription: Review retained resources.\ninstructions: Follow the admitted task.\nmodel: claude-sonnet-4-6\n"} {
		if err := os.WriteFile(filepath.Join(root, "workflows", name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(store.Dir, "assets"), func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	slots := make(chan struct{}, 1)
	workers := &workflows.WorkerManager{Store: store, Slots: slots, Factory: func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
		return runner, nil, nil, nil
	}}
	if err = workers.Init(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); workers.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	executor := &workflows.Executor{WorkspaceDir: root, Agents: runner, Store: store, Workers: workers, Slots: slots, BeforeFinalize: workers.StopRun}
	faultDB, err := sql.Open("sqlite", filepath.Join(store.Dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer faultDB.Close()
	if cutStage == "" {
		if _, err = faultDB.Exec(`CREATE TRIGGER lose_child_step_completion BEFORE UPDATE OF status ON attempts WHEN NEW.status='succeeded' BEGIN SELECT RAISE(ABORT,'lost child step completion'); END`); err != nil {
			t.Fatal(err)
		}
	}
	run, err := executor.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"selected": map[string]any{"directory": filepath.Join(plugin, "skills/probe")}}})
	if err == nil || run == nil {
		t.Fatal("lost step completion not exercised", run, err)
	}
	if cutStage != "" {
		list, e := workers.List(ctx, run.ID)
		if e != nil || len(list) != 1 || !list[0].SkillTurnUncertain || list[0].Remaining != 31 || len(list[0].SkillChildPermits) != 1 || len(list[0].SkillEvidence.UncertainAttempts) == 0 {
			t.Fatal("unknown child attempt lost durable hold", list, e)
		}
		report, e := store.Usage(ctx, run.ID)
		if e != nil || report.Summary.Complete {
			t.Fatal("unknown child usage settled", report, e)
		}
		if cutStage == "final-parent" && (report.Summary.InputTokens == nil || *report.Summary.InputTokens != 64 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 20 || report.Summary.KnownCostUSD == nil || math.Abs(*report.Summary.KnownCostUSD-0.000492) > 1e-12 || report.Summary.CostComplete) {
			t.Fatal("late parent failure discarded known parent+child charges", report.Summary)
		}
		// Reopen SQLite independently to verify that the hold and charges were committed.
		fresh, e := workflows.OpenSQLite(store.Dir)
		if e != nil {
			t.Fatal(e)
		}
		defer fresh.Close()
		fw := &workflows.WorkerManager{Store: fresh}
		retained, e := fw.List(ctx, run.ID)
		if e != nil || len(retained) != 1 || !retained[0].SkillTurnUncertain || retained[0].Remaining != 31 {
			t.Fatal("hold was not durable", retained, e)
		}
		beforeGeneration := run.Generation
		if _, e = executor.Resume(ctx, run.ID, nil, true); e == nil {
			t.Fatal("explicit retry replayed unknown child effects")
		}
		after, _, e := store.Load(ctx, run.ID)
		if e != nil || after.Generation != beforeGeneration {
			t.Fatal("blocked retry claimed new generation", after, e)
		}
		retained, e = fw.List(ctx, run.ID)
		if e != nil || len(retained) != 1 || retained[0].Remaining != 31 || len(retained[0].SkillChildPermits) != 1 {
			t.Fatal("blocked retry reset original allowance/permit", retained, e)
		}
		if cutStage == "final-parent" {
			again, e := fresh.Usage(ctx, run.ID)
			if e != nil || again.Summary.Complete || again.Summary.KnownCostUSD == nil || math.Abs(*again.Summary.KnownCostUSD-0.000492) > 1e-12 {
				t.Fatal("restart/retry lost known original cost", again, e)
			}
		}
		t.Logf("native consumed child socket cut: original worker=%s session=%s remaining31 permit1 usageIncomplete=true, fresh SQLite hold and explicit retry rejected before claim", list[0].ID, list[0].SessionID)
		return
	}
	list, err := workers.List(ctx, run.ID)
	if err != nil || len(list) != 1 || list[0].SkillTurnUncertain || list[0].Remaining != 31 || len(list[0].SkillChildPermits) != 1 || list[0].SkillEvidence.Complete(list[0].Skill) != nil {
		t.Fatal("engine original child did not complete", list, err)
	}
	if dotResources && (list[0].Skill.Resources[0] != "reference.txt" || list[0].Skill.Children[0].Resources[0] != "reference.txt") {
		t.Fatal("parent/child resource identities were not canonical", list[0].Skill)
	}
	rows, err := store.ToolCalls(ctx, run.ID, false)
	if err != nil || len(rows) != 1 || rows[0].Status != "completed" || !strings.Contains(string(rows[0].Result), "FULL_ORIGINAL_CHILD_OUTPUT_139") {
		t.Fatal("engine omitted original child outputs", rows, err)
	}
	if err = os.RemoveAll(filepath.Join(plugin, "skills/probe")); err != nil {
		t.Fatal(err)
	}
	if _, err = faultDB.Exec("DROP TRIGGER lose_child_step_completion"); err != nil {
		t.Fatal(err)
	}
	resumed, err := executor.Resume(ctx, run.ID, nil, true)
	if err != nil || resumed.Status != workflows.RunStatusSucceeded {
		t.Fatal("engine replayed instead of restoring original child completion", resumed, err)
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil || !report.Summary.Complete || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 96 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 30 {
		t.Fatal("engine lost original cumulative child usage", report, err)
	}
	continued, err := workers.List(ctx, run.ID)
	if err != nil || len(continued) != 1 || continued[0].Remaining != 30 || continued[0].SessionID != list[0].SessionID || len(continued[0].SkillChildPermits) != 1 || continued[0].SkillTurnUncertain || len(continued[0].SkillEvidence.Children) != len(list[0].SkillEvidence.Children) {
		t.Fatal("fresh continuation changed original child/session/allowance", continued, err)
	}
	messages, err := workers.Messages(ctx, run.ID, list[0].ID)
	if err != nil || len(messages) != 2 || messages[0].ID != list[0].SkillChildPermits[0].InvocationID || messages[0].Status != workflows.RunStatusSucceeded || messages[1].Status != workflows.RunStatusSucceeded {
		t.Fatal("original child turn was replayed", messages, err)
	}
	summary, _ := json.Marshal(report.Summary)
	t.Logf("engine original worker=%s session=%s child=%s permit=%s usage=%s removed-source recovery restored original turn; intended fresh-process continuation consumed one more parent turn (remaining30), retained one original child permit", list[0].ID, list[0].SessionID, list[0].Skill.Children[0].Type, rows[0].CallID, summary)
}
