//go:build linux

package network

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// routingMountOps keeps deterministic tests away from privileged mount calls.
// Production always uses the kernel implementation and root ownership policy.
type routingMountOps struct {
	mount   func(string, string, string, uintptr, string) error
	unmount func(string, int) error
	seal    func(int) error
	owner   uint32
	trusted func(string, unix.Stat_t) bool
}

func applyRoutingPlan(plan routingPlan, endpoint, staging string) error {
	return applyRoutingPlanWithOps(plan, endpoint, staging, routingMountOps{
		mount:   unix.Mount,
		unmount: unix.Unmount,
		seal: func(fd int) error {
			return unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH|unix.AT_RECURSIVE, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY})
		},
		trusted: func(_ string, st unix.Stat_t) bool { return st.Uid == 0 && st.Mode&0022 == 0 },
	})
}

type routingPin struct {
	file *os.File
	stat unix.Stat_t
}

func (p *routingPin) close()       { _ = p.file.Close() }
func (p *routingPin) proc() string { return "/proc/self/fd/" + strconv.Itoa(int(p.file.Fd())) }

// O_PATH plus O_NOFOLLOW at every component pins the actual mount objects. In
// particular, a caller cannot substitute a symlink while privileged code walks
// a HOME or XDG directory. An O_PATH descriptor for a symlink is rejected too.
func pinRoutingPath(path string) (*routingPin, error) {
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
		var st unix.Stat_t
		if e = unix.Fstat(fd, &st); e != nil {
			_ = unix.Close(fd)
			return nil, e
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("routing path contains symlink: %s", path)
		}
	}
	p := &routingPin{file: os.NewFile(uintptr(fd), path)}
	if err = unix.Fstat(fd, &p.stat); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func routingMountPath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("invalid routing path %q", path)
	}
	// /var/run is the one supported distribution alias. No arbitrary symlink
	// resolution is performed, including user-controlled config symlinks.
	if path == "/var/run" || strings.HasPrefix(path, "/var/run/") {
		link, err := os.Readlink("/var/run")
		if err == nil {
			if filepath.Clean(filepath.Join("/var", link)) != "/run" && link != "/run" {
				return "", errors.New("unsafe /var/run alias")
			}
			path = "/run" + strings.TrimPrefix(path, "/var/run")
		} else if !errors.Is(err, unix.EINVAL) {
			return "", err
		}
	}
	if path == "/" {
		return "", errors.New("routing cannot replace filesystem root")
	}
	return path, nil
}
func routingWithin(path, parent string) bool {
	return path == parent || strings.HasPrefix(path, parent+"/")
}
func routingSame(a, b unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino }

type routingTarget struct {
	path, kind string
	data       []byte
	mode       os.FileMode
	original   *routingPin
	parent     string
	owner      unix.Stat_t
}

func uniqueRoutingTargets(targets []routingTarget) ([]routingTarget, error) {
	var out []routingTarget
	seen := map[string]string{}
	for _, target := range targets {
		path, err := routingMountPath(target.path)
		if err != nil {
			return nil, err
		}
		if kind, exists := seen[path]; exists {
			if kind == target.kind && (kind == "socket" || kind == "readonly") {
				continue
			}
			return nil, fmt.Errorf("conflicting routing targets at %s", path)
		}
		target.path = path
		seen[path] = target.kind
		out = append(out, target)
	}
	return out, nil
}

