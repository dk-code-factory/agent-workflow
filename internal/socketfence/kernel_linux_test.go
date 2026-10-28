//go:build linux

package socketfence

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// This opt-in uses only disposable recording APIs, never a container daemon.
// Ordinary make test must neither install BPF programs nor create host cgroups.
func TestKernelSocketBoundary(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SOCKET_LSM_TEST") != "1" {
		t.Skip("explicit BPF LSM certification opt-in required")
	}
	if os.Geteuid() != 0 {
		t.Fatal("BPF LSM certification requires an explicit root operator")
	}
	if err := Supported(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var outside, selected, auth atomic.Int32
	record := func(name string, count *atomic.Int32) string {
		path := filepath.Join(dir, name)
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" {
				count.Add(1)
			}
			w.WriteHeader(200)
		})}
		go server.Serve(listener)
		t.Cleanup(func() { server.Close() })
		return path
	}
	out := record("outside", &outside)
	api := record("selected", &selected)
	authSocket := record("authentication", &auth)
	alias := filepath.Join(dir, "saved-custom-path")
	if err := os.Symlink(out, alias); err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	enrolled, held, err := OpenSocket(authSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	boundary, err := Install(hex.EncodeToString(id[:]), 7, []string{api}, []Socket{enrolled})
	if err != nil {
		t.Fatal(err)
	}
	defer boundary.Close()
	defer func() {
		if err := boundary.Retire(); err != nil {
			t.Error(err)
		}
	}()
	retained, err := Restore(boundary.State)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	stale := boundary.State
	stale.Generation++
	if unexpected, err := Restore(stale); err == nil {
		unexpected.Close()
		t.Fatal("changed generation accepted")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"attached", "service-crashed"} {
		if phase == "service-crashed" {
			boundary.Close()
		}
		cmd := exec.Command(binary, "-test.run=^TestKernelSocketChild$")
		cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(retained.Group.Fd())}
		cmd.Env = []string{"PATH=/usr/bin:/bin", "AW_KERNEL_SOCKET_CHILD=1", "AW_KERNEL_OUTSIDE=" + alias, "AW_KERNEL_SELECTED=" + api, "AW_KERNEL_AUTH=" + authSocket}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %s: %v", phase, output, err)
		}
	}
	if outside.Load() != 0 || selected.Load() != 4 || auth.Load() != 4 {
		t.Fatalf("recorder counts outside=%d selected=%d auth=%d", outside.Load(), selected.Load(), auth.Load())
	}
}

func TestKernelSocketChild(t *testing.T) {
	if os.Getenv("AW_KERNEL_SOCKET_CHILD") != "1" {
		return
	}
	for key, allowed := range map[string]bool{"AW_KERNEL_OUTSIDE": false, "AW_KERNEL_SELECTED": true, "AW_KERNEL_AUTH": true} {
		path := os.Getenv(key)
		transport := &http.Transport{Dial: func(_, _ string) (net.Conn, error) { return net.DialTimeout("unix", path, time.Second) }}
		client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
		response, err := client.Post("http://record/networks/create", "application/json", nil)
		if response != nil {
			response.Body.Close()
		}
		transport.CloseIdleConnections()
		if (err == nil) != allowed {
			t.Fatalf("%s allowed=%v: %v", key, allowed, err)
		}
	}
	// A workload's own Unix IPC remains available and cannot be confused with a
	// host socket merely because it shares the original filesystem namespace.
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "owned"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if os.Getenv("AW_KERNEL_DESCENDANT") == "1" {
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestKernelSocketChild$")
	cmd.Env = append(os.Environ(), "AW_KERNEL_DESCENDANT=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("descendant: %s: %v", output, err)
	}
}
