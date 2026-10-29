//go:build linux

package network

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/socketfence"
	"golang.org/x/sys/unix"
)

// This substitutes only privileged Restore/namespace launch. The production
// finalizer, receipt, completion pipe and retained-fd emptiness wait are real.
func TestRetainedMemberFixture(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "retained-member" {
		return
	}
	var launch memberLaunch
	config := os.NewFile(4, "launch")
	if decodeSealedPayload(config, &launch) != nil || len(launch.Request.Env) != 2 {
		os.Exit(125)
	}
	config.Close()
	root, mode := launch.Request.Env[0], launch.Request.Env[1]
	completion := memberCompletion()
	flags, err := unix.FcntlInt(completion.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		os.Exit(125)
	}
	completionTarget, err := os.Readlink("/proc/self/fd/7")
	if err != nil {
		os.Exit(125)
	}
	// Model the guardian mapping its separate acknowledgement to child fd7.
	ackR, ackW, err := os.Pipe()
	if err != nil {
		os.Exit(125)
	}
	binary, _ := os.Executable()
	child := exec.Command(binary, "-test.run=^TestPrivateMemberCompletionFixture$", "--", completionTarget)
	child.ExtraFiles = []*os.File{nil, nil, nil, nil, ackW}
	if child.Run() != nil {
		os.Exit(125)
	}
	ackW.Close()
	observedAck, err := io.ReadAll(ackR)
	ackR.Close()
	if err != nil || string(observedAck) != "\x01" {
		os.Exit(125)
	}
	if mode == "early-failure" {
		os.Exit(125)
	}
	group, err := os.Open(filepath.Join(root, "group"))
	if err != nil {
		os.Exit(125)
	}
	defer group.Close()
	var st unix.Stat_t
	if unix.Fstat(int(group.Fd()), &st) != nil {
		os.Exit(125)
	}
	boundary := &socketfence.Boundary{Group: group, State: socketfence.State{CgroupID: st.Ino}}
	_, held, err := socketfence.OpenSocket(filepath.Join(root, "auth"))
	if err != nil {
		os.Exit(125)
	}
	// Restore normally acquires this original inode reference before execution.
	defer held.Close()
	if os.WriteFile(filepath.Join(root, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600) != nil {
		os.Exit(125)
	}
	life := os.NewFile(3, "lifetime")
	if mode == "cancel" {
		_, _ = io.Copy(io.Discard, life)
	}
	ack := ""
	if mode == "cleaned" {
		ack = "\x01"
	}
	if mode == "receipt-failure" {
		launch.Receipt = filepath.Join(root, "missing", "receipt")
	}
	err = finishMember(launch, nil, strings.NewReader(ack), completion, boundary.WaitEmpty, []*os.File{os.Stdin, os.Stdout, os.Stderr, life})
	// A separate marker observes return from the real retention wait, even after
	// service death orphaned this fixture and it is no longer directly waitable.
	held.Close()
	group.Close()
	_ = os.WriteFile(filepath.Join(root, "returned"), []byte(fmt.Sprint(err)), 0600)
	os.Exit(0)
}

func TestPrivateMemberCompletionFixture(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	forbidden := os.Args[len(os.Args)-1]
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		os.Exit(125)
	}
	for _, fd := range fds {
		target, _ := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if target == forbidden {
			os.Exit(125)
		}
	}
	ack := os.NewFile(7, "guardian-cleanup-ack")
	if _, err := ack.Write([]byte{1}); err != nil {
		os.Exit(125)
	}
	os.Exit(0)
}

func retainedMemberCommand() *exec.Cmd {
	binary, _ := os.Executable()
	return exec.Command(binary, "-test.run=^TestRetainedMemberFixture$", "--", "retained-member")
}

func retainedFixture(t *testing.T) (string, *net.UnixListener, socketfence.Socket) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "group"), 0700); err != nil {
		t.Fatal(err)
	}
	writePopulation(t, root, "populated 1\n")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(root, "auth"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	identity, f, err := socketfence.OpenSocket(filepath.Join(root, "auth"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() {
		// Never leave a fixture retainer behind when an assertion fails before
		// the test installs its normal process handle cleanup.
		if _, err := os.Stat(filepath.Join(root, "returned")); err == nil {
			return
		}
		data, err := os.ReadFile(filepath.Join(root, "pid"))
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			return
		}
		process, err := os.FindProcess(pid)
		if err == nil {
			_ = process.Kill()
			_ = process.Release()
		}
	})
	return root, listener, identity
}

func writePopulation(t *testing.T, root, value string) {
	t.Helper()
	path := filepath.Join(root, "group", "cgroup.events")
	if err := os.WriteFile(path+".next", []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".next", path); err != nil {
		t.Fatal(err)
	}
}

func waitRetainerFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return data
		}
		if !os.IsNotExist(err) || time.Now().After(deadline) {
			t.Fatalf("waiting for %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertRetainedInode(t *testing.T, pid int, expected socketfence.Socket) {
	t.Helper()
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatal("retainer exited:", err)
	}
	for _, entry := range entries {
		var st unix.Stat_t
		if unix.Stat(fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name()), &st) == nil && st.Ino == expected.Inode && uint32(unix.Major(st.Dev)<<20|unix.Minor(st.Dev)) == expected.Device {
			return
		}
	}
	t.Fatal("retainer lost the original enrolled inode")
}

