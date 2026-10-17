package buildinfo

import "runtime"

// Version is the single engine release version source. Commit/Dirty are injected
// only by intentional artifact builds, never guessed from a running workspace.
const Version = "0.2.0"

var Commit = "development"
var Dirty = "unknown"
var SourceFingerprint = "unrecorded"

func Metadata() map[string]string {
	return map[string]string{"version": Version, "commit": Commit, "dirty": Dirty, "source_fingerprint": SourceFingerprint, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}
}
