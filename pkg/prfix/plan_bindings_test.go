package prfix

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPlanBindingsRecheckArtifactBytesAndReturnIndependentValues(t *testing.T) {
	runtime := &Runtime{Root: t.TempDir()}
	plan := Plan{Version: Version, LogicalRun: strings.Repeat("a", 32), Instructions: "before", Identity: Identity{Repository: "github.com/fixture/repo"}}
	path, err := artifact(runtime.Root, plan)
	if err != nil {
		t.Fatal(err)
	}
	input := ActionInput{Plan: path}
	cache := &planBindingCache{}
	want := jsonBytes(map[string]any{"plan": digest(plan), "logical_run": plan.LogicalRun, "repository": plan.Identity.Repository, "root": runtime.Root})
	binding, err := cache.bindings(runtime, input)
	if err != nil || !bytes.Equal(binding, want) {
		t.Fatal(string(binding), err)
	}
	// A caller can own and modify its result without changing later observations.
	for i := range binding {
		binding[i] = 'x'
	}
	binding, err = cache.bindings(runtime, input)
	if err != nil || !bytes.Equal(binding, want) {
		t.Fatal("returned value aliased a later binding", string(binding), err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(original, []byte("before"), []byte("change"), 1)
	if len(changed) != len(original) || bytes.Equal(changed, original) {
		t.Fatal("fixture must replace bytes without changing file size")
	}
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.bindings(runtime, input); err == nil {
		t.Fatal("accepted modified artifact with unchanged size and mtime")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.bindings(runtime, input); err == nil {
		t.Fatal("reused bindings after artifact deletion")
	}
}

func TestPlanBindingsPreserveNormalizationAndStrictPlanValidation(t *testing.T) {
	runtime := &Runtime{Root: t.TempDir()}
	plan := Plan{Version: Version, LogicalRun: strings.Repeat("b", 32), Instructions: "<review>"}
	path, err := artifact(runtime.Root, plan)
	if err != nil {
		t.Fatal(err)
	}
	indented, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, indented, 0600); err != nil {
		t.Fatal(err)
	}
	cache := &planBindingCache{}
	if _, err := cache.bindings(runtime, ActionInput{Plan: path}); err != nil {
		t.Fatal("existing normalized identities must remain valid", err)
	}
	for _, value := range []any{
		map[string]any{"version": Version, "logical_run": plan.LogicalRun, "unexpected": true},
		Plan{Version: "unsupported", LogicalRun: plan.LogicalRun},
		Plan{Version: Version, LogicalRun: "invalid"},
	} {
		bad, err := artifact(runtime.Root, value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cache.bindings(runtime, ActionInput{Plan: bad}); err == nil {
			t.Fatalf("accepted invalid plan: %+v", value)
		}
	}
	outside := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(outside, indented, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.bindings(runtime, ActionInput{Plan: outside}); err == nil {
		t.Fatal("accepted identical bytes outside owned artifacts")
	}
}

func TestPlanBindingsConcurrentPlansKeepTheirOwnIdentities(t *testing.T) {
	cache := &planBindingCache{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		runtime := &Runtime{Root: t.TempDir()}
		plan := Plan{Version: Version, LogicalRun: strings.Repeat("c", 32)}
		path, err := artifact(runtime.Root, plan)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				binding, err := cache.bindings(runtime, ActionInput{Plan: path})
				if err != nil {
					t.Error(err)
					return
				}
				var value map[string]any
				if err := json.Unmarshal(binding, &value); err != nil || value["root"] != runtime.Root || value["plan"] != digest(plan) {
					t.Error("concurrent plan bindings were mixed", string(binding), err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
