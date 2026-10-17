// Package agentworkflow contains the example definitions distributed with the CLI.
package agentworkflow

import "embed"

// Examples are immutable at build time and copied by init without overwriting
// workspace definitions.
//
//go:embed workflows/*.yml agents.yml
var Examples embed.FS
