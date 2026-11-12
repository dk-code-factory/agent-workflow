package runtimeexec

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

type execResultRuntime struct {
	containerapi.Runtime
	owner   containerapi.Owner
	code    int
	copyErr error
}

func (r *execResultRuntime) Identity(context.Context) (string, error) {
	return r.owner.Selection.Identity, nil
}
func (r *execResultRuntime) Inspect(context.Context, string) (containerapi.Object, bool, error) {
	return containerapi.Object{ID: r.owner.ID, Name: r.owner.Name, Labels: r.owner.Labels}, true, nil
}
func (r *execResultRuntime) ExecCreate(context.Context, string, containerapi.ExecRequest) (string, error) {
	return "command", nil
}
func (r *execResultRuntime) ExecAttach(context.Context, string) (containerapi.Attachment, error) {
	return execResultAttachment{r.copyErr}, nil
}
func (r *execResultRuntime) ExecInspect(context.Context, string) (containerapi.ExecStatus, error) {
	return containerapi.ExecStatus{ID: "command", ContainerID: r.owner.ID, ExitCode: r.code}, nil
}

type execResultAttachment struct{ err error }

func (a execResultAttachment) Copy(containerapi.Streams) error { return a.err }
func (execResultAttachment) Close() error                      { return nil }

func TestExecPreservesInspectedExitCodeAfterAttachmentError(t *testing.T) {
	exitErr := exec.Command("sh", "-c", "exit 7").Run()
	var workloadExit *exec.ExitError
	if !errors.As(exitErr, &workloadExit) || workloadExit.ExitCode() != 7 {
		t.Fatalf("workload fixture: %v", exitErr)
	}
	streamErr := errors.New("output transport failed")
	for _, tc := range []struct {
		name string
		code int
		err  error
	}{
		{"success", 0, nil},
		{"podman-workload-failure", 7, exitErr},
		{"transport-failure", 0, streamErr},
		{"guardian-failure", 125, exitErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := &apiJournal{}
			ctx := WithBinding(t.Context(), Binding{Journal: journal, Dir: t.TempDir(), RunID: "run", LogicalID: "step", Owner: "owner", Generation: 1})
			parent, err := Admit(ctx, "container-api")
			if err != nil {
				t.Fatal(err)
			}
			owner := containerapi.Owner{ID: "parent", Name: "owned", Labels: map[string]string{"token": "retained"}, Selection: &containerapi.Selection{Identity: "runtime"}}
			r := &execResultRuntime{owner: owner, code: tc.code, copyErr: tc.err}
			s := &ContainerSession{Runtime: r, Invocation: parent, Owner: owner}
			code, err := s.Exec(ctx, containerapi.ExecRequest{Command: []string{"command"}}, containerapi.Streams{})
			if code != tc.code || !errors.Is(err, tc.err) {
				t.Fatalf("code=%d error=%v; want code=%d error=%v", code, err, tc.code, tc.err)
			}
			last := journal.records[len(journal.records)-1]
			if tc.code == 125 {
				if !errors.Is(err, process.ErrCleanup) || last.State != "uncertain" {
					t.Fatalf("guardian failure lost cleanup barrier: error=%v record=%+v", err, last)
				}
			} else if last.State != "cleaned" {
				t.Fatalf("completed exec not cleaned: %+v", last)
			}
		})
	}
}
