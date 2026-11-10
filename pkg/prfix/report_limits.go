package prfix

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const maxReportCollection = 4096
const maxReportEntries = 16384
const maxReportDepth = 32

// Validate collection/depth limits before decoding large typed elements such as
// Intent. Serialized byte limits alone cannot bound their heap expansion.
func validateReportShape(text string) error {
	if len(text) > 16<<20 {
		return fmt.Errorf("original report exceeds 16 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	entries := 0
	var value func(int) error
	value = func(depth int) error {
		if depth >= maxReportDepth {
			return fmt.Errorf("original report nesting exceeds %d", maxReportDepth)
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		opening, container := token.(json.Delim)
		if !container {
			return nil
		}
		if opening != '{' && opening != '[' {
			return fmt.Errorf("invalid JSON container")
		}
		count := 0
		for decoder.More() {
			count++
			entries++
			if count > maxReportCollection || entries > maxReportEntries {
				return fmt.Errorf("original report collection limit exceeded")
			}
			if opening == '{' {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				if _, ok := key.(string); !ok {
					return fmt.Errorf("object key required")
				}
			}
			if err := value(depth + 1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("one original JSON report required")
	}
	return nil
}
