package workflows

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestParseAgentOutputContractDefaultsRepairAttempts(t *testing.T) {
	contract, err := ParseAgentOutputContract("json")
	if err != nil {
		t.Fatalf("ParseAgentOutputContract(string) error = %v", err)
	}
	if contract == nil || contract.Format != "json" || contract.RepairAttempts != 1 {
		t.Fatalf("contract = %#v, want json with one repair attempt", contract)
	}

	contract, err = ParseAgentOutputContract(map[string]any{
		"schema": map[string]any{
			"type": "object",
		},
		"repair_attempts": -1,
	})
	if err != nil {
		t.Fatalf("ParseAgentOutputContract(map) error = %v", err)
	}
	if contract.Format != "json" || contract.RepairAttempts != 1 {
		t.Fatalf("contract = %#v, want inferred json with default repair attempt", contract)
	}
}

func TestAgentRequestDetachStructuredContractOwnsPatternCache(t *testing.T) {
	t.Parallel()

	original := &AgentOutputContract{
		Format: "json",
		Schema: map[string]any{"type": "string"},
		patterns: map[string]*regexp.Regexp{
			"^ok$": regexp.MustCompile("^ok$"),
		},
	}
	request := AgentRequest{Output: original}
	detachedRequest, detached := request.DetachStructuredContract()
	if detached == nil || detached == original || detachedRequest.Output != detached {
		t.Fatalf("detached request/contract = %#v / %#v", detachedRequest, detached)
	}
	delete(detached.patterns, "^ok$")
	if original.patterns["^ok$"] == nil {
		t.Fatal("detached contract shared its pattern map")
	}

	empty, contract := (AgentRequest{}).DetachStructuredContract()
	if empty.Output != nil || contract != nil {
		t.Fatalf("nil contract detach = %#v / %#v", empty, contract)
	}
}

func TestParseAgentOutputContractRejectsUnsupportedFormat(t *testing.T) {
	_, err := ParseAgentOutputContract(map[string]any{"format": "xml"})
	if err == nil {
		t.Fatal("ParseAgentOutputContract() error = nil, want unsupported format error")
	}
}

func TestValidateAgentStructuredOutputParsesAndValidatesSchema(t *testing.T) {
	contract := &AgentOutputContract{
		Format: "json",
		Schema: map[string]any{
			"type":     "object",
			"required": []any{"summary", "findings"},
			"properties": map[string]any{
				"summary": map[string]any{"type": "string"},
				"findings": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":     "object",
						"required": []any{"severity", "title"},
						"properties": map[string]any{
							"severity": map[string]any{
								"type": "string",
								"enum": []any{"high", "low"},
							},
							"title": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	result := ValidateAgentStructuredOutput(
		"```json\n{\"summary\":\"ok\",\"findings\":[{\"severity\":\"high\",\"title\":\"bug\"}]}\n```",
		contract,
	)
	if !result.Valid {
		t.Fatalf("structured output valid = false: %s", result.Error)
	}
	parsed, ok := result.Structured.(map[string]any)
	if !ok || parsed["summary"] != "ok" {
		t.Fatalf("structured output = %#v, want parsed object", result.Structured)
	}
}

func TestValidateAgentStructuredOutputRejectsSchemaMismatch(t *testing.T) {
	contract := &AgentOutputContract{
		Format: "json",
		Schema: map[string]any{
			"type":     "object",
			"required": []any{"summary"},
			"properties": map[string]any{
				"summary": map[string]any{"type": "string"},
			},
		},
	}

	result := ValidateAgentStructuredOutput(`{"findings":[]}`, contract)
	if result.Valid {
		t.Fatalf("structured output valid = true, want schema error")
	}
	if result.Error == "" {
		t.Fatalf("structured output error is empty")
	}
}

func TestValidateAgentStructuredOutputEnforcesMinimumStringLength(t *testing.T) {
	contract := &AgentOutputContract{Format: "json", Schema: map[string]any{
		"type": "object", "required": []any{"symbol"},
		"properties": map[string]any{
			"symbol": map[string]any{"type": "string", "minLength": 1},
		},
	}}
	if result := ValidateAgentStructuredOutput(`{"symbol":""}`, contract); result.Valid {
		t.Fatalf("empty required symbol passed minLength: %#v", result)
	}
	if result := ValidateAgentStructuredOutput(`{"symbol":"Save"}`, contract); !result.Valid {
		t.Fatalf("nonempty symbol failed minLength: %#v", result)
	}
}

func TestValidateAgentStructuredOutputEnforcesAdditionalProperties(t *testing.T) {
	contract := &AgentOutputContract{
		Format: "json",
		Schema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"category": map[string]any{"type": "string"},
			},
		},
	}

	result := ValidateAgentStructuredOutput(
		`{"category":"bug","model_prose":"post this verbatim"}`,
		contract,
	)
	if result.Valid {
		t.Fatalf("structured output valid = true, want additional property error")
	}
	if !strings.Contains(result.Error, "$.model_prose is not allowed") {
		t.Fatalf("structured output error = %q, want rejected property path", result.Error)
	}

	contract.Schema["additionalProperties"] = map[string]any{"type": "boolean"}
	result = ValidateAgentStructuredOutput(
		`{"category":"bug","comment":true}`,
		contract,
	)
	if !result.Valid {
		t.Fatalf("schema-valued additional property rejected: %s", result.Error)
	}
	result = ValidateAgentStructuredOutput(
		`{"category":"bug","comment":"yes"}`,
		contract,
	)
	if result.Valid {
		t.Fatalf("invalid schema-valued additional property accepted: %#v", result)
	}
}

func TestValidateAgentStructuredOutputEnforcesMaximumsAndNumericMinimum(t *testing.T) {
	contract := &AgentOutputContract{Format: "json", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label": map[string]any{"type": "string", "maxLength": json.Number("4")},
			"score": map[string]any{"type": "number", "minimum": 2.5},
			"items": map[string]any{"type": "array", "maxItems": int64(1)},
		},
	}}
	for _, raw := range []string{
		`{"label":"longer","score":3,"items":[]}`,
		`{"label":"ok","score":2,"items":[]}`,
		`{"label":"ok","score":"three","items":[]}`,
		`{"label":"ok","score":3,"items":[1,2]}`,
	} {
		if result := ValidateAgentStructuredOutput(raw, contract); result.Valid {
			t.Fatalf("structured output %s unexpectedly passed bounded schema", raw)
		}
	}
	if result := ValidateAgentStructuredOutput(
		`{"label":"four","score":2.5,"items":[1]}`,
		contract,
	); !result.Valid {
		t.Fatalf("bounded structured output failed: %#v", result)
	}

	for _, test := range []struct {
		value any
		want  int
	}{
		{value: 2, want: 2},
		{value: int64(3), want: 3},
		{value: float64(4), want: 4},
		{value: json.Number("5"), want: 5},
	} {
		if got := schemaIntegerKeyword(map[string]any{"limit": test.value}, "limit"); got != test.want {
			t.Fatalf("schema integer %T = %d, want %d", test.value, got, test.want)
		}
	}
}

