//go:build linux

package network

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxRoutingPlan = 16 << 20

func callerPrivileges(launch memberLaunch) []string {
	args := []string{"--reuid", strconv.FormatUint(uint64(launch.UID), 10), "--regid", strconv.FormatUint(uint64(launch.GID), 10)}
	groups := make([]string, len(launch.Groups))
	for i, group := range launch.Groups {
		groups[i] = strconv.FormatUint(uint64(group), 10)
	}
	if len(groups) == 0 {
		return append(args, "--clear-groups")
	}
	return append(args, "--groups", strings.Join(groups, ","))
}

// writeRoutingPlan runs after setpriv. Configuration parsing never acquires the
// service's root file access or interprets provider-owned environment as root.
type routingDiscovery struct {
	Request  Request
	Expected routingPlan
}

func writeRoutingPlan(verify bool) error {
	var input routingDiscovery
	if err := decodePayload(os.Stdin, &input); err != nil {
		return errors.New("invalid routing discovery request")
	}
	req := input.Request
	if err := req.Reference.Validate(); err != nil {
		return err
	}
	plan, err := planContainerRouting(req.Env, req.Reference, uint32(os.Getuid()))
	if err != nil {
		return err
	}
	if verify {
		return verifyRoutingView(plan, input.Expected, req.Reference)
	}
	return json.NewEncoder(os.Stdout).Encode(plan)
}

type limitedPlanBuffer struct{ bytes.Buffer }

func (b *limitedPlanBuffer) Write(p []byte) (int, error) {
	if len(p) > maxRoutingPlan-b.Len() {
		return 0, errors.New("container routing projection exceeds limit")
	}
	return b.Buffer.Write(p)
}

func discoverRouting(launch memberLaunch, expected *routingPlan) (routingPlan, error) {
	var plan routingPlan
	input := routingDiscovery{Request: launch.Request}
	operation := "__network_runtime_routing_plan_v1"
	if expected != nil {
		input.Expected = *expected
		input.Expected.Files = make([]routingFile, len(expected.Files))
		for i, file := range expected.Files {
			input.Expected.Files[i].Path = file.Path
		}
		operation = "__network_runtime_routing_verify_v1"
	}
	data, err := json.Marshal(input)
	if err != nil {
		return plan, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := append(callerPrivileges(launch), launch.Binary, operation, "plan")
	cmd := exec.CommandContext(ctx, "/usr/bin/setpriv", args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	cmd.Stdin = bytes.NewReader(data)
	var output limitedPlanBuffer
	cmd.Stdout = &output
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return plan, fmt.Errorf("container routing discovery failed: %w", err)
	}
	if expected != nil {
		return plan, nil
	}
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		return plan, errors.New("invalid container routing projection")
	}
	return plan, nil
}

// fenceWorkload is trusted launcher code. No workload, plugin, MCP process or
// provider executes until the private projection and generation check succeed.
func fenceWorkload() error {
	if os.Geteuid() != 0 {
		return errors.New("container API fencing requires the runtime supervisor")
	}
	// Helpers must not retain the guardian lifetime or completion writer. Only
	// the final trusted workload/guardian chain inherits these descriptors.
	unix.CloseOnExec(3)
	unix.CloseOnExec(7)
	unix.Close(5)
	unix.Close(6)
	config := os.NewFile(4, "launch")
	var launch memberLaunch
	err := decodePayload(config, &launch)
	config.Close()
	if err != nil {
		return errors.New("invalid container API fencing request")
	}
	ref := launch.Request.Reference
	if err := ref.Validate(); err != nil {
		return err
	}
	id := launch.Request.ID
	if len(id) != 32 || !hexID(id) || launch.Binary != filepath.Join(Registry, ref.Name, "launcher") || launch.Receipt != filepath.Join(Registry, ref.Name, "member-"+id+".done") {
		return errors.New("invalid container API fencing ownership")
	}
	plan, err := discoverRouting(launch, nil)
	if err != nil {
		return err
	}
	// Mount namespace operations are per-thread. Keep this thread through exec;
	// Go helper goroutines never enter the workload's private mount namespace.
	runtime.LockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("private invocation mount namespace: %w", err)
	}
	staging := filepath.Join(Registry, ref.Name, "fence-"+id)
	if err := applyRoutingPlan(plan, ref.Endpoint, staging); err != nil {
		return fmt.Errorf("container API fencing: %w", err)
	}
	// A host-side writer may have changed the directory inventory during initial
	// discovery. Frozen routing directories and private file inodes make this
	// second caller-credential pass a stable check before any workload effects.
	if _, err := discoverRouting(launch, &plan); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = Check(ctx, ref)
	cancel()
	if err != nil {
		return err
	}
	// Do not pass namespace handles to the workload. fd3 is the existing guardian
	// lifetime channel; fd4 carries only the original caller's execution request.
	f, err := sealedPayload(launch.Request)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := int(f.Fd())
	if fd == 4 {
		_, err = unix.FcntlInt(f.Fd(), unix.F_SETFD, 0)
	} else {
		err = unix.Dup3(fd, 4, 0)
	}
	if err != nil {
		return err
	}
	for _, inherited := range []uintptr{3, 7} {
		if _, err := unix.FcntlInt(inherited, unix.F_SETFD, 0); err != nil {
			return err
		}
	}
	args := append([]string{"/usr/bin/setpriv"}, callerPrivileges(launch)...)
	args = append(args, launch.Binary, "__network_runtime_workload_v1", id)
	return syscall.Exec(args[0], args, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"})
}

func verifyRoutingView(visible, expected routingPlan, ref Reference) error {
	paths := make(map[string]bool, len(expected.Files))
	for _, file := range expected.Files {
		paths[file.Path] = true
	}
	if len(visible.Files) != len(paths) {
		return errors.New("container routing inventory changed during projection")
	}
	for _, file := range visible.Files {
		if !paths[file.Path] {
			return errors.New("container routing inventory changed during projection")
		}
		f, err := openRoutingPath(file.Path, false)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxRoutingFile+1))
		var fs unix.Statfs_t
		statErr := unix.Fstatfs(int(f.Fd()), &fs)
		f.Close()
		if err != nil || statErr != nil || fs.Flags&unix.ST_RDONLY == 0 || !bytes.Equal(data, file.Data) {
			return errors.New("container routing projection is incomplete or writable")
		}
	}
	for _, dir := range append(append([]string(nil), expected.ReadOnlyDirs...), expected.EmptyDirs...) {
		f, err := openRoutingPath(dir, true)
		if err != nil {
			return err
		}
		var fs unix.Statfs_t
		err = unix.Fstatfs(int(f.Fd()), &fs)
		f.Close()
		if err != nil || fs.Flags&unix.ST_RDONLY == 0 {
			return errors.New("container routing directory is writable")
		}
	}
	socket, err := pinRoutingPath(strings.TrimPrefix(ref.Endpoint, "unix://"))
	if err != nil {
		return err
	}
	defer socket.close()
	for _, path := range append(append([]string(nil), expected.Sockets...), visible.Sockets...) {
		path, err = routingMountPath(path)
		if err != nil {
			return err
		}
		alias, err := pinRoutingPath(path)
		if err != nil {
			return err
		}
		matches := routingSame(alias.stat, socket.stat)
		alias.close()
		if !matches {
			return errors.New("container socket projection does not match pinned runtime")
		}
	}
	return nil
}
