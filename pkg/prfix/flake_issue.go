package prfix

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	flakeDeclarations = regexp.MustCompile("(?s)```dk-flaky\\s*\\n(.*?)\\n```")
	negativeFlake     = regexp.MustCompile(`(?i)\b(?:not|never|no|non)[ -]?(?:a )?(?:flaky|intermittent)`)
	uncertainFlake    = regexp.MustCompile(`(?i)\b(?:maybe|possibly|suspect|could|might|whether|is it)\b|\?`)
	confirmedFlake    = regexp.MustCompile(`(?im)^\s*(?:confirmed flaky test|known flaky test|this test is flaky)[.: ]`)
)

// affirmativeFlakeIssue ports the pinned issue_match affirmative criteria.
// Ambiguous prose remains diagnostic evidence; it cannot exempt a failure.
func affirmativeFlakeIssue(text string, c CIClassification) bool {
	identity := map[string]string{"workflow": c.Workflow, "job": c.Job, "test": c.Test, "signature": c.Signature}
	for _, value := range identity {
		if !validText(value) {
			return false
		}
	}
	for _, block := range flakeDeclarations.FindAllStringSubmatch(text, -1) {
		var declaration map[string]json.RawMessage
		if json.Unmarshal([]byte(block[1]), &declaration) != nil {
			continue
		}
		matched := true
		for key, expected := range identity {
			var actual string
			if json.Unmarshal(declaration[key], &actual) != nil || actual != expected {
				matched = false
				break
			}
		}
		if matched {
			var flaky bool
			return json.Unmarshal(declaration["flaky"], &flaky) == nil && flaky
		}
	}
	for _, value := range identity {
		if !strings.Contains(text, "`"+value+"`") {
			return false
		}
	}
	return !negativeFlake.MatchString(text) && !uncertainFlake.MatchString(text) && confirmedFlake.MatchString(text)
}
