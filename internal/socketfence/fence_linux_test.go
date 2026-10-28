//go:build linux

package socketfence

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProductionPolicyDecisionsWithRecordingAPI(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Fatal("deterministic policy checks require a C compiler:", err)
	}
	binary := filepath.Join(t.TempDir(), "policy")
	if out, err := exec.Command(compiler, "-O2", "-Wall", "-Werror", "-o", binary, "bpf/fixture.c").CombinedOutput(); err != nil {
		t.Fatalf("compile production policy fixture: %s: %v", out, err)
	}
	var mutations atomic.Int32
	socket := filepath.Join(t.TempDir(), "outside-custom-api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			mutations.Add(1)
		}
		w.WriteHeader(200)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	type scenario struct {
		name, current, peer, ino, dev, ready, prior, abstract string
		want                                                  int
	}
	cases := []scenario{
		{"outside-custom", "42", "9", "999", "7", "1", "0", "0", -1},
		{"outside-default", "42", "9", "999", "7", "1", "0", "0", -1},
		{"same-inode-other-filesystem", "42", "9", "123", "8", "1", "0", "0", -1},
		{"host-abstract", "42", "9", "0", "0", "1", "0", "1", -1},
		{"runtime-api", "42", "9", "123", "7", "1", "0", "0", 0},
		{"enrolled-authentication-alias", "42", "9", "123", "7", "1", "0", "0", 0},
		{"runtime-owned-local", "42", "42", "999", "7", "1", "0", "0", 0},
		{"runtime-owned-abstract", "42", "42", "0", "0", "1", "0", "1", 0},
		{"other-membership-generation", "42", "43", "999", "7", "1", "0", "0", -1},
		{"initialization-incomplete", "42", "42", "123", "7", "0", "0", "0", -1},
		{"prior-lsm-denial", "42", "42", "123", "7", "1", "-13", "0", -13},
		{"unselected-caller", "9", "9", "999", "7", "1", "0", "0", 0},
	}
	// Lost routing and hook behavior never enter the kernel decision inputs.
	for _, hook := range []string{"absent", "disabled", "untrusted", "modified", "crashed", "timed-out", "competing-rewrite", "outside-hook-mcp"} {
		for _, tc := range cases {
			t.Run(hook+"/"+tc.name, func(t *testing.T) {
				cmd := exec.Command(binary, tc.current, tc.peer, tc.ino, tc.dev, tc.ready, tc.prior, tc.abstract)
				cmd.Env = []string{"PATH=/usr/bin:/bin", "DOCKER_HOST=unix://" + socket, "HOOK_STATE=" + hook}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v", out, err)
				}
				var stream, datagram int
				if _, err = fmt.Sscanf(string(out), "%d %d", &stream, &datagram); err != nil || stream != tc.want || datagram != tc.want {
					t.Fatalf("got %s, want %d: %v", out, tc.want, err)
				}
				before := mutations.Load()
				// A positive control proves the recorder observes mutating requests;
				// denied C decisions never proceed to a userspace connect/request.
				if stream == 0 {
					transport := &http.Transport{Dial: func(_, _ string) (net.Conn, error) { return net.Dial("unix", socket) }}
					defer transport.CloseIdleConnections()
					client := &http.Client{Transport: transport}
					response, err := client.Post("http://fixture/networks/create", "application/json", strings.NewReader("{}"))
					if err != nil {
						t.Fatal(err)
					}
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				wantDelta := int32(0)
				if stream == 0 {
					wantDelta = 1
				}
				if mutations.Load()-before != wantDelta {
					t.Fatal("unexpected outside-API mutation")
				}
			})
		}
	}
}

func TestSocketIdentityAliasesAndReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	expected, held, err := OpenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	alias := filepath.Join(dir, "saved-custom-path")
	if err = os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	actual, f, err := OpenSocket(alias)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if actual.Inode != expected.Inode || actual.Device != expected.Device {
		t.Fatal("alias changed resolved identity")
	}
	listener.Close()
	replacement, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	actual, f, err = OpenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if actual.Inode == expected.Inode {
		t.Fatal("replaced authentication socket retained enrolled identity")
	}
	if _, _, err = OpenSocket("relative"); err == nil {
		t.Fatal("relative authentication socket accepted")
	}
	if _, _, err = OpenSocket(dir); err == nil {
		t.Fatal("directory enrolled as socket")
	}
}

