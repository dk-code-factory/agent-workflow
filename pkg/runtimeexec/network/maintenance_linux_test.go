//go:build linux

package network

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIdleHistoryPersistsOnlyTransitions(t *testing.T) {
	s, _ := fixtureService(t)
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("%032x", i)
		s.status.Members[id] = Membership{ID: id, State: "cleaned"}
	}
	id := randomID()
	m := Membership{ID: id, UID: uint32(os.Getuid()), State: "launching", ExitCode: 125}
	s.status.Members[id] = m
	if err := s.saveStatus(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "status.json")
	unchanged := func() {
		t.Helper()
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 12; i++ {
			if err := s.refreshStatus(); err != nil {
				t.Fatal(err)
			}
		}
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("unchanged history was replaced: %v", err)
		}
	}
	unchanged()
	for _, state := range []string{"uncertain", "cleaned"} {
		m.State, m.ExitCode = state, 7
		if err := save(filepath.Join(s.dir, "member-"+id+".done"), m); err != nil {
			t.Fatal(err)
		}
		// RPC reconciliation can observe a receipt before the ticker does.
		s.reconcile()
		if !s.statusPending {
			t.Fatal("receipt transition was not marked for persistence")
		}
		if err := s.refreshStatus(); err != nil {
			t.Fatal(err)
		}
		var durable Status
		if err := load(path, &durable); err != nil || durable.Members[id] != m || len(durable.Members) != 10001 {
			t.Fatalf("receipt/history not durable: %v %+v", err, durable.Members[id])
		}
		unchanged()
	}
	s.probe = func() error { return errors.New("namespace unavailable") }
	if err := s.refreshStatus(); err != nil {
		t.Fatal(err)
	}
	var durable Status
	if err := load(path, &durable); err != nil || durable.Healthy {
		t.Fatalf("health transition not durable: %v", err)
	}
	unchanged()
}

