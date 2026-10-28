//go:build linux

package network

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type registration struct {
	Name       string
	Users      map[uint32]bool
	Generation uint64
}
type memberLaunch struct {
	Request         Request
	UID, GID        uint32
	Groups          []uint32
	Keeper          int
	Binary, Receipt string
}
type service struct {
	netNS, mountNS   *os.File
	probe            func() error
	memberCommand    func(string) *exec.Cmd
	mu               sync.Mutex
	status           Status
	config           registration
	dir, run, binary string
	keeper           *exec.Cmd
	stopping         bool
	stop             chan struct{}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func save(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".partial", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(path+".partial", path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func load(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
func rootOnly(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("prepare/start require explicit root operator action")
	}
	return nil
}
func trustedDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return errors.New("runtime directory must be root owned and not group/world writable")
	}
	return nil
}

// Prepare enrolls only an existing rootful Podman authority. Starting a rootful
// API is equivalent to unrestricted service-user execution; narrow Podman read
// permissions are insufficient. No login or privilege refresh is performed.
func Prepare(name string, uid uint32) error {
	if err := rootOnly(name); err != nil {
		return err
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return err
	}
	if uid != 0 {
		check := exec.Command("/usr/bin/sudo", "-n", "-l", "-U", u.Username, "--", "/usr/bin/podman", "system", "service", "--time=0")
		check.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
		if err = check.Run(); err != nil {
			return errors.New("enrollment requires existing sudo authority to start the rootful Podman API")
		}
	}
	for _, path := range []string{Registry, RunRoot} {
		if err = os.MkdirAll(path, 0755); err != nil {
			return err
		}
		if err = trustedDir(path); err != nil {
			return err
		}
	}
	dir := filepath.Join(Registry, name)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err = trustedDir(dir); err != nil {
		return err
	}
	lock, err := lockRuntime(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	var cfg registration
	err = load(filepath.Join(dir, "registration.json"), &cfg)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	cfg.Name = name
	if cfg.Users == nil {
		cfg.Users = map[uint32]bool{}
	}
	cfg.Users[uid] = true
	// Install a trusted executable explicitly. Runtime launch never executes a
	// caller-owned binary as root. No setuid permission or sudo rule is installed.
	source, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	temp := filepath.Join(dir, "launcher.partial")
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	err = errors.Join(err, out.Sync(), out.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(temp, filepath.Join(dir, "launcher")); err != nil {
		return err
	}
	return save(filepath.Join(dir, "registration.json"), cfg)
}
func lockRuntime(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("network runtime already owned (stop before preparation/restart)")
	}
	return f, nil
}
func Start(name string) error {
	if err := rootOnly(name); err != nil {
		return err
	}
	dir := filepath.Join(Registry, name)
	if err := trustedDir(dir); err != nil {
		return err
	}
	// Transfer the advisory lock as an open file description. A clean stop may
	// still be draining its owned children when its RPC acknowledgement arrives.
	// Wait for that owner, then retain one continuous lock through daemon launch.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	current, e := Call(ctx, name, Request{Operation: "status"})
	cancel()
	if e == nil && current.Status.Healthy {
		return nil
	}
	var owner *os.File
	deadline := time.Now().Add(10 * time.Second)
	for {
		owner, e = lockRuntime(dir)
		if e == nil {
			break
		}
		if time.Now().After(deadline) {
			return e
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer owner.Close()
	// The daemon owns its lock; this process only requests an explicit start.
	cmd := exec.Command(filepath.Join(dir, "launcher"), "__network_runtime_daemon_v1", name)
	cmd.ExtraFiles = []*os.File{owner}
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	log, err := os.OpenFile(filepath.Join(dir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		select {
		case err = <-done:
			return fmt.Errorf("runtime start failed (%v); inspect %s", err, log.Name())
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err = Resolve(ctx, name)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("runtime readiness unconfirmed; inspect %s", log.Name())
}
func init() {
	if len(os.Args) == 2 && os.Args[1] == "__network_runtime_client_v1" {
		data := os.Getenv(launchEnvironment)
		_ = os.Unsetenv(launchEnvironment)
		if err := runClient(data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(125)
		}
		os.Exit(0)
	}
	if len(os.Args) < 3 {
		return
	}
	var err error
	switch os.Args[1] {
	case "__network_runtime_configure_v1":
		err = configureNamespace(os.Args[2])
	case "__network_runtime_probe_v1":
		err = probeNamespace(os.Args[2])
	case "__network_runtime_workload_v1":
		err = workload()
	case "__network_runtime_daemon_v1":
		err = serve(os.Args[2])
	case "__network_runtime_keeper_v1":
		err = keeper(os.Args[2])
	case "__network_runtime_member_v1":
		err = runMember(os.Args[2])
	default:
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	os.Exit(0)
}
func keeper(run string) error {
	if os.Geteuid() != 0 {
		return errors.New("namespace keeper requires root")
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"networks", "/run/containers/networks"}, {"netconfig", "/etc/containers/networks"}, {"netns", "/run/netns"}} {
		if err := os.MkdirAll(pair[1], 0755); err != nil {
			return err
		}
		if err := unix.Mount(filepath.Join(run, pair[0]), pair[1], "", unix.MS_BIND, ""); err != nil {
			return err
		}
	}
	// Resolve host symlinks (systemd-resolved) only for the mount target. The
	// private resolver is never written to the host's configuration file.
	resolver, err := filepath.EvalSymlinks("/etc/resolv.conf")
	if err != nil {
		return err
	}
	if err = unix.Mount(filepath.Join(run, "resolv.conf"), resolver, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if err = exec.Command("/usr/sbin/ip", "link", "set", "lo", "up").Run(); err != nil {
		return err
	}
	api := exec.Command("/usr/bin/podman", "--remote=false", "system", "service", "--time=0", "unix://"+filepath.Join(run, "api.sock"))
	api.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	api.Stdout = os.Stdout
	api.Stderr = os.Stderr
	if err = api.Start(); err != nil {
		return err
	}
	defer func() { _ = api.Process.Kill(); _ = api.Wait() }()
	ready := os.NewFile(4, "ready")
	if _, err = ready.Write([]byte{1}); err != nil {
		return err
	}
	ready.Close()
	lifetime := os.NewFile(3, "lifetime")
	_, err = io.Copy(io.Discard, lifetime)
	return err
}
func startChild(cmd *exec.Cmd) (func(), error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, r)
	if err = cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	r.Close()
	return func() { w.Close(); _ = cmd.Wait() }, nil
}
func waitReady(f *os.File) error {
	defer f.Close()
	_ = f.SetReadDeadline(time.Now().Add(10 * time.Second))
	var b [1]byte
	_, err := io.ReadFull(f, b[:])
	return err
}
func serve(name string) error {
	if err := rootOnly(name); err != nil {
		return err
	}
	dir := filepath.Join(Registry, name)
	if err := trustedDir(dir); err != nil {
		return err
	}
	lock := os.NewFile(3, "runtime-owner")
	expected, err := os.Stat(filepath.Join(dir, "owner.lock"))
	if err != nil {
		return err
	}
	actual, err := lock.Stat()
	if err != nil || !os.SameFile(expected, actual) {
		return errors.New("invalid runtime owner lock")
	}
	if err = unix.Flock(3, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	unix.CloseOnExec(3)
	defer lock.Close()
	var cfg registration
	if err = load(filepath.Join(dir, "registration.json"), &cfg); err != nil {
		return err
	}
	var prior Status
	if err = load(filepath.Join(dir, "status.json"), &prior); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Durable root-owned member receipts survive daemon death. Unknown launches
	// stay busy; an operator must reconcile them, never signal a retained PID.
	for id, m := range prior.Members {
		if m.State != "cleaned" {
			var receipt Membership
			if load(filepath.Join(dir, "member-"+id+".done"), &receipt) != nil || receipt.State != "cleaned" {
				return fmt.Errorf("member %s has unconfirmed cleanup; restart refused", id)
			}
		}
	}
	cfg.Generation++
	if err = save(filepath.Join(dir, "registration.json"), cfg); err != nil {
		return err
	}
	runParent := filepath.Join(RunRoot, name)
	if err = os.MkdirAll(runParent, 0755); err != nil {
		return err
	}
	if err = trustedDir(runParent); err != nil {
		return err
	}
	identity := randomID()
	run := filepath.Join(runParent, identity)
	if err = os.MkdirAll(run, 0755); err != nil {
		return err
	}
	if err = trustedDir(run); err != nil {
		return err
	}
	for _, part := range []string{"networks", "netconfig", "netns"} {
		if err = os.MkdirAll(filepath.Join(run, part), 0700); err != nil {
			return err
		}
	}
	if err = os.WriteFile(filepath.Join(run, "resolv.conf"), []byte("nameserver 10.0.2.3\noptions timeout:2 attempts:2\n"), 0644); err != nil {
		return err
	}
	// Only these service-owned sockets are replaced while holding the root lock.
	for _, part := range []string{"control.sock"} {
		if err = os.Remove(filepath.Join(runParent, part)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	binary := filepath.Join(dir, "launcher")
	kr, kw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer kw.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return err
	}
	keep := exec.Command(binary, "__network_runtime_keeper_v1", run)
	keep.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	keep.ExtraFiles = []*os.File{kr, readyW}
	keep.Stdout = os.Stdout
	keep.Stderr = os.Stderr
	keep.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNET | unix.CLONE_NEWNS}
	if err = keep.Start(); err != nil {
		return err
	}
	kr.Close()
	readyW.Close()
	defer func() { kw.Close(); _ = keep.Wait() }()
	if err = waitReady(readyR); err != nil {
		return fmt.Errorf("namespace initialization failed: %w", err)
	}
	netNS, err := os.Open(fmt.Sprintf("/proc/%d/ns/net", keep.Process.Pid))
	if err != nil {
		return err
	}
	defer netNS.Close()
	mountNS, err := os.Open(fmt.Sprintf("/proc/%d/ns/mnt", keep.Process.Pid))
	if err != nil {
		return err
	}
	defer mountNS.Close()

	sr, sw, err := os.Pipe()
	if err != nil {
		return err
	}
	slirp := exec.Command("/usr/bin/slirp4netns", "--configure", "--disable-host-loopback", "--ready-fd=3", "--exit-fd=4", "--api-socket="+filepath.Join(run, "slirp.sock"), "--netns-type=path", fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), netNS.Fd()), "tap0")
	slirp.ExtraFiles = []*os.File{sw}
	slirp.Stdout = os.Stdout
	slirp.Stderr = os.Stderr
	stopSlirp, err := startChild(slirp)
	sw.Close()
	if err != nil {
		return err
	}
	defer stopSlirp()
	if err = waitReady(sr); err != nil {
		return fmt.Errorf("outbound networking initialization failed: %w", err)
	}
	s := &service{netNS: netNS, mountNS: mountNS, config: cfg, dir: dir, run: run, binary: binary, keeper: keep, stop: make(chan struct{}), status: Status{Reference: Reference{Name: name, Identity: identity, Generation: cfg.Generation, Endpoint: Endpoint(name, identity)}, Members: map[string]Membership{}, Forwards: prior.Forwards}}
	configure := exec.Command("/usr/bin/nsenter", "--net=/proc/self/fd/3", "--mount=/proc/self/fd/4", "--", binary, "__network_runtime_configure_v1", run)
	configure.ExtraFiles = []*os.File{netNS, mountNS}
	configure.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	if err = configure.Run(); err != nil {
		return fmt.Errorf("runtime loopback forwarding initialization failed: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err = s.health(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	for i := range s.status.Forwards {
		f := &s.status.Forwards[i]
		id, err := s.addForward(*f)
		if err != nil {
			return fmt.Errorf("forwarding recovery failed: %w", err)
		}
		f.ID = id
	}
	s.status.Healthy = true
	if err = save(filepath.Join(dir, "status.json"), s.status); err != nil {
		return err
	}
	control, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: filepath.Join(runParent, "control.sock"), Net: "unixpacket"})
	if err != nil {
		return err
	}
	defer control.Close()
	api, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(run, "podman.sock"), Net: "unix"})
	if err != nil {
		return err
	}
	defer api.Close()
	for _, socket := range []string{filepath.Join(runParent, "control.sock"), filepath.Join(run, "podman.sock")} {
		if err = os.Chmod(socket, 0666); err != nil {
			return err
		}
	}
	go s.proxyAPI(api)
	go func() {
		for {
			c, err := control.AcceptUnix()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return nil
		case <-signals:
			s.mu.Lock()
			s.stopping = true
			s.status.Healthy = false
			_ = save(filepath.Join(dir, "status.json"), s.status)
			s.mu.Unlock()
			return nil
		case <-ticker.C:
			s.mu.Lock()
			if err = s.health(); err != nil {
				s.status.Healthy = false
			}
			s.reconcile()
			_ = save(filepath.Join(dir, "status.json"), s.status)
			s.mu.Unlock()
		}
	}
}
func (s *service) health() error {
	if s.probe != nil {
		return s.probe()
	}
	// A failed component latches unhealthy for this generation. DNS and outbound
	// probes check private mounts, interface state and the slirp DNS resolver.
	// DNS uses an RFC-reserved .invalid name; no model endpoint is contacted.
	for name, handle := range map[string]*os.File{"net": s.netNS, "mnt": s.mountNS} {
		expected, e := handle.Stat()
		if e != nil {
			return e
		}
		actual, e := os.Stat(fmt.Sprintf("/proc/%d/ns/%s", s.keeper.Process.Pid, name))
		if e != nil || !os.SameFile(expected, actual) {
			return errors.New("runtime namespace identity changed")
		}
		a, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", s.keeper.Process.Pid, name))
		if err != nil {
			return errors.New("runtime namespace lost")
		}
		b, _ := os.Readlink("/proc/self/ns/" + name)
		if a == b {
			return errors.New("runtime namespace is not isolated")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/nsenter", "--net=/proc/self/fd/3", "--mount=/proc/self/fd/4", "--", s.binary, "__network_runtime_probe_v1", s.run)
	cmd.ExtraFiles = []*os.File{s.netNS, s.mountNS}
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	if err := cmd.Run(); err != nil {
		return errors.New("runtime namespace/resolver/DNS probe failed")
	}

	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(s.run, "api.sock"))
	}}
	client := &http.Client{Transport: transport}
	request, _ := http.NewRequestWithContext(ctx, "GET", "http://podman/_ping", nil)
	response, err := client.Do(request)
	if err != nil {
		return errors.New("runtime Podman API unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("runtime Podman API unhealthy")
	}
	_, err = s.slirp(map[string]any{"execute": "list_hostfwd"})
	return err
}
func trustedConnection(c *net.UnixConn) error {
	cred, err := peer(c)
	if err != nil {
		return err
	}
	if cred.Uid != 0 {
		return errors.New("network runtime service must be root owned")
	}
	return nil
}
func peer(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var e error
	err = raw.Control(func(fd uintptr) { cred, e = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	return cred, errors.Join(err, e)
}
func (s *service) allowed(c *net.UnixConn) (*unix.Ucred, error) {
	cred, err := peer(c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	enrolled := s.config.Users[cred.Uid]
	s.mu.Unlock()
	if cred.Uid != 0 && !enrolled {
		return nil, errors.New("caller is not enrolled")
	}
	return cred, nil
}
func (s *service) proxyAPI(listener *net.UnixListener) {
	for {
		c, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			if _, err := s.allowed(c); err != nil {
				return
			}
			s.mu.Lock()
			healthy := s.status.Healthy && !s.stopping
			s.mu.Unlock()
			if !healthy {
				s.proxyCleanup(c)
				return
			}
			upstream, err := net.DialTimeout("unix", filepath.Join(s.run, "api.sock"), 2*time.Second)
			if err != nil {
				return
			}
			defer upstream.Close()
			done := make(chan struct{})
			go func() {
				_, _ = io.Copy(upstream, c)
				if u, ok := upstream.(*net.UnixConn); ok {
					_ = u.CloseWrite()
				}
				close(done)
			}()
			_, _ = io.Copy(c, upstream)
			c.Close()
			<-done
		}()
	}
}

// Unhealthy generations retain only the HTTP operations used by exact owned
// reconciliation. Parse every request: a keepalive connection is not permission
// to create, start, exec, or otherwise admit new work after a successful read.
var cleanupVersion = regexp.MustCompile(`^/v[0-9]+(?:\.[0-9]+)*/(?:libpod/)?`)

func cleanupRoute(method, path string) bool {
	path = cleanupVersion.ReplaceAllString(path, "/")
	if method == http.MethodGet && (path == "/_ping" || path == "/info" || path == "/containers/json") {
		return true
	}
	if !strings.HasPrefix(path, "/containers/") {
		return false
	}
	object := strings.TrimPrefix(path, "/containers/")
	if method == http.MethodGet && strings.HasSuffix(object, "/json") {
		name := strings.TrimSuffix(object, "/json")
		return name != "" && !strings.ContainsAny(name, "/%") && name != "." && name != ".."
	}
	return method == http.MethodDelete && len(object) == 64 && hexID(object)
}

func (s *service) proxyCleanup(c *net.UnixConn) {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(s.run, "api.sock"))
	}}
	defer transport.CloseIdleConnections()
	reader := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		if req.Header.Get("Upgrade") != "" || strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade") || req.URL.RawPath != "" || !cleanupRoute(req.Method, req.URL.Path) || req.ContentLength != 0 || len(req.TransferEncoding) != 0 {
			req.Body.Close()
			_, _ = io.WriteString(c, "HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
			return
		}
		req.RequestURI = ""
		req.URL.Scheme, req.URL.Host = "http", "podman"
		response, err := transport.RoundTrip(req)
		req.Body.Close()
		if err != nil {
			return
		}
		err = response.Write(c)
		response.Body.Close()
		if err != nil || req.Close {
			return
		}
	}
}

func (s *service) reconcile() {
	for id, m := range s.status.Members {
		if m.State != "cleaned" {
			var receipt Membership
			if load(filepath.Join(s.dir, "member-"+id+".done"), &receipt) == nil && receipt.ID == id && receipt.UID == m.UID {
				s.status.Members[id] = receipt
			}
		}
	}
}
func (s *service) handle(c *net.UnixConn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	cred, err := s.allowed(c)
	if err != nil {
		return
	}
	buf := make([]byte, 1<<20)
	oob := make([]byte, unix.CmsgSpace(3*4))
	n, on, flags, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return
	}
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	if on > 0 {
		messages, e := unix.ParseSocketControlMessage(oob[:on])
		if e != nil {
			return
		}
		for _, m := range messages {
			fds, e := unix.ParseUnixRights(&m)
			if e != nil {
				return
			}
			for _, fd := range fds {
				unix.CloseOnExec(fd)
				files = append(files, os.NewFile(uintptr(fd), "caller-stream"))
			}
		}
	}
	var req Request
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || json.Unmarshal(buf[:n], &req) != nil {
		return
	}
	respond := func(out Response, err error) {
		if err != nil {
			out.Error = err.Error()
		}
		data, _ := json.Marshal(out)
		_, _ = c.Write(data)
	}
	s.mu.Lock()
	s.reconcile()
	if req.Operation != "status" && req.Reference != s.status.Reference {
		s.mu.Unlock()
		respond(Response{}, errors.New("network runtime identity/generation changed"))
		return
	}
	if req.Operation == "cleanup-check" {
		s.mu.Unlock()
		respond(Response{}, nil)
		return
	}
	if req.Operation == "release" {
		m, ok := s.status.Members[req.ID]
		if ok && (m.UID != cred.Uid && cred.Uid != 0 || m.State != "reserved" && m.State != "cleaned") {
			s.mu.Unlock()
			respond(Response{}, errors.New("reservation is not owned by caller"))
			return
		}
		if ok {
			m.State = "cleaned"
			s.status.Members[req.ID] = m
			err = save(filepath.Join(s.dir, "status.json"), s.status)
		}
		s.mu.Unlock()
		respond(Response{}, err)
		return
	}
	if req.Operation == "member" {
		m, ok := s.status.Members[req.ID]
		s.mu.Unlock()
		if !ok || m.UID != cred.Uid && cred.Uid != 0 {
			respond(Response{}, errors.New("network membership unavailable"))
			return
		}
		respond(Response{Member: m}, nil)
		return
	}
	if req.Operation == "status" {
		if s.status.Healthy && s.health() != nil {
			s.status.Healthy = false
		}
		out := cloneStatus(s.status)
		s.mu.Unlock()
		respond(Response{Status: out}, nil)
		return
	}
	if req.Operation == "stop" {
		busy := false
		for _, m := range s.status.Members {
			if m.State != "cleaned" {
				busy = true
			}
		}
		if entries, e := os.ReadDir(filepath.Join(s.run, "netns")); e != nil || len(entries) > 0 {
			busy = true
		}
		if busy {
			s.mu.Unlock()
			respond(Response{}, errors.New("runtime is busy; stop refused"))
			return
		}
		s.stopping = true
		s.status.Healthy = false
		err = save(filepath.Join(s.dir, "status.json"), s.status)
		if err == nil {
			close(s.stop)
		}
		s.mu.Unlock()
		respond(Response{}, err)
		return
	}
	if !s.status.Healthy || s.stopping {
		out := cloneStatus(s.status)
		s.mu.Unlock()
		respond(Response{Status: out}, errors.New("network runtime unhealthy or stopping"))
		return
	}
	if err = s.health(); err != nil {
		s.status.Healthy = false
		s.mu.Unlock()
		respond(Response{}, err)
		return
	}
	switch req.Operation {
	case "reserve":
		if len(req.ID) != 32 || !hexID(req.ID) {
			s.mu.Unlock()
			respond(Response{}, errors.New("invalid reservation ID"))
			return
		}
		if _, exists := s.status.Members[req.ID]; exists {
			s.mu.Unlock()
			respond(Response{}, errors.New("reservation already exists; reconcile instead of replaying"))
			return
		}
		s.status.Members[req.ID] = Membership{ID: req.ID, UID: cred.Uid, State: "reserved"}
		err = save(filepath.Join(s.dir, "status.json"), s.status)
		s.mu.Unlock()
		respond(Response{}, err)
	case "status", "check":
		out := cloneStatus(s.status)
		s.mu.Unlock()
		respond(Response{Status: out}, nil)
	case "forward":
		f := req.Forward
		f.UID = cred.Uid
		if f.HostPort < 1 || f.HostPort > 65535 || f.GuestPort < 1 || f.GuestPort > 65535 {
			err = errors.New("forward ports must be 1–65535")
		} else {
			for _, existing := range s.status.Forwards {
				if existing.HostPort == f.HostPort {
					if existing.UID == f.UID && existing.GuestPort == f.GuestPort {
						out := cloneStatus(s.status)
						s.mu.Unlock()
						respond(Response{Status: out}, nil)
						return
					}
					err = errors.New("forward port already owned")
					break
				}
			}
		}
		if err == nil {
			f.ID, err = s.addForward(f)
			if err == nil {
				s.status.Forwards = append(s.status.Forwards, f)
				err = save(filepath.Join(s.dir, "status.json"), s.status)
				if err != nil {
					s.status.Healthy = false
				}
			}
		}
		out := cloneStatus(s.status)
		s.mu.Unlock()
		respond(Response{Status: out}, err)
	case "exec":
		if len(files) != 3 || len(req.ID) != 32 || !hexID(req.ID) || req.Path == "" || len(req.Env) > 10000 {
			s.mu.Unlock()
			respond(Response{}, errors.New("invalid launch request"))
			return
		}
		if _, exists := s.status.Members[req.ID]; exists {
			s.mu.Unlock()
			respond(Response{}, errors.New("invocation already admitted; reconcile instead of replaying"))
			return
		}
		m := Membership{ID: req.ID, UID: cred.Uid, State: "launching", ExitCode: 125}
		s.status.Members[req.ID] = m
		if err = save(filepath.Join(s.dir, "status.json"), s.status); err != nil {
			s.mu.Unlock()
			respond(Response{}, err)
			return
		}
		launch := memberLaunch{Request: req, UID: cred.Uid, GID: cred.Gid, Keeper: s.keeper.Process.Pid, Binary: s.binary, Receipt: filepath.Join(s.dir, "member-"+req.ID+".done")}
		launch.Groups, launch.Request.Umask, err = peerAuthority(cred)
		if err != nil {
			s.mu.Unlock()
			respond(Response{}, err)
			return
		}

		launchPath := req.ID
		launchFD, e := launchFile(launch)
		if e != nil {
			s.mu.Unlock()
			respond(Response{}, e)
			return
		}
		defer launchFD.Close()

		lifeR, lifeW, e := os.Pipe()
		if e != nil {
			s.mu.Unlock()
			respond(Response{}, e)
			return
		}
		cmd := exec.Command(s.binary, "__network_runtime_member_v1", launchPath)
		if s.memberCommand != nil {
			cmd = s.memberCommand(launchPath)
		}
		cmd.ExtraFiles = []*os.File{lifeR, launchFD, s.netNS, s.mountNS}
		if s.memberCommand != nil {
			cmd.ExtraFiles = cmd.ExtraFiles[:2]
		}
		cmd.Stdin = files[0]
		cmd.Stdout = files[1]
		cmd.Stderr = files[2]
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
		err = cmd.Start()
		lifeR.Close()
		if err != nil {
			lifeW.Close()
			m.State = "cleaned"
			s.status.Members[req.ID] = m
			_ = save(filepath.Join(s.dir, "status.json"), s.status)
			s.mu.Unlock()
			respond(Response{}, err)
			return
		}
		s.mu.Unlock()
		_ = c.SetDeadline(time.Time{})
		respond(Response{Member: m}, nil)
		done := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, c); lifeW.Close(); close(done) }()
		_ = cmd.Wait()
		lifeW.Close()
		var result Membership
		e = load(launch.Receipt, &result)
		respond(Response{Member: result}, e)
		_ = c.Close()
		<-done
	default:
		s.mu.Unlock()
		respond(Response{}, errors.New("unknown runtime operation"))
	}
}
func hexID(id string) bool { _, err := hex.DecodeString(id); return err == nil }
func runMember(path string) error {
	if os.Geteuid() != 0 {
		return errors.New("member supervisor requires root")
	}
	var launch memberLaunch
	config := os.NewFile(4, "launch")
	if err := json.NewDecoder(io.LimitReader(config, 1<<20)).Decode(&launch); err != nil {
		return err
	}
	// nsenter joins only mount/network namespaces. setpriv drops to the peer's
	// account before the trusted guardian interprets any caller command or env.
	config.Close()
	groups := ""
	for _, g := range launch.Groups {
		if groups != "" {
			groups += ","
		}
		groups += strconv.FormatUint(uint64(g), 10)
	}
	args := []string{"--net=/proc/self/fd/5", "--mount=/proc/self/fd/6", "--", "/usr/bin/setpriv", "--reuid", strconv.FormatUint(uint64(launch.UID), 10), "--regid", strconv.FormatUint(uint64(launch.GID), 10)}
	if groups == "" {
		args = append(args, "--clear-groups")
	} else {
		args = append(args, "--groups", groups)
	}
	args = append(args, launch.Binary, "__network_runtime_workload_v1", path)
	// The launch file is root-only. Pass its data over fd4 after dropping UID.
	data, _ := json.Marshal(launch.Request)
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	life := os.NewFile(3, "lifetime")
	unix.CloseOnExec(3)
	cmd := exec.Command("/usr/bin/nsenter", args...)
	cmd.ExtraFiles = []*os.File{life, r, os.NewFile(5, "network-namespace"), os.NewFile(6, "mount-namespace")}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	if err = cmd.Start(); err == nil {
		r.Close()
		_, _ = w.Write(data)
		w.Close()
		err = cmd.Wait()
	} else {
		r.Close()
		w.Close()
	}
	code := 0
	if err != nil {
		code = 125
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		}
	}
	state := "cleaned"
	if code == 125 || code < 0 {
		state = "uncertain"
	}
	return save(launch.Receipt, Membership{ID: launch.Request.ID, UID: launch.UID, State: state, ExitCode: code})
}
func (s *service) slirp(request map[string]any) (json.RawMessage, error) {
	c, err := net.DialTimeout("unix", filepath.Join(s.run, "slirp.sock"), 2*time.Second)
	if err != nil {
		return nil, errors.New("runtime outbound network unavailable")
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if err = json.NewEncoder(c).Encode(request); err != nil {
		return nil, err
	}
	var out struct {
		Return json.RawMessage `json:"return"`
		Error  json.RawMessage `json:"error"`
	}
	if err = json.NewDecoder(c).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Error) > 0 {
		return nil, fmt.Errorf("slirp request failed: %s", out.Error)
	}
	return out.Return, nil
}
func (s *service) addForward(f Forward) (int, error) {
	data, err := s.slirp(map[string]any{"execute": "add_hostfwd", "arguments": map[string]any{"proto": "tcp", "host_addr": "127.0.0.1", "host_port": f.HostPort, "guest_addr": "127.0.0.1", "guest_port": f.GuestPort}})
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int `json:"id"`
	}
	err = json.Unmarshal(data, &out)
	return out.ID, err
}
func runClient(data string) error {
	var req Request
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := connect(ctx, req.Reference.Name)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, _, err = c.WriteMsgUnix([]byte(data), unix.UnixRights(0, 1, 2), nil); err != nil {
		return err
	}
	read := func() (Response, error) {
		var out Response
		buf := make([]byte, 1<<20)
		n, err := c.Read(buf)
		if err == nil {
			err = json.Unmarshal(buf[:n], &out)
		}
		if out.Error != "" {
			err = errors.New(out.Error)
		}
		return out, err
	}
	if _, err = read(); err != nil {
		return err
	}
	out, err := read()
	if err != nil {
		return err
	}
	if out.Member.State != "cleaned" {
		return errors.New("network invocation cleanup is unconfirmed")
	}
	os.Exit(out.Member.ExitCode)
	return nil
}