func TestEnrolledSocketRetainsMatchingAliasIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	alias := path + "-alias"
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	expected, registered, err := OpenSocket(alias)
	if err != nil {
		t.Fatal(err)
	}
	registered.Close()
	held, err := openEnrolledSocket(expected)
	if err != nil {
		t.Fatal(err)
	}
	b := &Boundary{sockets: []*os.File{held}}
	defer b.Close()
	listener.Close()
	if _, err := held.Stat(); err != nil {
		t.Fatal("enrolled inode handle was not retained:", err)
	}
	b.Close()
	if _, err := held.Stat(); err == nil {
		t.Fatal("boundary close retained the inode handle")
	}
}

func TestInstallRejectsChangedEnrollmentBeforeKernelSetup(t *testing.T) {
	for _, change := range []string{"replacement", "symlink", "missing"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "auth")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetUnlinkOnClose(false)
			expected, registered, err := OpenSocket(path)
			if err != nil {
				t.Fatal(err)
			}
			registered.Close()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if change != "missing" {
				replacementPath := path
				if change == "symlink" {
					replacementPath += "-other"
				}
				replacement, err := net.Listen("unix", replacementPath)
				if err != nil {
					t.Fatal(err)
				}
				defer replacement.Close()
				if change == "symlink" {
					if err := os.Symlink(replacementPath, path); err != nil {
						t.Fatal(err)
					}
				}
			}
			// The first open succeeds, then the registered identity must fail.
			// Install must reject before Supported or any kernel policy work.
			runtimePath := filepath.Join(dir, "runtime")
			runtimeListener, err := net.Listen("unix", runtimePath)
			if err != nil {
				t.Fatal(err)
			}
			defer runtimeListener.Close()
			matching, checked, err := OpenSocket(runtimePath)
			if err != nil {
				t.Fatal(err)
			}
			checked.Close()
			boundary, err := Install(strings.Repeat("a", 32), 1, []string{runtimePath}, []Socket{matching, expected})
			if boundary != nil || err == nil || !strings.Contains(err.Error(), "enrolled socket identity changed") {
				t.Fatalf("changed identity reached kernel setup: boundary=%v err=%v", boundary, err)
			}
			fds, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			for _, fd := range fds {
				target, _ := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
				if target == runtimePath || target == path || target == path+"-other" {
					t.Fatalf("failed enrollment retained inode handle %s", target)
				}
			}
		})
	}
}

func TestEmbeddedPolicyContract(t *testing.T) {
	source, err := os.ReadFile("bpf/fence.c")
	if err != nil {
		t.Fatal(err)
	}
	object, err := elf.NewFile(bytes.NewReader(_FenceBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	section := object.Section(".note.fence")
	if section == nil {
		t.Fatal("embedded policy missing source identity; run go generate ./internal/socketfence")
	}
	digest, err := section.Data()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(string(digest), "\x00") != fmt.Sprintf("%x", sha256.Sum256(source)) {
		t.Fatal("C source differs from embedded policy; run go generate ./internal/socketfence")
	}
	spec, err := loadFence()
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"fence_stream": "unix_stream_connect", "fence_datagram": "unix_may_send"} {
		program := spec.Programs[name]
		if program == nil || program.AttachTo != target {
			t.Fatalf("missing resolved-peer hook %s", name)
		}
	}
	if spec.Maps["allowed"].KeySize != 16 || spec.Maps["policy"].ValueSize != 40 {
		t.Fatal("policy ABI changed")
	}
	if _, err := Restore(State{Identity: strings.Repeat("a", 32), Generation: 1, Cgroup: "/tmp/untrusted", Pins: "/tmp/untrusted"}); err == nil {
		t.Fatal("untrusted retained policy accepted")
	}
	if (*Boundary)(nil).Check() == nil {
		t.Fatal("missing enforcement accepted")
	}
}