func applyRoutingPlanWithOps(plan routingPlan, endpoint, staging string, ops routingMountOps) error {
	if ops.mount == nil || ops.unmount == nil || ops.seal == nil || ops.trusted == nil {
		return errors.New("missing routing mount operations")
	}
	stage, err := routingMountPath(staging)
	if err != nil {
		return err
	}
	stagePin, err := pinRoutingPath(stage)
	if err != nil {
		return err
	}
	defer stagePin.close()
	if stagePin.stat.Mode&unix.S_IFMT != unix.S_IFDIR || stagePin.stat.Uid != ops.owner || stagePin.stat.Mode&0077 != 0 {
		return errors.New("routing staging must be a private root-owned directory")
	}
	if !strings.HasPrefix(endpoint, "unix://") {
		return errors.New("routing endpoint must be a Unix socket")
	}
	socketPath, err := routingMountPath(strings.TrimPrefix(endpoint, "unix://"))
	if err != nil {
		return err
	}
	socket, err := pinRoutingPath(socketPath)
	if err != nil {
		return fmt.Errorf("pin routing proxy: %w", err)
	}
	defer socket.close()
	if socket.stat.Mode&unix.S_IFMT != unix.S_IFSOCK || socket.stat.Uid != ops.owner {
		return errors.New("routing endpoint must be a root-owned socket")
	}
	var targets []routingTarget
	for _, f := range plan.Files {
		targets = append(targets, routingTarget{path: f.Path, kind: "file", data: f.Data, mode: f.Mode})
	}
	for _, p := range plan.Sockets {
		targets = append(targets, routingTarget{path: p, kind: "socket"})
	}
	for _, p := range plan.EmptyDirs {
		targets = append(targets, routingTarget{path: p, kind: "empty"})
	}
	for _, p := range plan.ReadOnlyDirs {
		targets = append(targets, routingTarget{path: p, kind: "readonly"})
	}
	// Saved Docker endpoints often use /var/run while defaults use /run. These
	// aliases describe one socket projection, rather than conflicting mounts.
	targets, err = uniqueRoutingTargets(targets)
	if err != nil {
		return err
	}
	// Parent grafts detach directory entries from mutable host directories while
	// keeping each unrelated child backed by its original writable inode.
	grafts := map[string]*routingPin{}
	defer func() {
		for _, p := range grafts {
			p.close()
		}
		for _, t := range targets {
			if t.original != nil {
				t.original.close()
			}
		}
	}()
	for i := range targets {
		t := &targets[i]
		t.path, err = routingMountPath(t.path)
		if err != nil {
			return err
		}
		if routingWithin(t.path, stage) || routingWithin(stage, t.path) {
			return fmt.Errorf("routing target overlaps staging: %s", t.path)
		}
		for j := 0; j < i; j++ {
			other := targets[j]
			if (t.path == other.path && (t.kind != "readonly" || other.kind != "readonly")) || (routingWithin(t.path, other.path) && other.kind != "readonly") || (routingWithin(other.path, t.path) && t.kind != "readonly") {
				return fmt.Errorf("overlapping routing targets: %s and %s", t.path, other.path)
			}
		}
		t.original, err = pinRoutingPath(t.path)
		if err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("pin routing target %s: %w", t.path, err)
		}
		if t.original != nil {
			st := t.original.stat
			kind := st.Mode & unix.S_IFMT
			if (t.kind == "file" && (kind != unix.S_IFREG || st.Nlink != 1)) || (t.kind == "socket" && kind != unix.S_IFSOCK) || ((t.kind == "empty" || t.kind == "readonly") && kind != unix.S_IFDIR) {
				return fmt.Errorf("unsafe routing target type or hardlink: %s", t.path)
			}
			if routingSame(st, stagePin.stat) {
				return errors.New("routing target aliases staging")
			}
			t.owner = st
		}
		parent := filepath.Dir(t.path)
		var p *routingPin
		for {
			p, err = pinRoutingPath(parent)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.ENOENT) {
				return err
			}
			parent = filepath.Dir(parent)
		}
		if p.stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			p.close()
			return errors.New("routing parent is not a directory")
		}
		if t.original == nil {
			t.owner = p.stat
		}
		t.parent = parent
		if old := grafts[parent]; old != nil {
			p.close()
		} else {
			grafts[parent] = p
		}
		// Once a mutable ancestor is reached, each following directory needs its
		// own private entries; otherwise renaming a host ancestor bypasses a bind.
		current := "/"
		mutable := false
		for _, part := range strings.Split(strings.TrimPrefix(parent, "/"), "/") {
			if part == "" {
				continue
			}
			current = filepath.Join(current, part)
			p, err = pinRoutingPath(current)
			if err != nil {
				return err
			}
			mutable = mutable || !ops.trusted(current, p.stat)
			if mutable && grafts[current] == nil {
				grafts[current] = p
			} else {
				p.close()
			}
		}
	}
	// A read-only bind alone does not stop an outside process from adding
	// entries through its host view. Give every directory its own entries too.
	budget := 4096
	for _, t := range targets {
		if t.kind == "readonly" && t.original != nil {
			if err = collectRoutingDirectories(t.path, t.original, grafts, &budget, 0); err != nil {
				return err
			}
		}
	}
	var dirs []string
	stageAncestors := []unix.Stat_t{stagePin.stat}
	for parent := filepath.Dir(stage); parent != "/"; parent = filepath.Dir(parent) {
		p, e := pinRoutingPath(parent)
		if e != nil {
			return e
		}
		if !ops.trusted(parent, p.stat) {
			p.close()
			return fmt.Errorf("untrusted routing staging ancestor: %s", parent)
		}
		stageAncestors = append(stageAncestors, p.stat)
		p.close()
	}
	for path, p := range grafts {
		for _, ancestor := range stageAncestors {
			if routingSame(p.stat, ancestor) {
				return fmt.Errorf("routing graft aliases staging ancestor: %s", path)
			}
		}
		for other, pin := range grafts {
			if path != other && routingSame(p.stat, pin.stat) {
				return fmt.Errorf("routing graft paths alias: %s and %s", path, other)
			}
		}
		if path == "/" || routingWithin(stage, path) || routingSame(p.stat, stagePin.stat) {
			return fmt.Errorf("routing graft overlaps staging or root: %s", path)
		}
		dirs = append(dirs, path)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) < len(dirs[j]) || (len(dirs[i]) == len(dirs[j]) && dirs[i] < dirs[j])
	})
	// No operation before this point changes a mount or creates an entry.
	if err = ops.mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make invocation mounts private: %w", err)
	}
	if err = ops.mount("tmpfs", stagePin.proc(), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0700"); err != nil {
		return fmt.Errorf("mount routing staging: %w", err)
	}
	// Reopen after the mount: the original descriptor deliberately pins the
	// covered directory, not the newly mounted tmpfs.
	privateStage, err := pinRoutingPath(stage)
	if err != nil {
		return err
	}
	defer privateStage.close()
	base := privateStage.proc()
	for i, path := range dirs {
		source := filepath.Join(base, "graft-"+strconv.Itoa(i))
		if err = mirrorRoutingDirectory(grafts[path], source, ops); err != nil {
			return fmt.Errorf("prepare routing graft %s: %w", path, err)
		}
		target, err := pinRoutingPath(path)
		if err != nil {
			return err
		}
		err = ops.mount(source, target.proc(), "", unix.MS_BIND|unix.MS_REC, "")
		target.close()
		if err != nil {
			return fmt.Errorf("mount routing graft %s: %w", path, err)
		}
	}
	// Routing-only ancestors are sealed after their projected descendants.
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].kind == "readonly" && targets[j].kind == "readonly" {
			return len(targets[i].path) > len(targets[j].path)
		}
		return targets[i].kind != "readonly" && targets[j].kind == "readonly"
	})
	for i, t := range targets {
		// All missing entries are made in grafts, never in host-backed directories.
		if err = makeRoutingParents(t.path, t.parent, t.owner); err != nil {
			return err
		}
		if t.original == nil {
			if t.kind == "empty" || t.kind == "readonly" {
				err = os.Mkdir(t.path, 0755)
				if errors.Is(err, unix.EEXIST) {
					p, e := pinRoutingPath(t.path)
					if e != nil {
						return e
					}
					if p.stat.Mode&unix.S_IFMT != unix.S_IFDIR {
						p.close()
						return errors.New("private routing directory changed type")
					}
					p.close()
					err = nil
				}
			} else {
				var f *os.File
				f, err = os.OpenFile(t.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if f != nil {
					_ = f.Close()
				}
			}
			if err != nil {
				return fmt.Errorf("create private routing target: %w", err)
			}
			if err = os.Chown(t.path, int(t.owner.Uid), int(t.owner.Gid)); err != nil {
				return err
			}
		}
		target, err := pinRoutingPath(t.path)
		if err != nil {
			return err
		}
		source := filepath.Join(base, "target-"+strconv.Itoa(i))
		switch t.kind {
		case "file":
			mode := t.mode.Perm()
			if t.original != nil {
				mode = os.FileMode(t.owner.Mode & 0777)
			}
			if mode&0444 == 0 {
				target.close()
				return errors.New("routing configuration has no read permissions")
			}
			err = os.WriteFile(source, t.data, 0600)
			if err == nil {
				err = os.Chown(source, int(t.owner.Uid), int(t.owner.Gid))
			}
			if err == nil {
				err = os.Chmod(source, mode)
			}
			if err == nil && t.original != nil {
				err = copyRoutingACL(t.original, source, false)
			}
		case "socket":
			source = socket.proc()
		case "empty":
			err = os.Mkdir(source, 0700)
			if err == nil {
				err = os.Chown(source, int(t.owner.Uid), int(t.owner.Gid))
			}
			if err == nil {
				mode := uint32(0755)
				if t.original != nil {
					mode = t.owner.Mode & 07777
				}
				err = unix.Chmod(source, mode)
			}
			if err == nil && t.original != nil {
				err = copyRoutingACL(t.original, source, true)
			}
		case "readonly":
			source = target.proc()
		}
		if err == nil {
			flags := uintptr(unix.MS_BIND)
			if t.kind == "readonly" {
				flags |= unix.MS_REC
			}
			err = ops.mount(source, target.proc(), "", flags, "")
		}
		target.close()
		if err != nil {
			return fmt.Errorf("project routing target %s: %w", t.path, err)
		}
		if t.kind != "socket" {
			sealed, err := pinRoutingPath(t.path)
			if err != nil {
				return err
			}
			err = ops.seal(int(sealed.file.Fd()))
			sealed.close()
			if err != nil {
				return fmt.Errorf("seal routing target %s: %w", t.path, err)
			}
		}
	}
	// Bind mounts retain their sources after the private scratch mount is
	// detached. Remove scratch aliases to original routing entries before exec,
	// including for a uid-zero caller who could otherwise read this directory.
	if err = ops.unmount(privateStage.proc(), unix.MNT_DETACH); err != nil {
		return fmt.Errorf("hide routing staging aliases: %w", err)
	}
	return nil
}

