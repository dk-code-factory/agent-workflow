// agent-workflow-runtime explicitly prepares/owns a shared Linux network runtime.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

func main() {
	if err := run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: agent-workflow-runtime prepare|start|exec|status|forward|stop NAME [options]")
	}
	op, name := os.Args[1], os.Args[2]
	if err := network.ValidateName(name); err != nil {
		return err
	}
	ctx := context.Background()
	switch op {
	case "prepare":
		fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
		uid := fs.Uint("uid", uint(os.Getuid()), "existing rootful Podman operator UID to enroll")
		if err := fs.Parse(os.Args[3:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected prepare arguments")
		}
		return network.Prepare(name, uint32(*uid))
	case "start":
		return network.Start(name)
	case "status":
		out, err := network.Call(ctx, name, network.Request{Operation: "status"})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(out.Status)
	case "stop", "forward":
		status, err := network.Call(ctx, name, network.Request{Operation: "status"})
		ref := status.Status.Reference
		if err != nil {
			return err
		}
		req := network.Request{Operation: op, Reference: ref}
		if op == "forward" {
			if len(os.Args) != 5 {
				return fmt.Errorf("usage: forward NAME HOST_PORT RUNTIME_LOOPBACK_PORT")
			}
			req.Forward.HostPort, err = strconv.Atoi(os.Args[3])
			if err != nil {
				return err
			}
			req.Forward.GuestPort, err = strconv.Atoi(os.Args[4])
			if err != nil {
				return err
			}
		}
		_, err = network.Call(ctx, name, req)
		return err
	case "exec":
		args := os.Args[3:]
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			return fmt.Errorf("exec requires a command")
		}
		ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer cancel()
		policy, err := runtimeexec.PinPolicy(ctx, runtimeexec.Policy{NetworkRuntime: name})
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		restore, err := prepareTerminal(cmd)
		if err != nil {
			return err
		}
		defer restore()
		stop, err := runtimeexec.SupervisePolicy(ctx, cmd, policy)
		if err != nil {
			return err
		}
		defer stop()
		return cmd.Run()
	default:
		return fmt.Errorf("unknown operation %s", op)
	}
}
