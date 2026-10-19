package workflows

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// These definitions are also checked by ui/test/compat.test.ts. Keep the
// browser's parser/admission boundary aligned with Parse and Validate.
func TestUICompatibilityFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "ui-compat")
	data, err := os.ReadFile(filepath.Join(root, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string
		File  string
		Parse bool
		Valid bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join(root, c.File))
			if err != nil {
				t.Fatal(err)
			}
			workflow, err := Parse(source)
			if (err == nil) != c.Parse {
				t.Fatalf("Parse: got %v, want success=%t", err, c.Parse)
			}
			if err == nil {
				err = Validate(workflow)
				if (err == nil) != c.Valid {
					t.Fatalf("Validate: got %v, want success=%t", err, c.Valid)
				}
			}
		})
	}
}
