package workflows

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSharedContainerOrdinaryCommandJSONInputAndOutput(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{true: "valid", false: "invalid"}[valid], func(t *testing.T) {
			e, _, d := containerExecutor(t)
			var captured []byte
			d.helper = func(_ context.Context, argv []string, input []byte, _ []string) ([]byte, []byte, int, bool) {
				if len(argv) != 1 || argv[0] != "capture-json" {
					return nil, nil, 0, false
				}
				captured = append([]byte(nil), input...)
				if valid {
					return input, nil, 0, true
				}
				return []byte(`{"value":42}`), nil, 0, true
			}
			writeWorkflow(t, e.WorkspaceDir, "test.yml", `jobs:
  capture:
    container: ubuntu:24.04
    steps:
      - id: value
        argv: [capture-json]
        with:
          input: {value: "literal '$data'", nested: [true, 9007199254740993]}
          output:
            format: json
            schema:
              type: object
              properties: {value: {type: string}}
              required: [value]
`)
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
			if valid {
				if err != nil {
					t.Fatal(err)
				}
				structured := run.Jobs["capture"].Steps["value"].Outputs["structured"]
				var expected map[string]any
				decoder := json.NewDecoder(strings.NewReader(string(captured)))
				decoder.UseNumber()
				if err := decoder.Decode(&expected); err != nil || !reflect.DeepEqual(structured, expected) {
					t.Fatalf("input/output lost JSON values: %+v != %+v: %v", structured, expected, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "invalid command structured output") {
				t.Fatalf("invalid command output accepted: %v", err)
			}
			if len(captured) == 0 || !strings.Contains(string(captured), "9007199254740993") {
				t.Fatalf("container did not receive exact JSON: %q", captured)
			}
		})
	}
}
