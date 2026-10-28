//go:build linux

package network

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The network test binary substitutes only the already-tested process guardian;
// workload() still consumes the production fd4 request and applies its mask.
func init() {
	if len(os.Args) > 4 && os.Args[1] == "__agent_workflow_guardian_v1" && os.Getenv("AW_AUTHORITY_FIXTURE") == "1" {
		target := 3
		if os.Args[target] == "--clean-ack" {
			target += 2
		}
		if err := syscall.Exec(os.Args[target], os.Args[target:], os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(125)
		}
	}
}

func TestAuthorityPeerFixture(t *testing.T) {
	if os.Getenv("AW_AUTHORITY_PEER") != "1" {
		return
	}
	mask, err := strconv.ParseInt(os.Getenv("AW_AUTHORITY_MASK"), 8, 32)
	if err != nil {
		os.Exit(125)
	}
	unix.Umask(int(mask))
	c, err := net.Dial("unix", os.Getenv("AW_AUTHORITY_SOCKET"))
	if err != nil {
		os.Exit(125)
	}
	_, _ = io.Copy(io.Discard, c)
	c.Close()
	os.Exit(0)
}

func TestAuthenticatedCallersRetainDistinctUmasks(t *testing.T) {
	self := &unix.Ucred{Pid: int32(os.Getpid()), Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	_, before, err := peerAuthority(self)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "peer"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var peers []*exec.Cmd
	for _, mask := range []string{"0077", "0002"} {
		cmd := exec.Command(binary, "-test.run=^TestAuthorityPeerFixture$")
		cmd.Env = append(os.Environ(), "AW_AUTHORITY_PEER=1", "AW_AUTHORITY_MASK="+mask, "AW_AUTHORITY_SOCKET="+listener.Addr().String())
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		peers = append(peers, cmd)
	}
	seen := map[int]bool{}
	for range peers {
		listener.SetDeadline(time.Now().Add(5 * time.Second))
		c, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		cred, err := peer(c)
		if err != nil {
			t.Fatal(err)
		}
		_, mask, err := peerAuthority(cred)
		if err != nil {
			t.Fatal(err)
		}
		seen[mask] = true
		c.Close()
	}
	for _, cmd := range peers {
		if err = cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if !seen[0077] || !seen[0002] || len(seen) != 2 {
		t.Fatal(seen)
	}
	_, after, err := peerAuthority(self)
	if err != nil || before != after {
		t.Fatalf("shared caller mask changed %o -> %o: %v", before, after, err)
	}
}

func TestIsolatedWorkloadUmaskAndPrivateEnvironment(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mask := range []int{0077, 0002} {
		t.Run(fmt.Sprintf("%04o", mask), func(t *testing.T) {
			dir := t.TempDir()
			req := Request{Path: "/bin/sh", Args: []string{"-c", `set -e; test -z "${AGENT_WORKFLOW_NETWORK_REQUEST+x}"; test "$FAKE_PROVIDER_CREDENTIAL" = credential-only-in-private-environment; umask; touch created; mkdir directory`}, Dir: dir, Umask: mask, Env: []string{"PATH=/usr/bin:/bin", "AW_AUTHORITY_FIXTURE=1", "FAKE_PROVIDER_CREDENTIAL=credential-only-in-private-environment"}}
			data, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			life, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer life.Close()
			cmd := exec.Command(binary, "__network_runtime_workload_v1", "fixture")
			cmd.ExtraFiles = []*os.File{life, r}
			cmd.Env = []string{"PATH=/usr/bin:/bin", launchEnvironment + "=must-not-reach-workload"}
			if _, err = w.Write(data); err != nil {
				t.Fatal(err)
			}
			w.Close()
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v", out, err)
			}
			got, err := strconv.ParseInt(strings.TrimSpace(string(out)), 8, 32)
			if err != nil || int(got) != mask {
				t.Fatalf("mask %q: %v", out, err)
			}
			for name, expected := range map[string]os.FileMode{"created": os.FileMode(0666 &^ mask), "directory": os.FileMode(0777 &^ mask)} {
				info, err := os.Stat(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != expected {
					t.Fatalf("%s: %o want %o", name, info.Mode().Perm(), expected)
				}
			}
		})
	}
}

func TestLaunchTransportExcludesSecretsAndSelectorsFromWorkload(t *testing.T) {
	ref := Reference{Name: "test", Identity: randomID(), Generation: 1}
	ref.Endpoint = Endpoint(ref.Name, ref.Identity)
	cmd := exec.Command("/bin/true")
	cmd.Env = []string{"FAKE_PROVIDER_CREDENTIAL=credential-only-in-private-environment", launchEnvironment + "=obsolete-secret", "AGENT_WORKFLOW_NETWORK_TERMINAL=1"}
	cleanup, err := Wrap(cmd, ref, randomID())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if strings.Contains(strings.Join(cmd.Args, " "), "credential") || len(cmd.Args) != 2 {
		t.Fatal(cmd.Args)
	}
	var request Request
	if err := decodePayload(cmd.ExtraFiles[0], &request); err != nil {
		t.Fatal(err)
	}
	for _, value := range cmd.Env {
		if strings.HasPrefix(value, launchEnvironment+"=") {
			t.Fatal("payload in environment")
		}
	}
	if !request.Terminal {
		t.Fatal("terminal request not consumed")
	}
	for _, value := range request.Env {
		if strings.HasPrefix(value, launchEnvironment+"=") || strings.HasPrefix(value, "AGENT_WORKFLOW_NETWORK_TERMINAL=") {
			t.Fatal("transport selector leaked", value)
		}
	}
	launch, err := launchFile(memberLaunch{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	seals, err := unix.FcntlInt(launch.Fd(), unix.F_GET_SEALS, 0)
	if err != nil {
		t.Fatal(err)
	}
	expected := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if seals&expected != expected {
		t.Fatal("root launch data unsealed", seals)
	}
	if _, err = launch.Write([]byte("changed")); err == nil {
		t.Fatal("root launch data writable")
	}
}
