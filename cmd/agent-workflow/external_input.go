package main

import (
	"bytes"
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
	if !json.Valid(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("answer must be one non-null JSON value")
	}
	return json.RawMessage(raw), nil
}