// Send real stream descriptors so EOF proves a retained root member does not
// keep a caller pipe alive merely because the shared group is still populated.
func retainedRequest(t *testing.T, control string, req Request) (*net.UnixConn, *os.File) {
	t.Helper()
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: control, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	outputR, outputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { outputR.Close() })
	defer outputW.Close()
	payload, err := sealedPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	envelope, _ := json.Marshal(Request{Operation: "exec", Reference: req.Reference, ID: req.ID})
	if _, _, err = c.WriteMsgUnix(envelope, unix.UnixRights(int(input.Fd()), int(outputW.Fd()), int(outputW.Fd()), int(payload.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	if response := readResponse(t, c); response.Error != "" {
		t.Fatal(response.Error)
	}
	return c, outputR
}

func assertStreamClosed(t *testing.T, stream *os.File) {
	t.Helper()
	if err := stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(stream); err != nil || len(data) != 0 {
		t.Fatalf("retainer held stream open: %q %v", data, err)
	}
}

func TestRetainedMemberReportsBeforeSharedGroupDrains(t *testing.T) {
	for _, mode := range []string{"uncertain", "cleaned", "receipt-failure", "early-failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root, listener, identity := retainedFixture(t)
			s, control := fixtureService(t)
			s.memberCommand = func(string) *exec.Cmd { return retainedMemberCommand() }
			id := randomID()
			c, stream := retainedRequest(t, control, Request{Operation: "exec", Reference: s.status.Reference, ID: id, Path: "/bin/true", Env: []string{root, mode}, Terminal: true})
			if mode == "cancel" {
				c.Close()
			} else {
				response := readResponse(t, c)
				if mode == "receipt-failure" || mode == "early-failure" {
					if response.Error == "" {
						t.Fatal("missing receipt did not fail promptly")
					}
				} else if response.Error != "" || response.Member.State != mode {
					t.Fatalf("completion: %+v", response)
				}
			}
			assertStreamClosed(t, stream)
			if mode == "early-failure" {
				return
			}
			pid, _ := strconv.Atoi(string(waitRetainerFile(t, filepath.Join(root, "pid"))))
			process, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = process.Kill(); _ = process.Release() })
			if mode == "cleaned" {
				// A trusted ack must not wait for another shared-runtime member.
				waitRetainerFile(t, filepath.Join(root, "returned"))
				return
			}
			if mode == "cancel" {
				data := waitRetainerFile(t, filepath.Join(s.dir, "member-"+id+".done"))
				var receipt Membership
				if json.Unmarshal(data, &receipt) != nil || receipt.State != "uncertain" || receipt.ID != id {
					t.Fatal("cancel lost uncertain receipt")
				}
			}
			listener.Close()
			assertRetainedInode(t, pid, identity)
			if _, err := os.Stat(filepath.Join(root, "returned")); !os.IsNotExist(err) {
				t.Fatal("uncertain outcome released while populated")
			}
			writePopulation(t, root, "populated 0\n")
			waitRetainerFile(t, filepath.Join(root, "returned"))
			if mode == "receipt-failure" {
				data := waitRetainerFile(t, filepath.Join(root, "returned"))
				if string(data) == "<nil>" {
					t.Fatal("receipt persistence error was lost")
				}
			}
		})
	}
}

type retainerController struct {
	Control   string
	Reference Reference
}

func TestRetainerControllerFixture(t *testing.T) {
	if os.Getenv("AW_RETAINER_CONTROLLER") != "1" {
		return
	}
	s, control := fixtureService(t)
	s.memberCommand = func(string) *exec.Cmd { return retainedMemberCommand() }
	_ = json.NewEncoder(os.Stdout).Encode(retainerController{control, s.status.Reference})
	for {
		time.Sleep(time.Hour)
	}
}

func TestUncertainInodeReferencesSurviveServiceSIGKILL(t *testing.T) {
	root, listener, identity := retainedFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	temp, err := os.MkdirTemp("", "retainer-controller-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	controller := exec.Command(binary, "-test.run=^TestRetainerControllerFixture$")
	controller.Env = append(os.Environ(), "AW_RETAINER_CONTROLLER=1", "TMPDIR="+temp)
	output, err := controller.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	controller.Stderr = os.Stderr
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Process.Kill(); controller.Wait() })
	type readiness struct {
		service retainerController
		err     error
	}
	info := make(chan readiness, 1)
	go func() {
		var value retainerController
		err := json.NewDecoder(bufio.NewReader(output)).Decode(&value)
		info <- readiness{value, err}
	}()
	var service retainerController
	select {
	case ready := <-info:
		if ready.err != nil {
			t.Fatal("controller readiness:", ready.err)
		}
		service = ready.service
	case <-time.After(3 * time.Second):
		t.Fatal("controller readiness timed out")
	}
	c, stream := retainedRequest(t, service.Control, Request{Operation: "exec", Reference: service.Reference, ID: randomID(), Path: "/bin/true", Env: []string{root, "uncertain"}})
	if response := readResponse(t, c); response.Error != "" || response.Member.State != "uncertain" {
		t.Fatalf("completion: %+v", response)
	}
	assertStreamClosed(t, stream)
	pid, _ := strconv.Atoi(string(waitRetainerFile(t, filepath.Join(root, "pid"))))
	retainer, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = retainer.Kill(); _ = retainer.Release() })
	listener.Close()
	if err := controller.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = controller.Wait()
	// No listener/service/registration handle remains: the independent member
	// must own the unlinked authentication inode until the fake kernel is empty.
	assertRetainedInode(t, pid, identity)
	writePopulation(t, root, "invalid\n")
	time.Sleep(300 * time.Millisecond)
	assertRetainedInode(t, pid, identity)
	writePopulation(t, root, "populated 0\n")
	waitRetainerFile(t, filepath.Join(root, "returned"))
}
