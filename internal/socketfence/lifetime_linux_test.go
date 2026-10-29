//go:build linux

package socketfence

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func fakeRetainedGroup(t *testing.T) (*Boundary, string) {
	t.Helper()
	path := t.TempDir()
	group, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { group.Close() })
	var st unix.Stat_t
	if err := unix.Fstat(int(group.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	return &Boundary{Group: group, State: State{Cgroup: path, CgroupID: st.Ino}}, path
}

func TestRetainedGroupEmptyRequiresExactProof(t *testing.T) {
	b, path := fakeRetainedGroup(t)
	for _, events := range []string{"", "frozen 0\n", "populated 0", "populated nope\n", "populated 2\n", "populated 0\npopulated 1\n", "populated 0\nbad\n", "populated 1\n", "populated 0\nfrozen 0\n"} {
		if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte(events), 0600); err != nil {
			t.Fatal(err)
		}
		empty, err := b.empty()
		if empty != (events == "populated 0\nfrozen 0\n") {
			t.Fatalf("events %q: empty=%v err=%v", events, empty, err)
		}
	}
	if err := os.Remove(filepath.Join(path, "cgroup.events")); err != nil {
		t.Fatal(err)
	}
	if empty, err := b.empty(); empty || err == nil {
		t.Fatal("missing events proved empty")
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b.State.CgroupID++
	if empty, err := b.empty(); empty || err == nil {
		t.Fatal("wrong descriptor identity proved empty")
	}
	b.State.CgroupID--
	if err := os.Rename(filepath.Join(path, "cgroup.events"), filepath.Join(path, "external-events")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("external-events", filepath.Join(path, "cgroup.events")); err != nil {
		t.Fatal(err)
	}
	if empty, err := b.empty(); empty || err == nil {
		t.Fatal("symlink event source proved empty")
	}
	b.Group.Close()
	if empty, err := b.empty(); empty || err == nil {
		t.Fatal("closed descriptor proved empty")
	}
}

func TestWaitEmptyRetainsOriginalSocketAcrossPathReplacement(t *testing.T) {
	b, path := fakeRetainedGroup(t)
	events := filepath.Join(path, "cgroup.events")
	if err := os.WriteFile(events, []byte("populated 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A replacement pathname must not prove that the original retained group is empty.
	moved := path + "-retained"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(moved) })
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(events, []byte("populated 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if empty, err := b.empty(); empty || err != nil {
		t.Fatalf("followed replacement group: %v %v", empty, err)
	}
	socket := filepath.Join(t.TempDir(), "auth")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	expected, checked, err := OpenSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	checked.Close()
	held, err := openEnrolledSocket(expected)
	if err != nil {
		t.Fatal(err)
	}
	b.sockets = []*os.File{held}
	listener.Close()
	done := make(chan struct{})
	go func() { b.WaitEmpty(); b.Close(); close(done) }()
	// Errors must retain ownership too, rather than release on an unreadable event.
	if err := os.Remove(filepath.Join(moved, "cgroup.events")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("missing events released references")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := held.Stat(); err != nil {
		t.Fatal("original socket inode released:", err)
	}
	if err := os.WriteFile(filepath.Join(moved, "cgroup.events"), []byte("populated 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("empty retained group did not release")
	}
	if _, err := held.Stat(); err == nil {
		t.Fatal("empty group retained socket handle")
	}
}
