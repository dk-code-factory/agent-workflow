package workflows

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCanonicalAgentSyntaxRejectsRemovedForms(t *testing.T) {
	for _, step := range []string{
		`agent: reviewer, prompt: hi`,
		`uses: agent/reviewer, with: {prompt: hi}`,
		`uses: agent/run, prompt: hi, with: {agent: reviewer.toml}`,
		`uses: agent/run, with: {prompt: hi}`,
		`uses: agent/run, with: {agent: reviewer.toml, prompt: hi, session: public}`,
		`uses: agent/run, with: {agent: reviewer.toml, prompt: hi, provider-session-id: public}`,
	} {
		w, err := Parse([]byte("jobs: {review: {steps: [{" + step + "}]}}"))
		if err == nil {
			err = Validate(w)
		}
		if err == nil {
			t.Errorf("accepted removed or invalid syntax: %s", step)
		}
	}
	for _, step := range []string{
		`uses: agent/run, with: {agent: reviewer.toml, prompt: hi}`,
		`uses: agent/run, with: {agent: '${{ steps.first.outputs.agent }}', prompt: hi}`,
	} {
		w, err := Parse([]byte("jobs: {review: {steps: [{" + step + "}]}}"))
		if err == nil {
			err = Validate(w)
		}
		if err != nil && !strings.Contains(err.Error(), "first") {
			t.Fatal(err)
		}
	}
}

func TestCanonicalPromptRetainsQuotedTimestamp(t *testing.T) {
	data, err := os.ReadFile("../../testdata/ui-compat/concise-timestamps.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Value     string
		Timestamp bool
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		quoted, _ := json.Marshal(tc.Value)
		w, err := Parse([]byte("jobs: {review: {steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: " + string(quoted) + "}}]}}"))
		if err != nil {
			t.Fatal(err)
		}
		if err = Validate(w); err != nil {
			t.Fatal(err)
		}
		if w.Jobs["review"].Steps[0].With["prompt"] != tc.Value {
			t.Fatal(tc.Value)
		}
	}
}
