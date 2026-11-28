// Package codex contains the single repository-owned managed source descriptor.
// It does not enable a runtime capability or accept third-party profile entries.
package codex

import _ "embed"

//go:embed profile.json
var descriptor []byte

func Descriptor() []byte { return append([]byte(nil), descriptor...) }
