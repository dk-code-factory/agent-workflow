//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This fake runs only under the deterministic test binary. It exercises the
// production Unix protocol and lifetime fd without privileges or provider calls.
func TestMemberFixture(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "fixture" {
		return
	}
	var launch memberLaunch
	f := os.NewFile(4, "config")
	if json.NewDecoder(f).Decode(&launch) != nil {
		os.Exit(125)
	}
	f.Close()
	_, _ = os.Stdout.Write([]byte("joined:" + launch.Request.Env[0]))
	life := os.NewFile(3, "lifetime")
	_, _ = io.Copy(io.Discard, life)
	if save(launch.Receipt, Membership{ID: launch.Request.ID, UID: launch.UID, State: "cleaned", ExitCode: 7}) != nil {
		os.Exit(125)
	}
	os.Exit(0)
}
func fixtureService(t *testing.T) (*service, string) {
	t.Helper()
	dir := t.TempDir()
	run := t.TempDir()
	if err := os.Mkdir(filepath.Join(run, "netns"), 0700); err != nil {
		t.Fatal(err)
	}
	ref := Reference{Name: "test", Identity: randomID(), Generation: 1}
	ref.Endpoint = Endpoint(ref.Name, ref.Identity)
	s := &service{probe: func() error { return nil }, config: registration{Users: map[uint32]bool{uint32(os.Getuid()): true}}, dir: dir, run: run, stop: make(chan struct{}), status: Status{Reference: ref, Healthy: true, Members: map[string]Membership{}}, keeper: &exec.Cmd{Process: &os.Process{Pid: 1}}}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.memberCommand = func(string) *exec.Cmd { return exec.Command(binary, "-test.run=^TestMemberFixture$") }
	// Use a wrapper so test-only environment does not enter the service API.
	s.binary = binary
	socket := filepath.Join(t.TempDir(), "control")
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			c, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s, socket
}
func requestAt(t *testing.T, socket string, req Request, files bool) (*net.UnixConn, Response) {
	t.Helper()
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	data, _ := json.Marshal(req)
	if files {
		f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		_, _, err = c.WriteMsgUnix(data, unix.UnixRights(int(f.Fd()), int(f.Fd()), int(f.Fd())), nil)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err = c.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	return c, readResponse(t, c)
}
func readResponse(t *testing.T, c *net.UnixConn) Response {
	t.Helper()
	data := make([]byte, 1<<20)
	n, err := c.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	var out Response
	if err = json.Unmarshal(data[:n], &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestConcurrentMembershipDisconnectAndBusyStop(t *testing.T) {
	t.Setenv("AW_MEMBER_FIXTURE", "1")
	s, socket := fixtureService(t)
	// The production helper deliberately receives only fixed root environment.
	// Inject the fixture selector through the command's argument via test flags.
	original := s.memberCommand
	s.memberCommand = func(p string) *exec.Cmd { cmd := original(p); cmd.Args = append(cmd.Args, "--", "fixture"); return cmd }
	// Test helper uses a fixed selector because production removes inherited env.

	var clients []*net.UnixConn
	for i := 0; i < 3; i++ {
		id := randomID()
		c, out := requestAt(t, socket, Request{Operation: "exec", Reference: s.status.Reference, ID: id, Path: "/bin/true", Env: []string{"AW_MEMBER_FIXTURE=1"}}, true)
		if out.Error != "" {
			t.Fatal(out.Error)
		}
		clients = append(clients, c)
	}
	_, busy := requestAt(t, socket, Request{Operation: "stop", Reference: s.status.Reference}, false)
	if busy.Error == "" {
		t.Fatal("busy stop accepted")
	}
	clients[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, out := requestAt(t, socket, Request{Operation: "status"}, false)
		cleaned := 0
		for _, m := range out.Status.Members {
			if m.State == "cleaned" {
				cleaned++
			}
		}
		if cleaned == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnect cleanup missing: %+v", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, busy = requestAt(t, socket, Request{Operation: "stop", Reference: s.status.Reference}, false)
	if busy.Error == "" {
		t.Fatal("one caller stopped other invocations")
	}
	for _, c := range clients[1:] {
		c.Close()
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, out := requestAt(t, socket, Request{Operation: "stop", Reference: s.status.Reference}, false)
		if out.Error == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(out.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, out := requestAt(t, socket, Request{Operation: "exec", Reference: s.status.Reference, ID: randomID(), Path: "/bin/true"}, true)
	if out.Error == "" {
		t.Fatal("admitted after stop")
	}
}
func TestAdmissionFencesAndUncertainReceipts(t *testing.T) {
	s, socket := fixtureService(t)
	ref := s.status.Reference
	ref.Identity = randomID()
	ref.Endpoint = Endpoint(ref.Name, ref.Identity)
	_, out := requestAt(t, socket, Request{Operation: "exec", Reference: ref, ID: randomID(), Path: "/bin/true"}, true)
	if out.Error == "" {
		t.Fatal("stale identity accepted")
	}
	id := randomID()
	s.mu.Lock()
	s.status.Members[id] = Membership{ID: id, UID: uint32(os.Getuid()), State: "launching"}
	s.mu.Unlock()
	_, out = requestAt(t, socket, Request{Operation: "exec", Reference: s.status.Reference, ID: id, Path: "/bin/true"}, true)
	if out.Error == "" {
		t.Fatal("lost acknowledgement replayed")
	}
	_, out = requestAt(t, socket, Request{Operation: "member", Reference: s.status.Reference, ID: id}, false)
	if out.Member.State != "launching" {
		t.Fatal(out)
	}
	if err := save(filepath.Join(s.dir, "member-"+id+".done"), Membership{ID: id, UID: uint32(os.Getuid()), State: "uncertain"}); err != nil {
		t.Fatal(err)
	}
	_, out = requestAt(t, socket, Request{Operation: "stop", Reference: s.status.Reference}, false)
	if out.Error == "" {
		t.Fatal("uncertain cleanup permitted stop")
	}
	s.mu.Lock()
	s.probe = func() error { return errors.New("DNS/API/namespace failure") }
	s.mu.Unlock()
	_, out = requestAt(t, socket, Request{Operation: "exec", Reference: s.status.Reference, ID: randomID(), Path: "/bin/true"}, true)
	if out.Error == "" {
		t.Fatal("unhealthy runtime accepted launch")
	}
}
func TestForwardOwnershipConflictsAndLoopback(t *testing.T) {
	s, socket := fixtureService(t)
	listener, err := net.Listen("unix", filepath.Join(s.run, "slirp.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var mu sync.Mutex
	var requests []map[string]any
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			var req map[string]any
			_ = json.NewDecoder(c).Decode(&req)
			mu.Lock()
			requests = append(requests, req)
			mu.Unlock()
			_, _ = c.Write([]byte(`{"return":{"id":9}}`))
			c.Close()
		}
	}()
	req := Request{Operation: "forward", Reference: s.status.Reference, Forward: Forward{HostPort: 40001, GuestPort: 8080}}
	_, out := requestAt(t, socket, req, false)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	_, out = requestAt(t, socket, req, false)
	if out.Error != "" || len(out.Status.Forwards) != 1 {
		t.Fatal(out)
	}
	req.Forward.GuestPort++
	_, out = requestAt(t, socket, req, false)
	if out.Error == "" {
		t.Fatal("forward conflict accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatal(requests)
	}
	args := requests[0]["arguments"].(map[string]any)
	if args["host_addr"] != "127.0.0.1" || args["guest_addr"] != "127.0.0.1" {
		t.Fatal(args)
	}
}
func TestReferenceAndEnvironment(t *testing.T) {
	ref := Reference{Name: "test", Identity: randomID(), Generation: 1}
	ref.Endpoint = Endpoint(ref.Name, ref.Identity)
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	changed := ref
	changed.Identity = randomID()
	if changed.Validate() == nil {
		t.Fatal("reused endpoint accepted")
	}
	env := Environment([]string{"HOME=/caller", "DOCKER_HOST=unix:///host", "CONTAINER_CONNECTION=host", "GH_TOKEN=preserved-for-command", ReferenceEnvironment + "=stale-reference"}, ref)
	refData, _ := json.Marshal(ref)
	expected := map[string]bool{ReferenceEnvironment + "=" + string(refData): true, "HOME=/caller": true, "DOCKER_HOST=" + ref.Endpoint: true, "CONTAINER_HOST=" + ref.Endpoint: true, "AGENT_WORKFLOW_NETWORK_RUNTIME=test": true, "GH_TOKEN=preserved-for-command": true}
	for _, v := range env {
		if !expected[v] {
			t.Fatal(v)
		}
		delete(expected, v)
	}
	if len(expected) != 0 {
		t.Fatal(expected)
	}
	cmd := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if err := Wrap(cmd, ref, randomID()); err != nil {
		t.Fatal(err)
	}
	var req Request
	if len(cmd.Args) != 2 {
		t.Fatal("request exposed in helper argv", cmd.Args)
	}
	var data string
	for _, value := range cmd.Env {
		if strings.HasPrefix(value, launchEnvironment+"=") {
			data = strings.TrimPrefix(value, launchEnvironment+"=")
		}
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		t.Fatal(err)
	}
	if req.Path != "/bin/echo" || req.Dir != cmd.Dir || req.Args[0] != "hello" {
		t.Fatal(req)
	}
}

func TestContainerReservationBlocksStopUntilReconciled(t *testing.T) {
	s, socket := fixtureService(t)
	id := randomID()
	req := Request{Operation: "reserve", Reference: s.status.Reference, ID: id}
	_, out := requestAt(t, socket, req, false)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	_, out = requestAt(t, socket, Request{Operation: "stop", Reference: req.Reference}, false)
	if out.Error == "" {
		t.Fatal("stop overtook container creation")
	}
	// Persisted reservation survives a lost response, and cannot be replayed.
	var saved Status
	if err := load(filepath.Join(s.dir, "status.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Members[id].State != "reserved" {
		t.Fatal(saved)
	}
	_, out = requestAt(t, socket, req, false)
	if out.Error == "" {
		t.Fatal("duplicate reservation accepted")
	}
	req.Operation = "release"
	_, out = requestAt(t, socket, req, false)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	_, out = requestAt(t, socket, Request{Operation: "stop", Reference: req.Reference}, false)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
}
func TestUnauthorizedPeerAndServerAreRejected(t *testing.T) {
	s, socket := fixtureService(t)
	if os.Getuid() == 0 {
		t.Skip("root is the explicit operator")
	}
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if trustedConnection(c) == nil {
		t.Fatal("non-root service impersonation accepted")
	}
	s.mu.Lock()
	s.config.Users = map[uint32]bool{}
	s.mu.Unlock()
	data, _ := json.Marshal(Request{Operation: "status"})
	if _, err = c.Write(data); err != nil {
		// Authentication can close the connection before the first request.
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err = c.Read(b[:]); err == nil {
		t.Fatal("unenrolled peer accepted")
	}
}

func TestStatusExposesHeldNetworkNamespace(t *testing.T) {
	s, socket := fixtureService(t)
	s.mu.Lock()
	s.status.NetworkNamespace = "net:[12345]"
	ref := s.status.Reference
	s.mu.Unlock()
	for _, operation := range []string{"status", "check"} {
		connection, response := requestAt(t, socket, Request{Operation: operation, Reference: ref}, false)
		connection.Close()
		if response.Error != "" || response.Status.NetworkNamespace != "net:[12345]" || response.Status.Reference != ref || !response.Status.Healthy {
			t.Fatalf("namespace status missing: %+v", response)
		}
	}
}
