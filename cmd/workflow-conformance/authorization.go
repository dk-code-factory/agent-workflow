package main

import (
	"fmt"
	"os"
	"regexp"
)

// This is a guard against accidental live calls, not a substitute for GitHub's
// protected environments. Publication is never admitted by this CI exception.
func authorizeCI(release bool, scenario string, publication bool) error {
	_, ci := os.LookupEnv("CI")
	if !ci && os.Getenv("GITHUB_ACTIONS") != "true" && !release {
		return nil
	}
	const repo = "dk-code-factory/agent-workflow"
	ref := os.Getenv("GITHUB_REF")
	if !release || publication || scenario == "" ||
		os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != repo ||
		os.Getenv("GITHUB_EVENT_NAME") != "push" ||
		!regexp.MustCompile(`^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(ref) ||
		os.Getenv("GITHUB_WORKFLOW_REF") != repo+"/.github/workflows/release.yml@"+ref ||
		!regexp.MustCompile(`^[0-9]+$`).MatchString(os.Getenv("GITHUB_RUN_ID")) ||
		!regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(os.Getenv("GITHUB_RUN_ATTEMPT")) ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(os.Getenv("GITHUB_SHA")) {
		return fmt.Errorf("CI live conformance requires --release-ci in the expected version-tag release workflow; publication remains manual")
	}
	return nil
}
