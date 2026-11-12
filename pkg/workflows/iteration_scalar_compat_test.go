package workflows

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// Both runtimes consume these scalars to check the browser's strict integer
// boundary against yaml.v3 resolution and Go's positive int decoding.
func TestIterationConcurrencyScalarCompatibility(t *testing.T) {
	data, err := os.ReadFile("../../testdata/ui-compat/iteration-concurrency-scalars.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Source string `json:"source"`
		Value  string `json:"value"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Source, func(t *testing.T) {
			source := fmt.Sprintf("jobs: {review: {uses: workflows/child.yml, for-each: {items: [], key: item.id, concurrency: %s}}}", c.Source)
			workflow, err := Parse([]byte(source))
			if c.Value == "" {
				if err == nil {
					t.Fatal("invalid bound accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(workflow); err != nil {
				t.Fatal(err)
			}
			if bound := workflow.Jobs["review"].ForEach.Concurrency; bound == nil || strconv.Itoa(*bound) != c.Value {
				t.Fatalf("bound = %v, want %s", bound, c.Value)
			}
		})
	}
}
