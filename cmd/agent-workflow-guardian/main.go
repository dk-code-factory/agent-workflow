// The image entrypoint is handled by pkg/process initialization so the same
// guardian protocol is available to the controller and the small worker binary.
package main

import (
	"fmt"
	_ "github.com/dk-code-factory/agent-workflow/pkg/process"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "internal agent-workflow guardian; no public command")
	os.Exit(2)
}
