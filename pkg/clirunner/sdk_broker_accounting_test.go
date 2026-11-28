package clirunner

import (
	"context"
	"testing"
)

func TestSDKBrokerTerminalKeepsOriginalCounterPartition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		input      int
		created    int
		wantReject bool
	}{
		{name: "original", input: 11, created: 5},
		{name: "cache-creation-reclassified-as-input", input: 16, created: 0, wantReject: true},
		{name: "input-reclassified-as-cache-creation", input: 10, created: 6, wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := newSDKBrokerBatch(nil, nil, nil)
			proof := map[string]any{
				"response_id": "original-response", "model": "fixture", "stop_reason": "end_turn",
				"usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 5},
				"calls": []any{},
			}
			if err := batch.start(proof); err != nil {
				t.Fatal(err)
			}
			if err := batch.seal(context.Background(), proof, "fixture", "original-session"); err != nil {
				t.Fatal(err)
			}
			err := batch.complete(map[string]any{"usage": map[string]any{
				"input_tokens": tc.input, "output_tokens": 3, "cache_read_input_tokens": 2, "cache_creation_input_tokens": tc.created,
			}})
			if tc.wantReject && err == nil {
				t.Fatal("terminal changed original counter categories without changing their normalized total")
			}
			if !tc.wantReject && err != nil {
				t.Fatal(err)
			}
		})
	}
}
