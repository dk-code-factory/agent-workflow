// Package agentworkflow contains the example definitions distributed with the CLI.
package agentworkflow

import "embed"

// Examples are immutable at build time and copied by init without overwriting
// workspace definitions.
//
//go:embed workflows/*.yml workflows/agents/*.yaml agents.yml
var Examples embed.FS

// PluginBootstrap is the maintained, package-relative bootstrap source. Builders
// copy it unchanged; generated configuration selects an exact engine release.
//
//go:embed plugins/agent-workflow/scripts/engine_bootstrap.py
var PluginBootstrap embed.FS
