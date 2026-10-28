//go:build linux

package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type mountRoutingFixture struct{ root, home, stage, endpoint string }

func TestRoutingTargetsCoalesceSocketAliases(t *testing.T) {
	paths := []routingTarget{{path: "/run/docker.sock", kind: "socket"}, {path: "/run/docker.sock", kind: "socket"}}
	if link, err := os.Readlink("/var/run"); err == nil && (link == "/run" || filepath.Clean(filepath.Join("/var", link)) == "/run") {
		paths = append(paths, routingTarget{path: "/var/run/docker.sock", kind: "socket"})
	}
	got, err := uniqueRoutingTargets(paths)
	if err != nil || len(got) != 1 || got[0].path != "/run/docker.sock" {
		t.Fatalf("ordinary socket aliases conflict: %+v, %v", got, err)
	}
	if _, err := uniqueRoutingTargets(append(paths, routingTarget{path: "/run/docker.sock", kind: "file"})); err == nil {
		t.Fatal("configuration file can replace socket projection")
	}
}

func newMountRoutingFixture(t *testing.T) mountRoutingFixture {
	t.Helper()
	f := mountRoutingFixture{root: t.TempDir()}
	f.home = filepath.Join(f.root, "home")
	f.stage = filepath.Join(f.root, "stage")
	for _, p := range []string{f.home, f.stage} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(f.root, "proxy.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	f.endpoint = "unix://" + path
	return f
}
func fakeRoutingMountOps(mount func(string, string, string, uintptr, string) error) routingMountOps {
	return routingMountOps{mount: mount, unmount: func(string, int) error { return nil }, seal: func(int) error { return nil }, owner: uint32(os.Geteuid()), trusted: func(string, unix.Stat_t) bool { return true }}
}
func TestRoutingMountRejectsUnsafePlansBeforeMount(t *testing.T) {
	for _, name := range []string{"symlink-file", "symlink-ancestor", "hardlink-file", "wrong-kind", "unclean", "overlap", "staging", "staging-graft", "endpoint-file", "staging-permissions"} {
		t.Run(name, func(t *testing.T) {
			f := newMountRoutingFixture(t)
			path := filepath.Join(f.home, "config.json")
			if err := os.WriteFile(path, []byte("host-secret"), 0600); err != nil {
				t.Fatal(err)
			}
			plan := routingPlan{Files: []routingFile{{Path: path, Data: []byte("private"), Mode: 0600}}}
			switch name {
			case "symlink-file":
				if err := os.Symlink(path, filepath.Join(f.home, "link")); err != nil {
					t.Fatal(err)
				}
				plan.Files[0].Path = filepath.Join(f.home, "link")
			case "symlink-ancestor":
				if err := os.Symlink(f.home, filepath.Join(f.root, "alias")); err != nil {
					t.Fatal(err)
				}
				plan.Files[0].Path = filepath.Join(f.root, "alias", "config.json")
			case "hardlink-file":
				if err := os.Link(path, filepath.Join(f.home, "hardlink")); err != nil {
					t.Fatal(err)
				}
			case "wrong-kind":
				plan.Files[0].Path = f.home
			case "unclean":
				plan.Files[0].Path = f.home + "/./config.json"
			case "overlap":
				plan.Sockets = []string{path + "/child"}
			case "staging":
				plan.Files[0].Path = filepath.Join(f.stage, "config")
			case "staging-graft":
				plan.Files[0].Path = filepath.Join(f.root, "missing")
			case "endpoint-file":
				f.endpoint = "unix://" + path
			case "staging-permissions":
				if err := os.Chmod(f.stage, 0755); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			ops := fakeRoutingMountOps(func(string, string, string, uintptr, string) error { calls++; return nil })
			if err := applyRoutingPlanWithOps(plan, f.endpoint, f.stage, ops); err == nil {
				t.Fatal("unsafe projection accepted")
			}
			if calls != 0 {
				t.Fatalf("unsafe plan performed %d mount calls", calls)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != "host-secret" {
				t.Fatalf("host file modified: %q, %v", got, err)
			}
			entries, err := os.ReadDir(f.stage)
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging mutated before validation: %v, %v", entries, err)
			}
		})
	}
}
func TestRoutingMountFailureStopsBeforeAnyHostEntryCreation(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			f := newMountRoutingFixture(t)
			plan := routingPlan{Files: []routingFile{{Path: filepath.Join(f.home, "missing", "config"), Data: []byte("private"), Mode: 0600}}}
			calls := 0
			sentinel := errors.New("injected mount failure")
			ops := fakeRoutingMountOps(func(source, target, kind string, flags uintptr, data string) error {
				calls++
				if calls == 1 && (source != "" || target != "/" || flags != unix.MS_REC|unix.MS_PRIVATE) {
					t.Fatalf("first mount did not isolate propagation: %q %q %x", source, target, flags)
				}
				if calls == 2 && (source != "tmpfs" || !strings.HasPrefix(target, "/proc/self/fd/") || kind != "tmpfs" || data != "mode=0700") {
					t.Fatalf("staging mount not pinned private tmpfs: %q %q %q %q", source, target, kind, data)
				}
				if calls == failAt {
					return sentinel
				}
				return nil
			})
			err := applyRoutingPlanWithOps(plan, f.endpoint, f.stage, ops)
			if !errors.Is(err, sentinel) || calls != failAt {
				t.Fatalf("failure did not stop projection: %v calls=%d", err, calls)
			}
			if _, err = os.Lstat(filepath.Join(f.home, "missing")); !os.IsNotExist(err) {
				t.Fatalf("created host target: %v", err)
			}
		})
	}
}
func TestRoutingMountAllowsReadonlyAncestorsBeforeMutation(t *testing.T) {
	f := newMountRoutingFixture(t)
	nested := filepath.Join(f.home, "modules", "nested")
	if err := os.MkdirAll(nested, 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "module")
	if err := os.WriteFile(path, []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	plan := routingPlan{Files: []routingFile{{Path: path, Data: []byte("private"), Mode: 0600}}, ReadOnlyDirs: []string{filepath.Join(f.home, "modules"), nested, nested}}
	sentinel := errors.New("stop before mount")
	err := applyRoutingPlanWithOps(plan, f.endpoint, f.stage, fakeRoutingMountOps(func(string, string, string, uintptr, string) error { return sentinel }))
	if !errors.Is(err, sentinel) {
		t.Fatalf("readonly ancestors rejected: %v", err)
	}
}
func TestRoutingGraftPreservesDirectoryModeAndWritableSiblingBinds(t *testing.T) {
	f := newMountRoutingFixture(t)
	if err := os.Chmod(f.home, 0750); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(f.home, "auth.json")
	if err := os.WriteFile(auth, []byte("credentials"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.home, "work"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("auth.json", filepath.Join(f.home, "auth-link")); err != nil {
		t.Fatal(err)
	}
	original, err := pinRoutingPath(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer original.close()
	mounted := map[string]uintptr{}
	destination := filepath.Join(f.stage, "graft")
	ops := fakeRoutingMountOps(func(source, target, kind string, flags uintptr, data string) error {
		if !strings.HasPrefix(source, "/proc/self/fd/") {
			t.Errorf("unpinned source: %s", source)
		}
		if flags&unix.MS_RDONLY != 0 {
			t.Errorf("auth sibling made readonly: %x", flags)
		}
		mounted[filepath.Base(target)] = flags
		return nil
	})
	if err = mirrorRoutingDirectory(original, destination, ops); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatalf("directory mode lost: %v %v", info, err)
	}
	var st unix.Stat_t
	if err = unix.Stat(destination, &st); err != nil || st.Uid != original.stat.Uid || st.Gid != original.stat.Gid {
		t.Fatalf("directory owner lost: %+v %v", st, err)
	}
	if mounted["auth.json"] != unix.MS_BIND || mounted["work"] != unix.MS_BIND|unix.MS_REC {
		t.Fatalf("writable siblings not preserved: %v", mounted)
	}
	link, err := os.Readlink(filepath.Join(destination, "auth-link"))
	if err != nil || link != "auth.json" {
		t.Fatalf("sibling symlink changed: %q %v", link, err)
	}
	got, err := os.ReadFile(auth)
	if err != nil || string(got) != "credentials" {
		t.Fatalf("host auth changed: %q %v", got, err)
	}
}
func TestRoutingReadonlyTopologySnapshotsEmptySubdirectories(t *testing.T) {
	f := newMountRoutingFixture(t)
	nested := filepath.Join(f.home, "one", "two")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	original, err := pinRoutingPath(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer original.close()
	grafts := map[string]*routingPin{}
	defer func() {
		for _, pin := range grafts {
			pin.close()
		}
	}()
	budget := 100
	if err = collectRoutingDirectories(f.home, original, grafts, &budget, 0); err != nil {
		t.Fatal(err)
	}
	if len(grafts) != 3 || grafts[nested] == nil {
		t.Fatalf("mutable empty descendant omitted: %v", grafts)
	}
}

// Simulated successful mounts inspect their pinned sources without changing
// mounts, credentials, or any host configuration. The real target data must
// therefore remain unchanged throughout this deterministic orchestration test.
func TestRoutingMountProjectsPinnedSourcesThenSealsAndHidesAliases(t *testing.T) {
	f := newMountRoutingFixture(t)
	modules := filepath.Join(f.home, "modules")
	nested := filepath.Join(modules, "nested")
	empty := filepath.Join(f.home, "tls")
	for _, path := range []string{nested, empty} {
		if err := os.MkdirAll(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(nested, "module")
	if err := os.WriteFile(path, []byte("original-routing"), 0640); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.home, "alias.sock")
	listener, err := net.Listen("unix", alias)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	plan := routingPlan{Files: []routingFile{{Path: path, Data: []byte("selected-routing"), Mode: 0600}}, ReadOnlyDirs: []string{modules, nested}, EmptyDirs: []string{empty}, Sockets: []string{alias}}
	var events []string
	grafts := 0
	projected := false
	socketMounted := false
	ops := fakeRoutingMountOps(func(source, target, kind string, flags uintptr, data string) error {
		if strings.HasPrefix(source, "/proc/self/fd/") && strings.Contains(source, "/graft-") {
			grafts++
			if flags != unix.MS_BIND|unix.MS_REC {
				t.Errorf("graft omits original sibling child mounts: %x", flags)
			}
		}
		if strings.Contains(source, "/target-") {
			info, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().IsRegular() {
				content, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != "selected-routing" || info.Mode().Perm() != 0640 {
					t.Fatalf("projected source data/mode: %q %v", content, info.Mode())
				}
				projected = true
			}
		}
		if source != "" && strings.HasPrefix(source, "/proc/self/fd/") && !strings.Contains(source, "/graft-") && !strings.Contains(source, "/target-") {
			info, err := os.Stat(source)
			if err == nil && info.Mode()&os.ModeSocket != 0 {
				socketMounted = true
			}
		}
		if source != "tmpfs" && source != "" && !strings.HasPrefix(target, "/proc/self/fd/") && !strings.Contains(target, "/graft-") {
			t.Errorf("unpinned mount target: %s", target)
		}
		events = append(events, "mount")
		return nil
	})
	seals := 0
	ops.seal = func(fd int) error {
		if fd < 0 {
			t.Error("invalid seal fd")
		}
		seals++
		events = append(events, "seal")
		return nil
	}
	ops.unmount = func(target string, flags int) error {
		if !strings.HasPrefix(target, "/proc/self/fd/") || flags != unix.MNT_DETACH {
			t.Errorf("scratch aliases not detached through pinned mount: %q %x", target, flags)
		}
		events = append(events, "detach")
		return nil
	}
	if err := applyRoutingPlanWithOps(plan, f.endpoint, f.stage, ops); err != nil {
		t.Fatal(err)
	}
	if grafts < 3 || !projected || !socketMounted || seals != 4 {
		t.Fatalf("projection incomplete: grafts=%d file=%v socket=%v seals=%d", grafts, projected, socketMounted, seals)
	}
	if len(events) == 0 || events[len(events)-1] != "detach" {
		t.Fatalf("scratch aliases remain after projections: %v", events)
	}
	original, err := os.ReadFile(path)
	if err != nil || string(original) != "original-routing" {
		t.Fatalf("simulated mounts changed host: %q %v", original, err)
	}
}

func TestRoutingGraftAndProjectedFilePreservePOSIXACLs(t *testing.T) {
	f := newMountRoutingFixture(t)
	// Linux's POSIX ACL xattr: version, then (tag, permissions, identifier).
	acl := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(acl, 2)
	entries := []struct {
		tag, permissions uint16
		id               uint32
	}{
		{1, 7, ^uint32(0)}, {2, 5, uint32(os.Getuid() + 1)}, {4, 0, ^uint32(0)}, {16, 5, ^uint32(0)}, {32, 0, ^uint32(0)},
	}
	for i, e := range entries {
		offset := 4 + i*8
		binary.LittleEndian.PutUint16(acl[offset:], e.tag)
		binary.LittleEndian.PutUint16(acl[offset+2:], e.permissions)
		binary.LittleEndian.PutUint32(acl[offset+4:], e.id)
	}
	for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		err := unix.Setxattr(f.home, name, acl, 0)
		if errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip("filesystem does not support POSIX ACLs")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	original, err := pinRoutingPath(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer original.close()
	destination := filepath.Join(f.stage, "graft")
	if err = mirrorRoutingDirectory(original, destination, fakeRoutingMountOps(func(string, string, string, uintptr, string) error { return nil })); err != nil {
		t.Fatal(err)
	}
	assertACL := func(path, name string) {
		t.Helper()
		value := make([]byte, 4096)
		n, err := unix.Getxattr(path, name, value)
		if err != nil {
			t.Fatalf("read ACL %s at %s: %v", name, path, err)
		}
		if !bytes.Equal(value[:n], acl) {
			t.Fatalf("ACL %s lost at %s: %x", name, path, value[:n])
		}
	}
	assertACL(destination, "system.posix_acl_access")
	assertACL(destination, "system.posix_acl_default")
	sourceFile := filepath.Join(f.home, "config")
	privateFile := filepath.Join(f.stage, "private-config")
	for _, path := range []string{sourceFile, privateFile} {
		if err = os.WriteFile(path, []byte("config"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = unix.Setxattr(sourceFile, "system.posix_acl_access", acl, 0); err != nil {
		t.Fatal(err)
	}
	sourcePin, err := pinRoutingPath(sourceFile)
	if err != nil {
		t.Fatal(err)
	}
	defer sourcePin.close()
	if err = os.Chmod(privateFile, 0600); err != nil {
		t.Fatal(err)
	}
	if err = copyRoutingACL(sourcePin, privateFile, false); err != nil {
		t.Fatal(err)
	}
	assertACL(privateFile, "system.posix_acl_access")
}
