package plugins

import (
	"errors"
	"io/fs"
	"strings"
)

// Diagnostic is shared by structural checks and behavioral fixture reports.
// File is package-relative; Reason retains the actionable validator explanation.
type Diagnostic struct {
	Code   string `json:"code"`
	File   string `json:"file"`
	Case   string `json:"case,omitempty"`
	Reason string `json:"reason"`
}

type CheckReport struct {
	SchemaVersion int          `json:"schema-version"`
	Kind          string       `json:"kind"`
	Passed        bool         `json:"passed"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
}

// Check validates source structure without executing developer or host code.
func Check(root string) CheckReport {
	_, report := checkCaptured(root)
	return report
}

func checkCaptured(root string) (*CapturedPackage, CheckReport) {
	report := CheckReport{SchemaVersion: 1, Kind: "structure", Diagnostics: []Diagnostic{}}
	captured, err := Capture(root)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, packageDiagnostic(err))
		return nil, report
	}
	report.Passed = true
	return captured, report
}

func packageDiagnostic(err error) Diagnostic {
	file := DescriptorFile
	var p *fs.PathError
	if errors.As(err, &p) {
		file = p.Path
	} else if prefix, _, ok := strings.Cut(err.Error(), ":"); ok {
		if clean, e := localPath(prefix); e == nil && strings.Contains(clean, ".") {
			file = clean
		}
	}
	return Diagnostic{Code: "package.invalid", File: file, Reason: err.Error()}
}
