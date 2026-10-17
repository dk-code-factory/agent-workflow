//go:build darwin

package process

// Process-group ownership is enforced on macOS. Detached-process-tree recovery
// needs additional native conformance before production certification.
func prepareDescendants() error { return nil }
func stopDescendants(int) error { return nil }