func makeRoutingParents(path, parent string, owner unix.Stat_t) error {
	relative, err := filepath.Rel(parent, filepath.Dir(path))
	if err != nil {
		return err
	}
	current := parent
	for _, part := range strings.Split(relative, "/") {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		p, e := pinRoutingPath(current)
		if e == nil {
			p.close()
			continue
		}
		if !errors.Is(e, unix.ENOENT) {
			return e
		}
		if e = os.Mkdir(current, 0755); e != nil {
			return e
		}
		if e = os.Chown(current, int(owner.Uid), int(owner.Gid)); e != nil {
			return e
		}
	}
	return nil
}

func mirrorRoutingDirectory(original *routingPin, path string, ops routingMountOps) error {
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	fd, err := unix.Openat(int(original.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), path)
	defer directory.Close()
	entries, err := directory.ReadDir(maxRoutingEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > maxRoutingEntries {
		return errors.New("routing graft directory entry limit exceeded")
	}
	for _, entry := range entries {
		childFD, e := unix.Openat(fd, entry.Name(), unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return e
		}
		child := &routingPin{file: os.NewFile(uintptr(childFD), entry.Name())}
		e = unix.Fstat(childFD, &child.stat)
		destination := filepath.Join(path, entry.Name())
		if e == nil {
			switch child.stat.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				e = os.Mkdir(destination, 0700)
			case unix.S_IFLNK:
				buffer := make([]byte, 4096)
				var n int
				n, e = unix.Readlinkat(childFD, "", buffer)
				if e == nil && n == len(buffer) {
					e = errors.New("oversized routing sibling symlink")
				}
				if e == nil {
					e = os.Symlink(string(buffer[:n]), destination)
				}
				if e == nil {
					e = os.Lchown(destination, int(child.stat.Uid), int(child.stat.Gid))
				}
			default:
				var f *os.File
				f, e = os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if f != nil {
					_ = f.Close()
				}
			}
		}
		if e == nil && child.stat.Mode&unix.S_IFMT != unix.S_IFLNK {
			flags := uintptr(unix.MS_BIND)
			if child.stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				flags |= unix.MS_REC
			}
			e = ops.mount(child.proc(), destination, "", flags, "")
		}
		child.close()
		if e != nil {
			return e
		}
	}
	if err = os.Chown(path, int(original.stat.Uid), int(original.stat.Gid)); err != nil {
		return err
	}
	if err = unix.Chmod(path, original.stat.Mode&07777); err != nil {
		return err
	}
	return copyRoutingACL(original, path, true)
}

