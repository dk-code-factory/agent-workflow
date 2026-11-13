package workflows

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOutputLimitProcess(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_OUTPUT_LIMIT_PROCESS") != "1" {
		return
	}
	size, err := strconv.Atoi(os.Getenv("AGENT_WORKFLOW_OUTPUT_LIMIT_SIZE"))
	if err != nil || size < 0 || size > MaxCommandOutput+1 {
		os.Exit(2)
	}
	var output io.Writer = os.Stdout
	if os.Getenv("AGENT_WORKFLOW_OUTPUT_LIMIT_STREAM") == "stderr" {
		output = os.Stderr
	}
	if _, err := io.Copy(output, strings.NewReader(strings.Repeat("x", size))); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func copyProcessOutput(t *testing.T, output io.Writer, stream string, size int) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOutputLimitProcess$")
	command.WaitDelay = time.Second
	command.Env = append(os.Environ(), "AGENT_WORKFLOW_OUTPUT_LIMIT_PROCESS=1",
		"AGENT_WORKFLOW_OUTPUT_LIMIT_SIZE="+strconv.Itoa(size),
		"AGENT_WORKFLOW_OUTPUT_LIMIT_STREAM="+stream)
	if stream == "stderr" {
		command.Stderr = output
	} else {
		command.Stdout = output
	}
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("output fixture exceeded its deadline: %v", ctx.Err())
	}
	return err
}

func TestContainerProtocolOutputLimitsThroughProcessPipes(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		limit := 1 << 20
		if stream == "stderr" {
			limit = 4 << 20
		}
		for _, extra := range []int{0, 1} {
			t.Run(stream+"/extra="+strconv.Itoa(extra), func(t *testing.T) {
				output := boundedProtocolOutput{limit: limit}
				err := copyProcessOutput(t, &output, stream, limit+extra)
				if extra == 0 {
					if err != nil || output.overflow || len(output.Bytes()) != limit {
						t.Fatalf("rejected boundary output: bytes=%d overflow=%v err=%v", len(output.Bytes()), output.overflow, err)
					}
				} else if err == nil || !output.overflow || len(output.Bytes()) > limit {
					t.Fatalf("oversized output escaped its limit: bytes=%d overflow=%v err=%v", len(output.Bytes()), output.overflow, err)
				}
			})
		}
	}
}

func TestCommandOutputLimitThroughProcessPipes(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		for _, extra := range []int{0, 1} {
			t.Run(stream+"/extra="+strconv.Itoa(extra), func(t *testing.T) {
				var output cappedOutput
				err := copyProcessOutput(t, &output, stream, MaxCommandOutput+extra)
				if err != nil || output.overflow != (extra != 0) || len(output.String()) != MaxCommandOutput {
					t.Fatalf("command output limit: bytes=%d overflow=%v err=%v", len(output.String()), output.overflow, err)
				}
			})
		}
	}
}
