package prfix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
)

// Retain integer tokens in GitHub objects, including their saved evidence and
// receipts. Identity comparisons must not round through float64 or use exponents.
func decodeGitHubJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fmt.Errorf("one GitHub JSON document required")
	}
	return nil
}

func (e *PREvidence) UnmarshalJSON(data []byte) error {
	type evidence PREvidence
	return decodeGitHubJSON(data, (*evidence)(e))
}

func (r *Receipt) UnmarshalJSON(data []byte) error {
	type receipt Receipt
	return decodeGitHubJSON(data, (*receipt)(r))
}

func githubObject(value any) map[string]any {
	var object map[string]any
	if err := decodeGitHubJSON(jsonBytes(value), &object); err != nil {
		panic(err)
	}
	return object
}

func githubID(object map[string]any, key string) string {
	var text string
	switch value := object[key].(type) {
	case json.Number:
		text = string(value)
	case string:
		text = value
	case int:
		text = strconv.Itoa(value)
	case int64:
		text = strconv.FormatInt(value, 10)
	case float64:
		// Legacy/fake objects may still contain floats. Reject values whose exact
		// original integer cannot be established from that representation.
		if value < 1 || value > 1<<53-1 || math.Trunc(value) != value {
			return ""
		}
		text = strconv.FormatFloat(value, 'f', 0, 64)
	default:
		return ""
	}
	if !numeric(text) {
		return ""
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 1 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}
