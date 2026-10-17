package process

import "errors"

// ErrCleanup means execution cleanup was not confirmed. It must remain visible
// even when a cancellation also occurred; callers must suspend further turns.
var ErrCleanup = errors.New("execution cleanup could not be confirmed")