func workload() error {
	requestFile := os.NewFile(4, "request")
	var req Request
	if err := json.NewDecoder(io.LimitReader(requestFile, 1<<20)).Decode(&req); err != nil {
		return err
	}
	requestFile.Close()
	if req.Umask < 0 || req.Umask > 0777 {
		return errors.New("invalid caller umask")
	}
	unix.Umask(req.Umask)
	if req.Dir != "" {
		if err := os.Chdir(req.Dir); err != nil {
			return err
		}
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	// The existing guardian retains fd3, then marks it close-on-exec before
	// launching the workload. Descendants therefore cannot prolong membership.
	args := []string{binary, "__agent_workflow_guardian_v1", "3"}
	if req.Terminal {
		args = append(args, "--terminal")
	}
	return syscall.Exec(binary, append(append(args, req.Path), req.Args...), req.Env)
}

func cloneStatus(in Status) Status {
	out := in
	out.Members = make(map[string]Membership, len(in.Members))
	for k, v := range in.Members {
		out.Members[k] = v
	}
	out.Forwards = append([]Forward(nil), in.Forwards...)
	return out
}
func launchFile(launch memberLaunch) (*os.File, error) {
	fd, err := unix.MemfdCreate("runtime-launch", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "launch")
	data, err := json.Marshal(launch)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		_, err = f.Seek(0, 0)
	}
	if err == nil {
		_, err = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func configureNamespace(run string) error {
	expected, err := os.Stat(filepath.Join(run, "resolv.conf"))
	if err != nil {
		return err
	}
	actual, err := os.Stat("/etc/resolv.conf")
	if err != nil || !os.SameFile(expected, actual) {
		return errors.New("private runtime mount required")
	}
	if _, err = net.InterfaceByName("tap0"); err != nil {
		return err
	}
	// slirp delivers 127.0.0.1 hostfwd packets through tap0. Enable local
	// destinations on this runtime interface only, never on a host interface.
	return os.WriteFile("/proc/sys/net/ipv4/conf/tap0/route_localnet", []byte("1\n"), 0600)
}
func probeNamespace(run string) error {
	localnet, err := os.ReadFile("/proc/sys/net/ipv4/conf/tap0/route_localnet")
	if err != nil || string(localnet) != "1\n" {
		return errors.New("runtime loopback forwarding unavailable")
	}

	for _, pair := range [][2]string{{"networks", "/run/containers/networks"}, {"netconfig", "/etc/containers/networks"}, {"netns", "/run/netns"}, {"resolv.conf", "/etc/resolv.conf"}} {
		expected, err := os.Stat(filepath.Join(run, pair[0]))
		if err != nil {
			return err
		}
		actual, err := os.Stat(pair[1])
		if err != nil || !os.SameFile(expected, actual) {
			return errors.New("private networking mount lost")
		}
	}
	for _, name := range []string{"lo", "tap0"} {
		device, err := net.InterfaceByName(name)
		if err != nil || device.Flags&net.FlagUp == 0 {
			return errors.New("runtime network interface unavailable")
		}
	}
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil || string(data) != "nameserver 10.0.2.3\noptions timeout:2 attempts:2\n" {
		return errors.New("runtime resolver changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", "10.0.2.3:53")
	}}
	_, err = resolver.LookupHost(ctx, "agent-workflow-runtime.invalid.")
	var dns *net.DNSError
	if err == nil || errors.As(err, &dns) && dns.IsNotFound {
		return nil
	}
	return errors.New("runtime DNS unavailable")
}

func peerAuthority(cred *unix.Ucred) ([]uint32, int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", cred.Pid))
	if err != nil {
		return nil, 0, err
	}
	var groups []uint32
	var mask uint64
	uidOK, gidOK, groupsOK, maskOK := false, false, false, false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "Uid:" && len(fields) >= 3 {
			uidOK = fields[2] == strconv.FormatUint(uint64(cred.Uid), 10)
		}
		if fields[0] == "Gid:" && len(fields) >= 3 {
			gidOK = fields[2] == strconv.FormatUint(uint64(cred.Gid), 10)
		}
		if fields[0] == "Umask:" {
			mask, err = strconv.ParseUint(fields[1], 8, 9)
			maskOK = err == nil
		}
		if fields[0] == "Groups:" {
			groupsOK = true
			for _, field := range fields[1:] {
				gid, e := strconv.ParseUint(field, 10, 32)
				if e != nil {
					return nil, 0, e
				}
				groups = append(groups, uint32(gid))
			}
		}
	}
	if !uidOK || !gidOK || !groupsOK || !maskOK {
		return nil, 0, errors.New("caller credential or umask unavailable during admission")
	}
	return groups, int(mask), nil
}
