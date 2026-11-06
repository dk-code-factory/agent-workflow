package workflowdef

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedExamples(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "workflow-definition")
	for _, name := range []string{"review.workflow.json", "conversation.workflow.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err != nil {
				t.Fatal(err)
			}
		})
	}
	data, err := os.ReadFile(filepath.Join(root, "failure-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]json.RawMessage
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(data); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRejectMalformedDefinitions(t *testing.T) {
	for name, data := range map[string]string{
		"duplicate root":        `{"version":"0.2","version":"0.2","steps":[{"value":1}]}`,
		"duplicate nested":      `{"version":"0.2","steps":[{"value":{"a":1,"a":2}}]}`,
		"multiple documents":    `{"version":"0.2","steps":[{"value":1}]} {}`,
		"unknown field":         `{"version":"0.2","extra":true,"steps":[{"value":1}]}`,
		"version":               `{"version":"0.3","steps":[{"value":1}]}`,
		"ambiguous operation":   `{"version":"0.2","steps":[{"agent":"a","prompt":"x","value":1}]}`,
		"bad schema":            `{"version":"0.2","schemas":{"X":{"type":42}},"steps":[{"value":1}]}`,
		"unresolved schema":     `{"version":"0.2","steps":[{"agent":"a","prompt":"x","output":"Missing"}]}`,
		"schema ref":            `{"version":"0.2","schemas":{"X":{"$ref":"#/$defs/missing"}},"steps":[{"value":1}]}`,
		"remote schema":         `{"version":"0.2","schemas":{"X":{"$ref":"https://example.invalid/x"}},"steps":[{"value":1}]}`,
		"nested resource":       `{"version":"0.2","schemas":{"X":{"properties":{"x":{"$id":"urn:other","type":"string"}}}},"steps":[{"value":1}]}`,
		"dialect":               `{"version":"0.2","schemas":{"X":{"$schema":"http://json-schema.org/draft-07/schema#"}},"steps":[{"value":1}]}`,
		"duplicate resource ID": `{"version":"0.2","schemas":{"X":{"$id":"urn:x"},"Y":{"$id":"urn:x"}},"steps":[{"value":1}]}`,
		"reserved source":       `{"version":"0.2","sources":{"steps":{"value":1}},"steps":[{"value":1}]}`,
		"duplicate IDs":         `{"version":"0.2","steps":[{"id":"x","value":1},{"id":"x","value":2}]}`,
		"forward result":        `{"version":"0.2","steps":[{"value":"$.steps.later"},{"id":"later","value":1}]}`,
		"parallel visibility":   `{"version":"0.2","steps":[{"parallel":[{"id":"a","value":1},{"value":"$.steps.a"}]}]}`,
		"loop visibility":       `{"version":"0.2","steps":[{"repeat":2,"steps":[{"id":"a","value":1}]},{"value":"$.steps.a"}]}`,
		"context":               `{"version":"0.2","steps":[{"value":"$.error"}]}`,
		"invalid selector":      `{"version":"0.2","steps":[{"value":"$.input["}]}`,
		"invalid placeholder":   `{"version":"0.2","steps":[{"agent":"a","prompt":"{{ $.input.task"}]}`,
		"recovery context":      `{"version":"0.2","steps":[{"recover":"helper"}]}`,
		"conversation member":   `{"version":"0.2","steps":[{"conversation":["a","b"],"prompt":"x","finishBy":"outside"}]}`,
		"conversation start":    `{"version":"0.2","steps":[{"conversation":["a","b"],"prompt":"x","start":"outside","finishBy":"b"}]}`,
		"bad breaker":           `{"version":"0.2","steps":[{"agent":"a","prompt":"x","breaker":{"failures":0,"cooldownMs":1}}]}`,
		"ambiguous breaker":     `{"version":"0.2","steps":[{"agent":"a","prompt":"x","breaker":{"agent":"b","run":["script"]}}]}`,
		"policy conflict":       `{"version":"0.2","steps":[{"agent":"a","prompt":"x","breaker":{"failures":1,"cooldownMs":1}},{"agent":"a","prompt":"x","breaker":{"failures":2,"cooldownMs":1}}]}`,
		"credential":            `{"version":"0.2","sources":{"config":{"value":1}},"steps":[{"agent":"a","prompt":"x","credentials":{"api":"config"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(data)); err == nil {
				t.Fatal("accepted invalid workflow")
			}
		})
	}
}

func TestSchemaAnnotationsAreData(t *testing.T) {
	data := `{"version":"0.2","schemas":{"X":{"type":"object","examples":[{"$ref":"https://ordinary.example/data","$id":"not-a-resource"}]}},"steps":[{"value":1}]}`
	if _, err := Parse([]byte(data)); err != nil {
		t.Fatal(err)
	}
}

func TestCustomBreakerForms(t *testing.T) {
	for _, policy := range []string{`{"agent":"supervisor"}`, `{"agent":"supervisor","prompt":"Inspect {{ $.breaker }}"}`, `{"run":["python3","breaker.py"]}`} {
		data := `{"version":"0.2","steps":[{"agent":"worker","prompt":"work","breaker":` + policy + `}]}`
		if _, err := Parse([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecoderBounds(t *testing.T) {
	for _, data := range []string{strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66), strings.Repeat(" ", MaxDocumentBytes+1)} {
		if _, err := DecodeJSON([]byte(data)); err == nil {
			t.Fatal("accepted oversized JSON")
		}
	}
}

func TestQuotedPlaceholderDelimiter(t *testing.T) {
	data := `{"version":"0.2","steps":[{"agent":"a","prompt":"Inspect {{ $.input['a}}b'] }}"}]}`
	if _, err := Parse([]byte(data)); err != nil {
		t.Fatal(err)
	}
}
