//go:build linux

package network

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnhealthyRecoveryRetainsReferenceAndAdmissionFences(t *testing.T) {
	s, socket := fixtureService(t)
	ref := s.status.Reference
	id := randomID()
	_, out := requestAt(t, socket, Request{Operation: "reserve", Reference: ref, ID: id}, false)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	s.mu.Lock()
	s.status.Healthy = false
	s.probe = func() error { return errors.New("failed admission health") }
	s.mu.Unlock()
	for _, op := range []string{"check", "reserve", "exec", "forward"} {
		_, out = requestAt(t, socket, Request{Operation: op, Reference: ref, ID: randomID()}, op == "exec")
		if out.Error == "" {
			t.Fatalf("unhealthy %s admitted", op)
		}
	}
	_, out = requestAt(t, socket, Request{Operation: "cleanup-check", Reference: ref}, false)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	stale := ref
	stale.Generation++
	_, out = requestAt(t, socket, Request{Operation: "cleanup-check", Reference: stale}, false)
	if out.Error == "" {
		t.Fatal("stale cleanup reference accepted")
	}
	_, out = requestAt(t, socket, Request{Operation: "release", Reference: ref, ID: id}, false)
	if out.Error != "" {
		t.Fatal(out.Error)
	}
}

func TestUnhealthyProxyChecksEveryRequestAndDeniesUpgrade(t *testing.T) {
	for _, second := range []string{
		"POST /v5.0.0/libpod/containers/create HTTP/1.1\r\nHost: podman\r\nContent-Length: 0\r\n\r\n",
		"GET /v5.0.0/libpod/info HTTP/1.1\r\nHost: podman\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n",
		"DELETE /v5.0.0/libpod/containers/arbitrary-name HTTP/1.1\r\nHost: podman\r\n\r\n",
	} {
		t.Run(strings.Fields(second)[0]+strings.Fields(second)[1], func(t *testing.T) {
			s, _ := fixtureService(t)
			upstream, err := net.Listen("unix", filepath.Join(s.run, "api.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer upstream.Close()
			reached := make(chan string, 4)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached <- r.Method + " " + r.URL.Path
				fmt.Fprint(w, "{}")
			})}
			go server.Serve(upstream)
			defer server.Close()
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "proxy"), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				c, e := listener.AcceptUnix()
				if e == nil {
					defer c.Close()
					s.proxyCleanup(c)
				}
			}()
			c, err := net.Dial("unix", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			_, err = io.WriteString(c, "GET /v5.0.0/libpod/info HTTP/1.1\r\nHost: podman\r\n\r\n"+second)
			if err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(c)
			first, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, first.Body)
			first.Body.Close()
			denied, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			denied.Body.Close()
			if denied.StatusCode != 503 {
				t.Fatal(denied.Status)
			}
			if got := <-reached; got != "GET /v5.0.0/libpod/info" {
				t.Fatal(got)
			}
			select {
			case extra := <-reached:
				t.Fatal("unsafe request forwarded", extra)
			default:
			}
		})
	}
}

func TestCleanupRoutesRequireExactDeleteID(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, path := range []string{"/v5.0.0/libpod/info", "/v5.0.0/libpod/_ping", "/v5.0.0/libpod/containers/json", "/v5.0.0/libpod/containers/owned/json"} {
		if !cleanupRoute(http.MethodGet, path) {
			t.Fatal(path)
		}
	}
	if !cleanupRoute(http.MethodDelete, "/v5.0.0/libpod/containers/"+id) {
		t.Fatal("exact deletion denied")
	}
	for _, path := range []string{"/containers/name", "/containers/" + id + "/start", "/containers/" + id + "/../other", "/containers/json/extra", "/containers/%2f/json"} {
		if cleanupRoute(http.MethodDelete, path) {
			t.Fatal(path)
		}
	}
}

func TestReconcileRetainsMissingAndMismatchedReceipts(t *testing.T) {
	s, _ := fixtureService(t)
	id := randomID()
	s.status.Members[id] = Membership{ID: id, UID: uint32(os.Getuid()), State: "launching"}
	for _, receipt := range []Membership{{ID: randomID(), UID: uint32(os.Getuid()), State: "cleaned"}, {ID: id, UID: uint32(os.Getuid()) + 1, State: "cleaned"}} {
		if err := save(filepath.Join(s.dir, "member-"+id+".done"), receipt); err != nil {
			t.Fatal(err)
		}
		s.reconcile()
		if s.status.Members[id].State != "launching" {
			t.Fatal("unproved receipt erased uncertainty")
		}
	}
	if err := os.Remove(filepath.Join(s.dir, "member-"+id+".done")); err != nil {
		t.Fatal(err)
	}
	s.reconcile()
	if s.status.Members[id].State != "launching" {
		t.Fatal("missing receipt erased uncertainty")
	}
}

// Model controller death using a separate process so the OS, rather than test
// cleanup, closes the service's only lifetime writer while a member is active.
func TestDaemonLifetimeFixture(t *testing.T) {
	if os.Getenv("AW_DAEMON_LIFETIME_FIXTURE") != "1" {
		return
	}
	lifeR, lifeW, err := os.Pipe()
	if err != nil {
		os.Exit(125)
	}
	binary, err := os.Executable()
	if err != nil {
		os.Exit(125)
	}
	member := exec.Command(binary, "-test.run=^TestMemberFixture$", "--", "fixture")
	member.ExtraFiles = []*os.File{lifeR, os.NewFile(3, "sealed-launch")}
	memberOutput, err := member.StdoutPipe()
	if err != nil {
		os.Exit(125)
	}
	member.Stderr = os.Stderr
	if err = member.Start(); err != nil {
		os.Exit(125)
	}
	lifeR.Close()
	var started [14]byte
	if _, err = io.ReadFull(memberOutput, started[:]); err != nil {
		fmt.Fprintln(os.Stderr, "member readiness:", err)
		os.Exit(125)
	}
	fmt.Fprintln(os.Stdout, "active")
	// Keep the writer reachable until SIGKILL; the member must outlive this daemon
	// long enough to confirm cleanup and leave its durable receipt.
	defer lifeW.Close()
	for {
		time.Sleep(time.Hour)
	}
}

func TestActiveMemberDaemonLossLeavesDurableCompletionReceipt(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "member.done")
	id := randomID()
	launch, err := launchFile(memberLaunch{Request: Request{ID: id, Env: []string{"fixture"}}, UID: uint32(os.Getuid()), Receipt: receipt})
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	daemon := exec.Command(binary, "-test.run=^TestDaemonLifetimeFixture$")
	daemon.ExtraFiles = []*os.File{launch}
	daemon.Stderr = os.Stderr
	daemon.Env = append(os.Environ(), "AW_DAEMON_LIFETIME_FIXTURE=1")
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Process.Kill(); daemon.Wait() })
	ready := make(chan error, 1)
	go func() {
		line, e := bufio.NewReader(out).ReadString('\n')
		if e == nil && line != "active\n" {
			e = fmt.Errorf("unexpected readiness %q", line)
		}
		ready <- e
	}()
	select {
	case err = <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon fixture not ready")
	}
	if _, err = os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatal("active member already completed")
	}
	if err = daemon.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	daemon.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var member Membership
		if load(receipt, &member) == nil {
			if member.ID != id || member.UID != uint32(os.Getuid()) || member.State != "cleaned" {
				t.Fatal(member)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controller death lost member completion receipt")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
