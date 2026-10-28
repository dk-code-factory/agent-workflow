//go:build linux

package network_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

func TestTransportClientFixture(t *testing.T) {
	if os.Getenv("AW_TRANSPORT_CLIENT") != "1" {
		return
	}
	payload := os.NewFile(3, "payload")
	var req network.Request
	if json.NewDecoder(payload).Decode(&req) != nil {
		os.Exit(126)
	}
	payload.Close()
	cmd := exec.Command(req.Path, req.Args...)
	cmd.Env = req.Env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(126)
	}
	os.Exit(0)
}
func TestNativePayloadFixture(t *testing.T) {
	if os.Getenv("AW_NATIVE_PAYLOAD") != "1" {
		return
	}
	args := os.Args[len(os.Args)-2:]
	if len(args[0]) != 70000 || len(args[1]) != 70000 {
		os.Exit(126)
	}
	for i := 0; i < 200; i++ {
		if len(os.Getenv(fmt.Sprintf("VALUE_%d", i))) != 1024 {
			os.Exit(126)
		}
	}
	if _, ok := os.LookupEnv("AGENT_WORKFLOW_NETWORK_REQUEST"); ok {
		os.Exit(126)
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		os.Exit(126)
	}
	for _, fd := range fds {
		target, _ := os.Readlink("/proc/self/fd/" + fd.Name())
		if strings.Contains(target, "memfd:runtime-launch") || (os.Getenv("AW_ACK_TARGET") != "" && target == os.Getenv("AW_ACK_TARGET")) {
			os.Exit(126)
		}
	}
	code, _ := strconv.Atoi(os.Getenv("AW_NATIVE_EXIT"))
	os.Exit(code)
}
func nativePayload(t *testing.T, ctx context.Context, exit int) *exec.Cmd {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestNativePayloadFixture$", "--", strings.Repeat("a", 70000), strings.Repeat("b", 70000))
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = []string{"AW_NATIVE_PAYLOAD=1", "AW_NATIVE_EXIT=" + strconv.Itoa(exit), "AW_TRANSPORT_CLIENT=1"}
	for i := 0; i < 200; i++ {
		cmd.Env = append(cmd.Env, fmt.Sprintf("VALUE_%d=%s", i, strings.Repeat("v", 1024)))
	}
	return cmd
}
func fixtureReference() network.Reference {
	ref := network.Reference{Name: "test", Identity: strings.Repeat("a", 32), Generation: 1}
	ref.Endpoint = network.Endpoint(ref.Name, ref.Identity)
	return ref
}
func TestWrapNativeAggregateThroughRealGuardian(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	native := nativePayload(t, ctx, 0)
	if out, err := native.CombinedOutput(); err != nil {
		t.Fatalf("native payload failed: %s %v", out, err)
	}
	cmd := nativePayload(t, ctx, 0)
	cleanup, err := network.Wrap(cmd, fixtureReference(), strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	payload := cmd.ExtraFiles[0]
	if len(cmd.Args) != 2 || len(strings.Join(cmd.Args, " ")) > 4096 {
		t.Fatal("payload in argv")
	}
	for _, value := range cmd.Env {
		if strings.HasPrefix(value, "AGENT_WORKFLOW_NETWORK_REQUEST=") || len(value) > 4096 {
			t.Fatal("flattened payload in environment")
		}
	}
	// Substitute the socket peer only; the real guardian must explicitly forward
	// the same sealed descriptor that the production client receives.
	cmd.Args = []string{cmd.Path, "-test.run=^TestTransportClientFixture$"}
	stop, err := process.SuperviseWithOptions(cmd, process.SupervisionOptions{InheritDescriptors: []int{3}})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	out, err := cmd.CombinedOutput()
	stop()
	cleanup()
	if err != nil {
		t.Fatalf("wrapped aggregate payload: %s %v", out, err)
	}
	if _, err := payload.Stat(); err == nil {
		t.Fatal("transport descriptor retained")
	}
}
func TestWorkloadGuardianCleanAckExit125AndDescriptorPrivacy(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		t.Run(fmt.Sprint("trusted=", trusted), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			lifeR, lifeW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer lifeR.Close()
			defer lifeW.Close()
			ackR, ackW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer ackR.Close()
			defer ackW.Close()
			if err := ackR.SetReadDeadline(time.Now().Add(12 * time.Second)); err != nil {
				t.Fatal(err)
			}
			cmd := nativePayload(t, ctx, 125)
			ackTarget, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", ackW.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			cmd.Env = append(cmd.Env, "AW_ACK_TARGET="+ackTarget)
			cleanup, err := network.Wrap(cmd, fixtureReference(), strings.Repeat("c", 32))
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			payload := cmd.ExtraFiles[0]
			dummy, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer dummy.Close()
			cmd.Args = []string{cmd.Path, "__network_runtime_workload_v1", "fixture"}
			if !trusted {
				cmd.Path = "/bin/sh"
				cmd.Args = []string{"/bin/sh", "-c", "exit 125"}
			}
			cmd.ExtraFiles = []*os.File{lifeR, payload, dummy, dummy, ackW}
			out, err := cmd.CombinedOutput()
			ackW.Close()
			if err == nil || cmd.ProcessState.ExitCode() != 125 {
				t.Fatalf("workload exit: %q %v", out, err)
			}
			ack, err := io.ReadAll(ackR)
			if err != nil {
				t.Fatal(err)
			}
			if trusted && string(ack) != "\x01" || !trusted && len(ack) != 0 {
				t.Fatalf("cleanup ack trusted=%t: %v", trusted, ack)
			}
		})
	}
}

func TestWrapTransportCleanupAfterFailedStartOrCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint("canceled=", canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/true")
			cleanup, err := network.Wrap(cmd, fixtureReference(), strings.Repeat("d", 32))
			if err != nil {
				t.Fatal(err)
			}
			payload := cmd.ExtraFiles[0]
			stop, err := process.SuperviseWithOptions(cmd, process.SupervisionOptions{InheritDescriptors: []int{3}})
			if err != nil {
				cleanup()
				t.Fatal(err)
			}
			if canceled {
				cancel()
			} else {
				cmd.Path = "/no-such-workflow-guardian"
			}
			if err := cmd.Start(); err == nil {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("failure fixture started")
			}
			stop()
			cleanup()
			cleanup()
			if _, err := payload.Stat(); err == nil {
				t.Fatal("transport descriptor retained after failed Start")
			}
		})
	}
}