func TestValidateAgentStructuredOutputChecksArrayItemSchema(t *testing.T) {
	contract := &AgentOutputContract{
		Format: "json",
		Schema: map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":     "object",
				"required": []any{"id"},
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
			},
		},
	}

	result := ValidateAgentStructuredOutput(`[{"id":1}]`, contract)
	if result.Valid {
		t.Fatalf("structured output valid = true, want item schema error")
	}
	if result.Error == "" {
		t.Fatalf("structured output error is empty")
	}
}

func TestValidateAgentStructuredOutputEnforcesNestedStringPattern(t *testing.T) {
	contract := &AgentOutputContract{Format: "json", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"candidateIds": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string", "pattern": `^cand_[0-9a-f]{64}$`,
				},
			},
		},
	}}
	validID := "cand_" + strings.Repeat("a", 64)
	if result := ValidateAgentStructuredOutput(
		`{"candidateIds":["`+validID+`","`+validID+`"]}`,
		contract,
	); !result.Valid {
		t.Fatalf("valid patterned IDs were rejected: %#v", result)
	}
	for _, value := range []string{
		"cand_unknown",
		"cand_" + strings.Repeat("A", 64),
		validID + "suffix",
		"prefix" + validID,
	} {
		result := ValidateAgentStructuredOutput(
			`{"candidateIds":["`+validID+`","`+value+`"]}`,
			contract,
		)
		if result.Valid || !strings.Contains(result.Error, "$.candidateIds[1] must match pattern") {
			t.Fatalf("pattern mismatch %q result=%#v", value, result)
		}
	}
}

func TestValidateAgentStructuredOutputRejectsInvalidPatternSchema(t *testing.T) {
	for _, pattern := range []any{1, "["} {
		result := ValidateAgentStructuredOutput(`"value"`, &AgentOutputContract{
			Format: "json",
			Schema: map[string]any{"type": "string", "pattern": pattern},
		})
		if result.Valid || !strings.Contains(result.Error, "pattern") {
			t.Fatalf("invalid pattern %#v result=%#v", pattern, result)
		}
	}
}

