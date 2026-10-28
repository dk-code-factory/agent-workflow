//go:build linux

// Package socketfence installs the operator-enabled Unix API boundary. Pins are
// intentionally independent of the service lifetime; Close never detaches it.
package socketfence

//go:generate python3 ../../scripts/generate-socket-fence.py

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

const PinRoot = "/sys/fs/bpf/agent-workflow"
const CgroupRoot = "/sys/fs/cgroup/agent-workflow"
const Capability = "bpf-lsm-unix-v1"

type Socket struct {
	Path   string `json:"path"`
	Inode  uint64 `json:"inode"`
	Device uint32 `json:"device"`
}
type State struct {
	Identity   string            `json:"identity"`
	Generation uint64            `json:"generation"`
	Cgroup     string            `json:"cgroup"`
	CgroupID   uint64            `json:"cgroup_id"`
	Pins       string            `json:"pins"`
	Programs   map[string]uint32 `json:"programs"`
	Maps       map[string]uint32 `json:"maps"`
	Sockets    []Socket          `json:"sockets"`
}
type Boundary struct {
	State   State
	Group   *os.File
	sockets []*os.File
}

func Supported() error {
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return fmt.Errorf("BPF LSM unavailable: %w", err)
	}
	found := false
	for _, name := range strings.Split(strings.TrimSpace(string(data)), ",") {
		if name == "bpf" {
			found = true
		}
	}
	if !found {
		return errors.New("BPF LSM is not active; operator must enable CONFIG_BPF_LSM and lsm=...,bpf")
	}
	for path, magic := range map[string]int64{"/sys/fs/bpf": unix.BPF_FS_MAGIC, "/sys/fs/cgroup": unix.CGROUP2_SUPER_MAGIC} {
		var st unix.Statfs_t
		if err := unix.Statfs(path, &st); err != nil {
			return err
		}
		if int64(st.Type) != magic {
			return fmt.Errorf("%s must have its kernel filesystem mounted", path)
		}
	}
	return nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	for p := path; p != "/"; p = filepath.Dir(p) {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0022 != 0 {
			return fmt.Errorf("untrusted policy directory %s", p)
		}
	}
	return nil
}

func OpenSocket(path string) (Socket, *os.File, error) {
	if !filepath.IsAbs(path) {
		return Socket{}, nil, errors.New("authentication socket must be absolute")
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return Socket{}, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		f.Close()
		return Socket{}, nil, fmt.Errorf("enrolled path is not a Unix socket: %s (%v)", path, err)
	}
	// Kernel dev_t uses 12 major / 20 minor bits; userspace stat uses new_encode_dev.
	dev := uint32(unix.Major(st.Dev)<<20 | unix.Minor(st.Dev))
	return Socket{Path: path, Inode: st.Ino, Device: dev}, f, nil
}

// Install runs only at explicit operator start, before launch admission opens.
func Install(identity string, generation uint64, paths []string) (*Boundary, error) {
	if err := Supported(); err != nil {
		return nil, err
	}
	if len(identity) != 32 || strings.Trim(identity, "0123456789abcdef") != "" || generation == 0 {
		return nil, errors.New("invalid enforcement generation")
	}
	state := State{Identity: identity, Generation: generation, Cgroup: filepath.Join(CgroupRoot, identity), Pins: filepath.Join(PinRoot, identity), Programs: map[string]uint32{}, Maps: map[string]uint32{}}
	if err := privateDir(state.Cgroup); err != nil {
		return nil, err
	}
	if err := privateDir(state.Pins); err != nil {
		return nil, err
	}
	b := &Boundary{State: state}
	var err error
	b.Group, err = os.Open(state.Cgroup)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = b.Retire()
			b.Close()
		}
	}()
	var st unix.Stat_t
	if err = unix.Fstat(int(b.Group.Fd()), &st); err != nil {
		return nil, err
	}
	b.State.CgroupID = st.Ino
	var objects fenceObjects
	if err = loadFenceObjects(&objects, nil); err != nil {
		return nil, fmt.Errorf("load socket BPF LSM: %w", err)
	}
	defer objects.Close()
	for _, path := range paths {
		socket, f, e := OpenSocket(path)
		if e != nil {
			return nil, e
		}
		b.sockets = append(b.sockets, f)
		b.State.Sockets = append(b.State.Sockets, socket)
		key := fenceSocketId{Inode: socket.Inode, Device: socket.Device}
		if err = objects.Allowed.Update(key, uint32(1), ebpf.UpdateAny); err != nil {
			return nil, err
		}
	}
	if err = os.WriteFile(filepath.Join(state.Cgroup, "cgroup.max.depth"), []byte("0"), 0600); err != nil {
		return nil, err
	}
	cfg := fenceConfig{CgroupId: st.Ino, Generation: generation, Ready: 1}
	identityBytes, _ := hex.DecodeString(identity)
	copy(cfg.Identity[:], identityBytes)
	if err = objects.Policy.Update(uint32(0), cfg, ebpf.UpdateAny); err != nil {
		return nil, err
	}
	for name, m := range map[string]*ebpf.Map{"policy": objects.Policy, "allowed": objects.Allowed} {
		if err = m.Freeze(); err != nil {
			return nil, err
		}
		if err = m.Pin(filepath.Join(state.Pins, name)); err != nil {
			return nil, err
		}
		info, e := m.Info()
		if e != nil {
			return nil, e
		}
		id, ok := info.ID()
		if !ok {
			return nil, errors.New("missing map identity")
		}
		b.State.Maps[name] = uint32(id)
	}
	for name, prog := range map[string]*ebpf.Program{"stream": objects.FenceStream, "datagram": objects.FenceDatagram} {
		lnk, e := link.AttachLSM(link.LSMOptions{Program: prog})
		if e != nil {
			return nil, fmt.Errorf("attach %s socket LSM: %w", name, e)
		}
		e = lnk.Pin(filepath.Join(state.Pins, name))
		info, ie := lnk.Info()
		lnk.Close() // A successful pin retains the policy through crashes.
		if e != nil || ie != nil {
			return nil, errors.Join(e, ie)
		}
		b.State.Programs[name] = uint32(info.Program)
	}
	if err = b.Check(); err != nil {
		return nil, err
	}
	failed = false
	return b, nil
}

