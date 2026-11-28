//go:build linux

package runtimeexec

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLocalDescriptorsRetainGuardianCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	journal := &networkPreparationJournal{}
	ctx = WithBinding(ctx, Binding{Journal: journal, Dir: t.TempDir()})
	file, err := os.CreateTemp(t.TempDir(), "descriptor")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.WriteString("original descriptor bytes"); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "/bin/cat", "/proc/self/fd/3")
	cmd.ExtraFiles = []*os.File{file}
	var output strings.Builder
	cmd.Stdout = &output
	stop, err := SuperviseLocalDescriptors(ctx, cmd, Policy{}, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = cmd.Run(); err != nil {
		t.Fatal(err)
	}
	stop()
	if output.String() != "original descriptor bytes" || journal.record.State != "cleaned" || journal.record.CompletionPath == "" {
		t.Fatalf("descriptor or completion lost: %q %+v", output.String(), journal.record)
	}
}

func TestLocalDescriptorsRejectUnprovenTransportAndImplicitFiles(t *testing.T) {
	file, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, test := range []struct {
		policy      Policy
		descriptors []int
	}{
		{Policy{Mode: "container", Image: "fixture"}, []int{3}},
		{Policy{NetworkRuntime: "fixture"}, []int{3}},
		{Policy{}, nil}, {Policy{}, []int{4}},
	} {
		cmd := exec.Command("/bin/cat")
		cmd.ExtraFiles = []*os.File{file}
		stop, err := SuperviseLocalDescriptors(context.Background(), cmd, test.policy, test.descriptors)
		if stop != nil {
			stop()
		}
		if err == nil || stop != nil || cmd.Process != nil {
			t.Fatal("unproven descriptor transport admitted", test)
		}
	}
}

func TestLocalDescriptorPipeEOFBeforeChildExit(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprintf("bound=%v", bound), func(t *testing.T) {
			if err := localDescriptorPipeExchange(t, bound); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func localDescriptorPipeExchange(t *testing.T, bound bool) (returnErr error) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	journal := &networkPreparationJournal{}
	if bound {
		ctx = WithBinding(ctx, Binding{Journal: journal, Dir: t.TempDir()})
	}
	var files []*os.File
	var cmd *exec.Cmd
	var stop func()
	wait := make(chan error, 1)
	started, joined := false, false
	var waitErr error
	childPID := 0
	defer func() {
		cancel()
		for _, file := range files {
			_ = file.Close()
		}
		if started && !joined {
			waitErr = <-wait
			joined = true
		}
		if stop != nil {
			stop()
		}
		if started {
			t.Logf("guardian_pid=%d child_pid=%d wait_joined=%v wait_error=%v cleanup_state=%q", cmd.Process.Pid, childPID, joined, waitErr, journal.record.State)
		}
	}()
	requestReader, requestWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	files = append(files, requestReader, requestWriter)
	replyReader, replyWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	files = append(files, replyReader, replyWriter)
	// The child must close fd 3 and remain alive waiting for the reply. The
	// controller sends that reply only after all copies of the writer close.
	cmd = exec.CommandContext(ctx, "/bin/sh", "-c", `set -eu
printf 'request:%s\n' "$$" >&3
exec 3>&-
IFS= read -r reply
test "$reply" = accepted
printf 'reply:%s\n' "$reply"`)
	cmd.ExtraFiles = []*os.File{requestWriter}
	cmd.Stdin = replyReader
	var output strings.Builder
	cmd.Stdout = &output
	stop, err = SuperviseLocalDescriptors(ctx, cmd, Policy{}, []int{3})
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	started = true
	go func() { wait <- cmd.Wait() }()
	if err := requestWriter.Close(); err != nil {
		return err
	}
	if err := requestReader.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	request, readErr := io.ReadAll(requestReader)
	childPID, err = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(request), "request:")))
	if err != nil || childPID <= 0 {
		return fmt.Errorf("child request identity %q: %v", request, err)
	}
	if readErr != nil {
		return fmt.Errorf("request pipe EOF before reply: %w", readErr)
	}
	select {
	case waitErr = <-wait:
		joined = true
		return fmt.Errorf("child exited before receiving its reply: %v", waitErr)
	default:
	}
	if _, err := replyWriter.WriteString("accepted\n"); err != nil {
		return err
	}
	if err := replyWriter.Close(); err != nil {
		return err
	}
	waitErr = <-wait
	joined = true
	if waitErr != nil {
		return waitErr
	}
	stop()
	if output.String() != "reply:accepted\n" {
		return fmt.Errorf("child reply exchange failed: %q", output.String())
	}
	if bound && (journal.record.State != "cleaned" || journal.record.CompletionPath == "") {
		return fmt.Errorf("guardian completion lost: %+v", journal.record)
	}
	return nil
}