func TestParseAgentOutputContractRejectsHiddenInvalidPatterns(t *testing.T) {
	for _, schema := range []map[string]any{
		{
			"type": "object",
			"properties": map[string]any{
				"optional": map[string]any{"type": "string", "pattern": "["},
			},
		},
		{
			"type":  "array",
			"items": map[string]any{"type": "string", "pattern": "["},
		},
		{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string", "pattern": "["},
		},
	} {
		if _, err := ParseAgentOutputContract(map[string]any{
			"format": "json", "schema": schema,
		}); err == nil || !strings.Contains(err.Error(), "RE2") {
			t.Fatalf("hidden invalid pattern schema=%#v error=%v", schema, err)
		}
	}
}

func TestParseAgentOutputContractRejectsPatternPreflightBoundaries(t *testing.T) {
	deep := map[string]any{"type": "string", "pattern": "safe"}
	for range maxAgentOutputSchemaDepth + 1 {
		deep = map[string]any{"type": "array", "items": deep}
	}
	for _, schema := range []map[string]any{
		deep,
		{"type": "object", "additionalProperties": "not-a-schema"},
	} {
		if _, err := ParseAgentOutputContract(map[string]any{
			"format": "json", "schema": schema,
		}); err == nil {
			t.Fatalf("invalid preflight schema was accepted: %#v", schema)
		}
	}
	if err := validateJSONSchemaPattern(
		"safe", map[string]any{"pattern": "safe"}, "$", map[string]*regexp.Regexp{},
	); err == nil || !strings.Contains(err.Error(), "not compiled") {
		t.Fatalf("missing compiled pattern error=%v", err)
	}
}

func TestAgentOutputPatternValidationHelperBoundaries(t *testing.T) {
	if err := (*AgentOutputContract)(nil).Validate(); err != nil {
		t.Fatalf("nil contract validation error=%v", err)
	}
	contract := &AgentOutputContract{Format: "json", Schema: map[string]any{
		"type": "string", "pattern": "safe",
	}}
	if err := contract.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("cached contract validation error=%v", err)
	}
	if err := validateJSONSchemaValue("value", map[string]any{
		"type": "string", "pattern": "[",
	}, "$"); err == nil {
		t.Fatal("invalid direct schema pattern was accepted")
	}
	if err := validateJSONSchemaValueWithPatterns(
		"value", map[string]any{}, "$", map[string]*regexp.Regexp{},
	); err != nil {
		t.Fatalf("empty schema validation error=%v", err)
	}
	if err := validateJSONSchemaPattern(
		"value", map[string]any{}, "$", map[string]*regexp.Regexp{},
	); err != nil {
		t.Fatalf("unconfigured pattern validation error=%v", err)
	}
	if err := validateJSONSchemaAdditionalProperties(
		map[string]any{"extra": "value"}, nil,
		map[string]any{"additionalProperties": map[string]any{}},
		"$", map[string]*regexp.Regexp{},
	); err != nil {
		t.Fatalf("empty additional schema validation error=%v", err)
	}
}

