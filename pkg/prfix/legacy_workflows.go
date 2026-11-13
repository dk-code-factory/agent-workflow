package prfix

import "embed"

// legacyExamples exists for the historical Go application's deterministic
// comparison tests. The stock CLI neither registers nor distributes these
// application-specific actions. New workflows use examples/pr-fix.
//
//go:embed testdata/workflows/pr-fix/v1/*.yml
var legacyExamples embed.FS