// Snapshot the directory topology with openat against pinned parent objects.
// Files remain writable bind sources until the planner's files replace them;
// the caller must verify the final routing inventory before starting workload.
func collectRoutingDirectories(path string, original *routingPin, grafts map[string]*routingPin, budget *int, depth int) error {
	if depth > 32 || *budget <= 0 {
		return errors.New("routing directory snapshot limit exceeded")
	}
	*budget--
	if grafts[path] == nil {
		fd, err := unix.Dup(int(original.file.Fd()))
		if err != nil {
			return err
		}
		unix.CloseOnExec(fd)
		grafts[path] = &routingPin{file: os.NewFile(uintptr(fd), path), stat: original.stat}
	}
	fd, err := unix.Openat(int(original.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), path)
	defer directory.Close()
	entries, err := directory.ReadDir(*budget + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > *budget {
		return errors.New("routing directory snapshot entry limit exceeded")
	}
	for _, entry := range entries {
		*budget--
		childFD, e := unix.Openat(fd, entry.Name(), unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return e
		}
		child := &routingPin{file: os.NewFile(uintptr(childFD), entry.Name())}
		e = unix.Fstat(childFD, &child.stat)
		if e == nil {
			switch child.stat.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				e = collectRoutingDirectories(filepath.Join(path, entry.Name()), child, grafts, budget, depth+1)
			case unix.S_IFREG:
			default:
				e = fmt.Errorf("unsafe entry in routing-only directory: %s", filepath.Join(path, entry.Name()))
			}
		}
		child.close()
		if e != nil {
			return e
		}
	}
	return nil
}

// ACL entries can be the caller's only access to a root-owned config directory.
// Copy them after chown/chmod, which otherwise adjust or clear ACL masks.
func copyRoutingACL(original *routingPin, destination string, directory bool) error {
	names := []string{"system.posix_acl_access"}
	if directory {
		names = append(names, "system.posix_acl_default")
	}
	for _, name := range names {
		size, err := unix.Getxattr(original.proc(), name, nil)
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read routing ACL: %w", err)
		}
		if size > 65536 {
			return errors.New("oversized routing ACL")
		}
		value := make([]byte, size)
		n, err := unix.Getxattr(original.proc(), name, value)
		if err != nil {
			return fmt.Errorf("read routing ACL: %w", err)
		}
		if err = unix.Setxattr(destination, name, value[:n], 0); err != nil {
			return fmt.Errorf("preserve routing ACL: %w", err)
		}
	}
	return nil
}