func TestValidateAgentStructuredOutputEnforcesTypelessRE2Pattern(t *testing.T) {
	contract, err := ParseAgentOutputContract(map[string]any{
		"format": "json",
		"schema": map[string]any{"pattern": `^safe-[0-9]+$`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := ValidateAgentStructuredOutput(`"safe-42"`, contract); !result.Valid {
		t.Fatalf("typeless matching pattern was rejected: %#v", result)
	}
	if result := ValidateAgentStructuredOutput(`"unsafe"`, contract); result.Valid {
		t.Fatalf("typeless mismatching pattern was accepted: %#v", result)
	}
	if result := ValidateAgentStructuredOutput(`42`, contract); !result.Valid {
		t.Fatalf("pattern incorrectly constrained a non-string instance: %#v", result)
	}
	if _, err := ParseAgentOutputContract(map[string]any{
		"format": "json",
		"schema": map[string]any{"type": "string", "pattern": `^(?!admin$).+$`},
	}); err == nil || !strings.Contains(err.Error(), "RE2") {
		t.Fatalf("unsupported ECMA lookahead error=%v", err)
	}
	if _, err := ParseAgentOutputContract(map[string]any{
		"format": "json",
		"schema": map[string]any{
			"type": "string", "pattern": strings.Repeat("x", maxAgentOutputPatternBytes+1),
		},
	}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized pattern error=%v", err)
	}
}

func TestValidateAgentStructuredOutputEnforcesPatternOnAdditionalProperties(t *testing.T) {
	contract := &AgentOutputContract{Format: "json", Schema: map[string]any{
		"type": "object",
		"additionalProperties": map[string]any{
			"type": "string", "pattern": `^[a-z]+$`,
		},
	}}
	if result := ValidateAgentStructuredOutput(
		`{"first":"valid","second":"also"}`,
		contract,
	); !result.Valid {
		t.Fatalf("valid patterned additional properties were rejected: %#v", result)
	}
	result := ValidateAgentStructuredOutput(
		`{"first":"valid","second":"NOT-VALID"}`,
		contract,
	)
	if result.Valid || !strings.Contains(result.Error, "$.second must match pattern") {
		t.Fatalf("invalid patterned additional property result=%#v", result)
	}
}

func TestValidateAgentStructuredOutputChecksScalarSchemaTypes(t *testing.T) {
	tests := []struct {
		name   string
		schema map[string]any
		text   string
		valid  bool
	}{
		{
			name:   "integer",
			schema: map[string]any{"type": "integer"},
			text:   `3`,
			valid:  true,
		},
		{
			name:   "integer rejects fraction",
			schema: map[string]any{"type": "integer"},
			text:   `3.5`,
		},
		{
			name:   "number",
			schema: map[string]any{"type": "number"},
			text:   `3.5`,
			valid:  true,
		},
		{
			name:   "boolean",
			schema: map[string]any{"type": "boolean"},
			text:   `true`,
			valid:  true,
		},
		{
			name:   "enum",
			schema: map[string]any{"type": "string", "enum": []any{"low", "high"}},
			text:   `"high"`,
			valid:  true,
		},
		{
			name:   "enum rejects unknown",
			schema: map[string]any{"type": "string", "enum": []any{"low", "high"}},
			text:   `"medium"`,
		},
		{
			name:   "nullable type chooses non-null",
			schema: map[string]any{"type": []any{"null", "string"}},
			text:   `"ok"`,
			valid:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateAgentStructuredOutput(tt.text, &AgentOutputContract{
				Format: "json",
				Schema: tt.schema,
			})
			if result.Valid != tt.valid {
				t.Fatalf("Valid = %v, want %v; error=%s", result.Valid, tt.valid, result.Error)
			}
		})
	}
}

func TestCombineStructuredOutputsConcatenatesSchemaArrays(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary":  map[string]any{"type": "string"},
			"findings": map[string]any{"type": "array"},
		},
	}
	combined := CombineStructuredOutputs([]any{
		map[string]any{"summary": "one", "findings": []any{map[string]any{"id": "a"}}},
		map[string]any{"summary": "two", "findings": []any{map[string]any{"id": "b"}}},
	}, schema)
	obj, ok := combined.(map[string]any)
	if !ok {
		t.Fatalf("combined = %#v, want object", combined)
	}
	if obj["summary"] != "one\ntwo" {
		t.Fatalf("summary = %#v, want joined summaries", obj["summary"])
	}
	if got := len(obj["findings"].([]any)); got != 2 {
		t.Fatalf("findings len = %d, want 2", got)
	}
}

func TestCombineStructuredOutputsWrapsNonObjectSchemaResults(t *testing.T) {
	combined := CombineStructuredOutputs([]any{
		[]any{map[string]any{"id": "a"}},
		[]any{map[string]any{"id": "b"}},
	}, map[string]any{"type": "array"})
	obj, ok := combined.(map[string]any)
	if !ok {
		t.Fatalf("combined = %#v, want wrapper object", combined)
	}
	results, ok := obj["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("results = %#v, want two child results", obj["results"])
	}
}

func TestCompareStructuredOutputsMatchesStableObjectArrayIdentities(t *testing.T) {
	left := map[string]any{
		"summary": "grouped wording",
		"findings": []any{
			map[string]any{
				"file":     "src/auth.js",
				"line":     3,
				"severity": "high",
				"title":    "first wording",
				"evidence": "long explanation A",
			},
			map[string]any{
				"file":     "src/session.js",
				"line":     3,
				"severity": "high",
				"title":    "second wording",
				"evidence": "long explanation B",
			},
		},
	}
	right := map[string]any{
		"summary": "split wording",
		"findings": []any{
			map[string]any{
				"file":           "src/session.js",
				"line":           3,
				"severity":       "high",
				"title":          "different prose",
				"recommendation": "fix it",
			},
			map[string]any{
				"file":           "src/auth.js",
				"line":           3,
				"severity":       "high",
				"title":          "different prose too",
				"recommendation": "fix it",
			},
		},
	}

	comparison := CompareStructuredOutputs(left, right)
	if comparison["match"] != true {
		t.Fatalf("comparison = %#v, want stable identity match", comparison)
	}
	if comparison["method"] != "object_array_identities" {
		t.Fatalf("method = %#v, want object_array_identities", comparison["method"])
	}
}
