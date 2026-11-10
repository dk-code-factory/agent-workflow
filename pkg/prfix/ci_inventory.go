package prfix

import (
	"fmt"
	"regexp"
	"strings"
)

var ciLogTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z\s`)
var ciNamedFailure = regexp.MustCompile(`^(?:--- FAIL: |FAILED )([^\s(]+)`)
var ciGoSummary = regexp.MustCompile(`^(?:FAIL(?:\s+\S+(?:\s+[0-9.]+s)?)?|PASS|ok\s+\S+\s+(?:[0-9.]+s|\(cached\))|\?\s+\S+\s+\[no test files\]|exit status [1-9][0-9]*)$`)
var ciMakeFailure = regexp.MustCompile(`^(?:make(?:\[[0-9]+\])?: \*\*\* \[[^\]]+: test\] Error [1-9][0-9]*|(?:##\[error\])?(?:Error: )?Process completed with exit code [1-9][0-9]*\.)$`)
var ciEnvField = regexp.MustCompile(`^\s+[A-Za-z_][A-Za-z0-9_]*: `)
var ciGoLocation = regexp.MustCompile(`^\s+[^\s]+\.go:[0-9]+:\s*`)

// Test evidence does not authorize exempting arbitrary job output. Account for
// every line using test-runner structure or an exact candidate signature; fail
// closed for unsupported/extra output rather than a finite fatal-string list.
func verifyTestLogInventory(log string, classes []CIClassification, covered map[string]bool) error {
	signatures := map[string]bool{}
	for _, c := range classes {
		if c.Signature != "" {
			if strings.ContainsAny(c.Signature, "\r\n") {
				return fmt.Errorf("test exemption signatures must identify one diagnostic line")
			}
			signatures[strings.TrimSpace(c.Signature)] = true
		}
	}
	failures := 0
	preamble := false
	for number, line := range strings.Split(log, "\n") {
		line = ciLogTimestamp.ReplaceAllString(line, "")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "##[group]Run ") {
			command := strings.TrimPrefix(trimmed, "##[group]Run ")
			if !testCommand(command) {
				return fmt.Errorf("unsupported failed-step command envelope")
			}
			preamble = true
			continue
		}
		if trimmed == "##[endgroup]" {
			preamble = false
			continue
		}
		if preamble && (strings.HasPrefix(trimmed, "shell: ") || trimmed == "env:" || ciEnvField.MatchString(line)) {
			continue
		}
		if testCommand(trimmed) || ciMakeFailure.MatchString(trimmed) {
			continue
		}
		if match := ciNamedFailure.FindStringSubmatch(trimmed); match != nil {
			if !covered[match[1]] {
				return fmt.Errorf("uncovered failure %s", match[1])
			}
			failures++
			continue
		}
		if ciGoSummary.MatchString(trimmed) || strings.HasPrefix(trimmed, "=== RUN   ") || strings.HasPrefix(trimmed, "=== PAUSE ") || strings.HasPrefix(trimmed, "=== CONT  ") || strings.HasPrefix(trimmed, "=== NAME  ") || strings.HasPrefix(trimmed, "--- PASS: ") || strings.HasPrefix(trimmed, "--- SKIP: ") {
			continue
		}
		if signatures[trimmed] || signatures[strings.TrimSpace(ciGoLocation.ReplaceAllString(line, ""))] {
			continue
		}
		return fmt.Errorf("test exemption cannot account for job log line %d", number+1)
	}
	if failures == 0 {
		return fmt.Errorf("unknown failure cannot be exempted by test evidence")
	}
	return nil
}

type ciStep struct {
	Number     int    `json:"number"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// gh --log-failed omits successful setup steps. Require an unambiguous log for
// every failed step in the exact job, rather than extrapolating one test failure
// across the job or accepting UNKNOWN STEP fallback output.
func failedStepLogs(log, job string, steps []ciStep) ([]string, error) {
	expected := map[string]bool{}
	order := []string{}
	for _, step := range steps {
		if step.Status != "completed" {
			return nil, fmt.Errorf("complete exact-attempt step metadata required")
		}
		if contains([]string{"success", "skipped", "neutral"}, step.Conclusion) {
			continue
		}
		if step.Number < 1 || step.Name == "" || expected[step.Name] {
			return nil, fmt.Errorf("unambiguous failed step identities required")
		}
		expected[step.Name] = true
		order = append(order, step.Name)
	}
	if len(expected) == 0 {
		return nil, fmt.Errorf("failed step metadata required for CI evidence")
	}
	lines := map[string][]string{}
	for _, line := range strings.Split(log, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || fields[0] != job || !expected[fields[1]] {
			return nil, fmt.Errorf("failed step log identity is missing or unsupported")
		}
		lines[fields[1]] = append(lines[fields[1]], fields[2])
	}
	result := []string{}
	for _, name := range order {
		if len(lines[name]) == 0 {
			return nil, fmt.Errorf("CI evidence omitted failed step %s", name)
		}
		result = append(result, strings.Join(lines[name], "\n"))
	}
	return result, nil
}

func testCommand(line string) bool {
	if strings.ContainsAny(line, ";|&`") || strings.Contains(line, "$(") {
		return false
	}
	return line == "make test" || strings.HasPrefix(line, "go test ")
}
