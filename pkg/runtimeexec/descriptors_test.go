//go:build linux

package runtimeexec

import (
	"context"
	"os"
	"os/exec"
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
		if stop, err := SuperviseLocalDescriptors(context.Background(), cmd, test.policy, test.descriptors); err == nil || stop != nil || cmd.Process != nil {
			t.Fatal("unproven descriptor transport admitted", test)
		}
	}
}
