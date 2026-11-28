package clirunner

import (
	"context"
	"testing"
)

func TestSDKBrokerTerminalRequiresExactOriginalCounters(t *testing.T) {
	for _, tc := range []struct {
		name            string
		originalCreated int
		input           int
		created         any
		omitCreated     bool
		wantReject      bool
	}{
		{name: "original", originalCreated: 5, input: 11, created: 5},
		{name: "cache-creation-reclassified-as-input", originalCreated: 5, input: 16, created: 0, wantReject: true},
		{name: "input-reclassified-as-cache-creation", originalCreated: 5, input: 10, created: 6, wantReject: true},
		{name: "missing-counter-is-not-zero", input: 11, omitCreated: true, wantReject: true},
		{name: "unknown-counter-is-not-zero", input: 11, created: nil, wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := newSDKBrokerBatch(nil, nil, nil)
			proof := map[string]any{
				"response_id": "original-response", "model": "fixture", "stop_reason": "end_turn",
				"usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "cache_read_input_tokens": 2, "cache_creation_input_tokens": tc.originalCreated},
				"calls": []any{},
			}
			if err := batch.start(proof); err != nil {
				t.Fatal(err)
			}
			if err := batch.seal(context.Background(), proof, "fixture", "original-session"); err != nil {
				t.Fatal(err)
			}
			usage := map[string]any{
				"input_tokens": tc.input, "output_tokens": 3, "cache_read_input_tokens": 2, "cache_creation_input_tokens": tc.created,
			}
			if tc.omitCreated {
				delete(usage, "cache_creation_input_tokens")
			}
			err := batch.complete(map[string]any{"usage": usage})
			if tc.wantReject && err == nil {
				t.Fatal("terminal changed original counter categories without changing their normalized total")
			}
			if !tc.wantReject && err != nil {
				t.Fatal(err)
			}
		})
	}
}
