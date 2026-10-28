package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func readAnswer(path string, stdin io.Reader) (json.RawMessage, error) {
	input := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		input = f
	}
	raw, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("answer exceeds 1 MiB")
	}
	var value any
	if !json.Valid(raw) || json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, fmt.Errorf("answer must be one non-null JSON value")
	}
	return json.RawMessage(raw), nil
}
