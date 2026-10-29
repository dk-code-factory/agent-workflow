//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
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

// The authority fixture substitutes the already-tested process guardian; its
// workload adapter still consumes the private fd4 request and applies its mask.
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

// Each child has one Wait owner, used both normally and by failure teardown.
func startAuthorityPeer(t *testing.T, cmd *exec.Cmd) func() error {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	var waitErr error
	wait := func() error {
		if !waited {
			waited = true
			waitErr = cmd.Wait()
		}
		return waitErr
	}
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = wait()
		}
	})
	return wait
}

func acceptAuthorityPeer(t *testing.T, listener *net.UnixListener) *net.UnixConn {
	t.Helper()
	_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	c, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
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
	var peers []func() error
	for _, mask := range []string{"0077", "0002"} {
		cmd := exec.Command(binary, "-test.run=^TestAuthorityPeerFixture$")
		cmd.Env = append(os.Environ(), "AW_AUTHORITY_PEER=1", "AW_AUTHORITY_MASK="+mask, "AW_AUTHORITY_SOCKET="+listener.Addr().String())
		peers = append(peers, startAuthorityPeer(t, cmd))
	}
	seen := map[int]bool{}
	for range peers {
		c := acceptAuthorityPeer(t, listener)
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
	for _, wait := range peers {
		if err = wait(); err != nil {
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

func TestAuthorityFailureTeardown(t *testing.T) {
	if stage := os.Getenv("AW_AUTHORITY_FAILURE_STAGE"); stage != "" {
		var cmd *exec.Cmd
		var accepted *net.UnixConn
		// Registered first, this checks the actual resources after all teardown.
		t.Cleanup(func() {
			result := "reaped-and-closed"
			if cmd == nil || cmd.ProcessState == nil {
				result = "unreaped"
			}
			if accepted != nil {
				_ = accepted.SetReadDeadline(time.Now().Add(time.Second))
				var b [1]byte
				if _, err := accepted.Read(b[:]); !errors.Is(err, net.ErrClosed) {
					result = "connection-not-closed"
				}
			}
			if err := os.WriteFile(os.Getenv("AW_AUTHORITY_FAILURE_REPORT"), []byte(result), 0600); err != nil {
				t.Error(err)
			}
		})
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "peer"), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(binary, "-test.run=^TestAuthorityPeerFixture$")
		cmd.Env = append(os.Environ(), "AW_AUTHORITY_PEER=1", "AW_AUTHORITY_MASK=0077", "AW_AUTHORITY_SOCKET="+listener.Addr().String())
		_ = startAuthorityPeer(t, cmd)
		if stage == "accept" {
			t.Fatal("injected failure before accept")
		}
		accepted = acceptAuthorityPeer(t, listener)
		cred, err := peer(accepted)
		if err != nil {
			t.Fatal(err)
		}
		cred.Pid = -1 // Force the real authority lookup to fail with a live peer.
		if _, _, err := peerAuthority(cred); err != nil {
			t.Fatal(err)
		}
		t.Fatal("authority lookup unexpectedly succeeded")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"accept", "authority"} {
		t.Run(stage, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "teardown")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestAuthorityFailureTeardown$")
			cmd.Env = append(os.Environ(), "AW_AUTHORITY_FAILURE_STAGE="+stage, "AW_AUTHORITY_FAILURE_REPORT="+report)
			if out, err := cmd.CombinedOutput(); err == nil || ctx.Err() != nil {
				t.Fatalf("failure fixture did not finish its expected failure: %v %s", err, out)
			}
			data, err := os.ReadFile(report)
			if err != nil || string(data) != "reaped-and-closed" {
				t.Fatalf("failure teardown: %q %v", data, err)
			}
		})
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
			config := filepath.Join(dir, "containers.conf")
			if err := os.WriteFile(config, nil, 0600); err != nil {
				t.Fatal(err)
			}
			req := Request{Path: "/bin/sh", Args: []string{"-c", `set -e; test -z "${AGENT_WORKFLOW_NETWORK_REQUEST+x}"; test "$FAKE_PROVIDER_CREDENTIAL" = credential-only-in-private-environment; umask; touch created; mkdir directory`}, Dir: dir, Umask: mask, Env: []string{"HOME=" + dir, "CONTAINERS_CONF=" + config, "PATH=/usr/bin:/bin", "AW_AUTHORITY_FIXTURE=1", "FAKE_PROVIDER_CREDENTIAL=credential-only-in-private-environment"}}
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
			cmd := exec.Command(binary, "__network_runtime_workload_fixture_v1", t.TempDir())
			cmd.ExtraFiles = []*os.File{life, r}
			ambient := hostileRoutingDefaults(t)
			cmd.Env = []string{"PATH=/usr/bin:/bin", launchEnvironment + "=must-not-reach-workload", "HOME=" + filepath.Join(ambient, "account"), "CONTAINERS_CONF=" + filepath.Join(ambient, "system/containers.conf")}
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

func hostileRoutingDefaults(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"account/.config/containers/containers.conf", "system/containers.conf"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("[engine.service_destinations.saved]\nuri='ssh://outside'\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestWorkloadFixtureRetainsSavedRoutingValidation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"account", "system", "request"} {
		t.Run(source, func(t *testing.T) {
			root := hostileRoutingDefaults(t)
			config := filepath.Join(root, "system/containers.conf")
			// Keep only the tested source, so every native discovery path must
			// independently reject its saved route before the guardian starts.
			if source != "account" {
				if err := os.Remove(filepath.Join(root, "account/.config/containers/containers.conf")); err != nil {
					t.Fatal(err)
				}
			}
			if source != "system" {
				if err := os.Rename(config, filepath.Join(root, "selected.conf")); err != nil {
					t.Fatal(err)
				}
				config = filepath.Join(root, "selected.conf")
			}
			req := Request{Path: "/bin/true", Env: []string{"HOME=" + t.TempDir()}}
			if source == "request" {
				req.Env = append(req.Env, "CONTAINERS_CONF="+config)
			}
			payload, err := sealedPayload(req)
			if err != nil {
				t.Fatal(err)
			}
			defer payload.Close()
			life, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer life.Close()
			cmd := exec.Command(binary, "__network_runtime_workload_fixture_v1", root)
			cmd.ExtraFiles = []*os.File{life, payload}
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "saved non-Unix container API transport") {
				t.Fatalf("%s routing reached guardian: %s: %v", source, out, err)
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