// Restore verifies the exact retained kernel objects, never a replacement.
func Restore(state State) (*Boundary, error) {
	if state.Cgroup != filepath.Join(CgroupRoot, state.Identity) || state.Pins != filepath.Join(PinRoot, state.Identity) || len(state.Identity) != 32 || strings.Trim(state.Identity, "0123456789abcdef") != "" || state.Generation == 0 {
		return nil, errors.New("invalid retained socket policy")
	}
	b := &Boundary{State: state}
	var err error
	b.Group, err = os.Open(state.Cgroup)
	if err != nil {
		return nil, err
	}
	for _, expected := range state.Sockets {
		actual, f, e := OpenSocket(expected.Path)
		if e != nil || actual != expected {
			if f != nil {
				f.Close()
			}
			b.Close()
			return nil, errors.New("enrolled socket identity changed")
		}
		b.sockets = append(b.sockets, f)
	}
	if err = b.Check(); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *Boundary) Check() error {
	if b == nil || b.Group == nil {
		return errors.New("socket enforcement missing")
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(b.Group.Fd()), &st); err != nil {
		return err
	}
	if st.Ino != b.State.CgroupID {
		return errors.New("socket enforcement cgroup changed")
	}
	for _, name := range []string{"policy", "allowed"} {
		m, err := ebpf.LoadPinnedMap(filepath.Join(b.State.Pins, name), nil)
		if err != nil {
			return err
		}
		info, e := m.Info()
		if e == nil && name == "policy" {
			var cfg fenceConfig
			e = m.Lookup(uint32(0), &cfg)
			if e == nil && (cfg.CgroupId != b.State.CgroupID || cfg.Generation != b.State.Generation || hex.EncodeToString(cfg.Identity[:]) != b.State.Identity || cfg.Ready != 1) {
				e = errors.New("socket policy identity/generation changed")
			}
		}
		m.Close()
		if e != nil {
			return e
		}
		id, ok := info.ID()
		if !ok || uint32(id) != b.State.Maps[name] || !info.Frozen() {
			return errors.New("socket enforcement map changed")
		}
	}
	for _, name := range []string{"stream", "datagram"} {
		lnk, err := link.LoadPinnedLink(filepath.Join(b.State.Pins, name), nil)
		if err != nil {
			return err
		}
		info, e := lnk.Info()
		lnk.Close()
		if e != nil {
			return e
		}
		if uint32(info.Program) != b.State.Programs[name] {
			return errors.New("socket enforcement link changed")
		}
	}
	for i, expected := range b.State.Sockets {
		actual, f, err := OpenSocket(expected.Path)
		if f != nil {
			f.Close()
		}
		if err != nil || actual != expected || i >= len(b.sockets) {
			return errors.New("enrolled socket changed")
		}
	}
	return nil
}

func (b *Boundary) Close() {
	if b.Group != nil {
		b.Group.Close()
	}
	for _, f := range b.sockets {
		f.Close()
	}
}

// Retire removes only this generation after kernel proof that its subtree is
// empty. Failure leaves the pins and descriptor available for operator audit.
func (b *Boundary) Retire() error {
	data, err := os.ReadFile(filepath.Join(b.State.Cgroup, "cgroup.events"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "populated 0\n") {
		return errors.New("socket policy still populated; retaining enforcement")
	}
	// Remove the cgroup before detaching so concurrent clone3 into its fd fails.
	if err = os.Remove(b.State.Cgroup); err != nil {
		return err
	}
	for _, name := range []string{"stream", "datagram", "policy", "allowed"} {
		if err = os.Remove(filepath.Join(b.State.Pins, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Remove(b.State.Pins)
}