func TestStatusTransitionRetriesFailedPersistence(t *testing.T) {
	for _, transition := range []string{"receipt", "health", "mutation"} {
		t.Run(transition, func(t *testing.T) {
			s, socket := fixtureService(t)
			id := randomID()
			m := Membership{ID: id, UID: uint32(os.Getuid()), State: "launching"}
			s.mu.Lock()
			s.status.Members[id] = m
			if err := s.saveStatus(); err != nil {
				t.Fatal(err)
			}
			s.mu.Unlock()
			path := filepath.Join(s.dir, "status.json")
			if err := os.Mkdir(path+".partial", 0700); err != nil {
				t.Fatal(err)
			}
			switch transition {
			case "receipt":
				m.State = "uncertain"
				if err := save(filepath.Join(s.dir, "member-"+id+".done"), m); err != nil {
					t.Fatal(err)
				}
			case "health":
				s.mu.Lock()
				s.probe = func() error { return errors.New("probe failed") }
				s.mu.Unlock()
				c, _ := requestAt(t, socket, Request{Operation: "status"}, false)
				c.Close()
			case "mutation":
				c, out := requestAt(t, socket, Request{Operation: "reserve", Reference: s.status.Reference, ID: randomID()}, false)
				c.Close()
				if out.Error == "" {
					t.Fatal("failed mutation persistence reported success")
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := s.refreshStatus(); err == nil || !s.statusPending {
				t.Fatal("failed transition lost its pending write")
			}
			var before Status
			if err := load(path, &before); err != nil || !before.Healthy || before.Members[id].State != "launching" || len(before.Members) != 1 {
				t.Fatalf("failed write replaced durable state: %v", err)
			}
			if err := os.Remove(path + ".partial"); err != nil {
				t.Fatal(err)
			}
			if err := s.refreshStatus(); err != nil || s.statusPending {
				t.Fatalf("transition retry failed: %v", err)
			}
			var after Status
			if err := load(path, &after); err != nil || !reflect.DeepEqual(after, s.status) {
				t.Fatalf("retried state not durable: %v", err)
			}
		})
	}
}

func TestMemberCommandPreservesNativeGroupSets(t *testing.T) {
	for _, count := range []int{-1, 0, 3, 20000, 65536} {
		for _, uid := range []uint32{0, 1000} {
			t.Run(fmt.Sprintf("groups=%d/uid=%d", count, uid), func(t *testing.T) {
				var groups []uint32
				if count >= 0 {
					groups = make([]uint32, count)
					for i := range groups {
						groups[i] = uint32(100000 + i)
					}
				}
				want := memberLaunch{UID: uid, GID: 1001, Groups: groups, Binary: "/trusted/launcher", Request: Request{Path: "/caller/program", Args: []string{"private-argument"}, Env: []string{"PRIVATE=secret"}}}
				payload, err := launchFile(want)
				if err != nil {
					t.Fatal(err)
				}
				defer payload.Close()
				var launch memberLaunch
				if err := decodeSealedPayload(payload, &launch); err != nil {
					t.Fatal(err)
				}
				cmd := memberWorkloadCommand(launch, "nonsecret-id")
				cred := cmd.SysProcAttr.Credential
				if cred.Uid != 0 || cred.Gid != uint32(os.Getegid()) || cred.NoSetGroups || !reflect.DeepEqual(cred.Groups, groups) {
					t.Fatalf("captured group authority changed: %+v", cred)
				}
				args := []string{"/usr/bin/nsenter", "--net=/proc/self/fd/5", "--mount=/proc/self/fd/6", "--", "/usr/bin/setpriv", "--reuid", strconv.FormatUint(uint64(uid), 10), "--regid", "1001", "--keep-groups", "/trusted/launcher", "__network_runtime_workload_v1", "nonsecret-id"}
				if cmd.Path != args[0] || !reflect.DeepEqual(cmd.Args, args) || len(strings.Join(cmd.Args, "\x00")) > 256 {
					t.Fatalf("group set grew/changed root helper argv: %q", cmd.Args)
				}
				if cmd.SysProcAttr.Cloneflags != 0 || len(cmd.SysProcAttr.AmbientCaps) != 0 {
					t.Fatal("group transport changed namespace/capability authority")
				}
			})
		}
	}
}

func TestPublicModesRejectUntrustedPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writable")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0777); err != nil {
		t.Fatal(err)
	}
	if err := publicDir(path); err == nil {
		t.Fatal("untrusted directory accepted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0777 {
		t.Fatal("untrusted directory changed before validation")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := publicDir(link); err == nil {
		t.Fatal("symlink accepted as managed directory")
	}
	f, err := os.CreateTemp(t.TempDir(), "launcher")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("preserve rejected launcher contents"); err != nil {
		t.Fatal(err)
	}
	if err := f.Chmod(0666); err != nil {
		t.Fatal(err)
	}
	if err := publicLauncher(f); err == nil {
		t.Fatal("untrusted launcher accepted")
	}
	info, _ = f.Stat()
	if info.Mode().Perm() != 0666 {
		t.Fatal("untrusted launcher changed before validation")
	}
	contents, err := os.ReadFile(f.Name())
	if err != nil || string(contents) != "preserve rejected launcher contents" {
		t.Fatal("rejected launcher contents changed")
	}
}

func TestResolverModeIgnoresOperatorUmask(t *testing.T) {
	if os.Getenv("AW_RESOLVER_MODE_FIXTURE") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^TestResolverModeIgnoresOperatorUmask$")
		cmd.Env = append(os.Environ(), "AW_RESOLVER_MODE_FIXTURE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("resolver mode fixture: %v\n%s", err, out)
		}
		return
	}
	unix.Umask(0077)
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := writeResolver(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("public resolver mode: %v %v", info, err)
	}
}

// Runs only when the test caller is already root. Ordinary tests never elevate.
func TestRestrictiveOperatorUmaskPublicModes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires an already-root test caller; no elevation")
	}
	if os.Getenv("AW_PUBLIC_MODES_FIXTURE") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^TestRestrictiveOperatorUmaskPublicModes$")
		cmd.Env = append(os.Environ(), "AW_PUBLIC_MODES_FIXTURE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restrictive mask fixture: %v\n%s", err, out)
		}
		return
	}
	unix.Umask(0077) // Isolated child only; the service never changes its mask.
	base := t.TempDir()
	for _, relative := range []string{"agent-workflow", "agent-workflow/runtimes", "agent-workflow/runtimes/test", "run", "run/test", "run/test/generation"} {
		path := filepath.Join(base, relative)
		if err := publicDir(path); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("managed traversal mode: %v %v", info, err)
		}
	}
	for _, mode := range []os.FileMode{0755, 0600} {
		path := filepath.Join(base, "launcher.partial")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			t.Fatal(err)
		}
		if err := publicLauncher(f); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := os.Rename(path, filepath.Join(base, "launcher")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(base, "launcher"))
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("launcher mode: %v %v", info, err)
		}
		if mode == 0755 {
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := save(filepath.Join(base, "status.json"), Status{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(base, "status.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private status mode: %v %v", info, err)
	}
	info, _ = os.Stat(base)
	if info.Mode().Perm() != 0700 {
		t.Fatal("unrelated ancestor permissions changed")
	}
}
