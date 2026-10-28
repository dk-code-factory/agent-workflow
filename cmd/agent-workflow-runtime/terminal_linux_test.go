//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// All session, terminal and signal changes happen in disposable child processes
// and PTYs. The test runner never supplies its terminal to the fixture.
func TestTerminalFixture(t *testing.T) {
	switch os.Getenv("AW_TERMINAL_FIXTURE") {
	case "frontend":
		os.Exit(terminalFrontend())
	case "workload":
		os.Exit(terminalWorkload())
	case "pipes":
		cmd := exec.Command("sh", "-c", "printf out; printf err >&2")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		finish, err := prepareTerminal(cmd)
		if err != nil {
			os.Exit(120)
		}
		defer finish()
		if cmd.Stdin != os.Stdin || cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr || cmd.Env != nil {
			os.Exit(121)
		}
		if cmd.Run() != nil {
			os.Exit(122)
		}
	}
}

func terminalFrontend() int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	lifeR, lifeW, err := os.Pipe()
	if err != nil {
		return 120
	}
	defer lifeR.Close()
	defer lifeW.Close()
	self, _ := os.Executable()
	cmd := exec.CommandContext(ctx, self, "__agent_workflow_guardian_v1", "3", self, "-test.run=^TestTerminalFixture$")
	cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE=workload")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{lifeR}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return lifeW.Close() }
	cmd.WaitDelay = 5 * time.Second
	finish, err := prepareTerminal(cmd)
	if err != nil {
		return 121
	}
	defer finish()
	for _, value := range cmd.Env {
		if value == "AGENT_WORKFLOW_NETWORK_TERMINAL=1" {
			cmd.Args = append(cmd.Args[:3], append([]string{"--terminal"}, cmd.Args[3:]...)...)
			break
		}
	}
	err = cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	if err != nil {
		return 122
	}
	return 0
}

func terminalWorkload() int {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		fmt.Println("NO-CTTY", err)
		return 123
	}
	defer tty.Close()
	fd := terminalFD(tty)
	fg, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil || fg != syscall.Getpgrp() {
		return 124
	}
	termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return 124
	}
	termios.Lflag &^= unix.ICANON | unix.ECHO
	termios.Cc[unix.VMIN] = 1
	if unix.IoctlSetTermios(fd, unix.TCSETS, termios) != nil {
		return 124
	}
	desc := exec.Command("/usr/bin/setsid", "sleep", "300")
	if err = desc.Start(); err != nil {
		return 124
	}
	if err = os.WriteFile(os.Getenv("AW_TERMINAL_PID"), []byte(strconv.Itoa(desc.Process.Pid)), 0600); err != nil {
		return 124
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGINT)
	size, _ := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	fmt.Fprintln(os.Stderr, "WORKLOAD DIAGNOSTIC")
	fmt.Printf("READY %d %d\n", size.Row, size.Col)
	go func() {
		for sig := range signals {
			if sig == syscall.SIGINT {
				fmt.Println("INTERRUPTED")
				os.Exit(130)
			}
			size, _ := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
			fmt.Printf("SIZE %d %d\n", size.Row, size.Col)
		}
	}()
	for {
		var b [1]byte
		if _, err := tty.Read(b[:]); err != nil {
			return 124
		}
		if b[0] == 'q' {
			fmt.Println("BYE")
			return 37
		}
		fmt.Printf("KEY %d\n", b[0])
	}
}

type terminalFixture struct {
	master, slave *os.File
	cmd           *exec.Cmd
	done          chan struct{}
	waitErr       error
	pidFile       string
	saved         *unix.Termios
	output        strings.Builder
}

func startTerminalFixture(t *testing.T) *terminalFixture {
	return startTerminalFixtureWithStderr(t, nil)
}

func startTerminalFixtureWithStderr(t *testing.T, diagnostics *os.File) *terminalFixture {
	t.Helper()
	master, slave, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	saved, err := unix.IoctlGetTermios(terminalFD(slave), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.IoctlSetWinsize(terminalFD(slave), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	pidFile := filepath.Join(t.TempDir(), "descendant")
	cmd := exec.Command(self, "-test.run=^TestTerminalFixture$")
	cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE=frontend", "AW_TERMINAL_PID="+pidFile)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if diagnostics != nil {
		cmd.Stderr = diagnostics
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f := &terminalFixture{master: master, slave: slave, cmd: cmd, pidFile: pidFile, saved: saved, done: make(chan struct{})}
	go func() {
		f.waitErr = cmd.Wait()
		close(f.done)
	}()
	t.Cleanup(func() {
		select {
		case <-f.done:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-f.done:
			return
		case <-time.After(5 * time.Second):
		}
		_ = cmd.Process.Kill()
		select {
		case <-f.done:
		case <-time.After(5 * time.Second):
			t.Error("terminal fixture did not exit after kill")
		}
	})
	f.expect(t, "READY 24 80")
	return f
}

func (f *terminalFixture) expect(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("terminal output %q does not contain %q", got.String(), want)
		}
		fds := []unix.PollFd{{Fd: int32(terminalFD(f.master)), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			buf := make([]byte, 4096)
			n, err := unix.Read(terminalFD(f.master), buf)
			if err != nil && err != unix.EAGAIN {
				t.Fatal(err)
			}
			got.Write(buf[:n])
			f.output.Write(buf[:n])
		}
	}
}

func (f *terminalFixture) finish(t *testing.T, code int) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal fixture did not finish within 10 seconds")
	}
	err := f.waitErr
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != code {
		t.Fatalf("exit = %v, want %d", err, code)
	}
	current, err := unix.IoctlGetTermios(terminalFD(f.slave), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(current, f.saved) {
		t.Fatalf("caller termios not restored: %v", err)
	}
	data, err := os.ReadFile(f.pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(data))
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("detached descendant %d survived: %v", pid, err)
	}
}

func TestTerminalInputResizeExitAndRestoration(t *testing.T) {
	f := startTerminalFixture(t)
	_, _ = f.master.Write([]byte("a"))
	f.expect(t, "KEY 97")
	if err := unix.IoctlSetWinsize(terminalFD(f.slave), unix.TIOCSWINSZ, &unix.Winsize{Row: 41, Col: 103}); err != nil {
		t.Fatal(err)
	}
	if err := f.cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "SIZE 41 103")
	_, _ = f.master.Write([]byte("q"))
	f.expect(t, "BYE")
	f.finish(t, 37)
}

func TestTerminalWithRedirectedStderrPreservesDiagnostics(t *testing.T) {
	diagnostics, err := os.CreateTemp(t.TempDir(), "diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	f := startTerminalFixtureWithStderr(t, diagnostics)
	_, _ = f.master.Write([]byte("a"))
	f.expect(t, "KEY 97")
	if err = unix.IoctlSetWinsize(terminalFD(f.slave), unix.TIOCSWINSZ, &unix.Winsize{Row: 41, Col: 103}); err != nil {
		t.Fatal(err)
	}
	if err = f.cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "SIZE 41 103")
	_, _ = f.master.Write([]byte("q"))
	f.expect(t, "BYE")
	f.finish(t, 37)
	data, err := os.ReadFile(diagnostics.Name())
	if err != nil || string(data) != "WORKLOAD DIAGNOSTIC\n" {
		t.Fatalf("redirected stderr = %q, %v", data, err)
	}
	if strings.Contains(f.output.String(), "WORKLOAD DIAGNOSTIC") {
		t.Fatal("redirected diagnostic was merged into the terminal output")
	}
}

func TestTerminalInterruptAndCallerCancellationPreserveOtherCaller(t *testing.T) {
	a, b := startTerminalFixture(t), startTerminalFixture(t)
	_, _ = a.master.Write([]byte{3})
	a.expect(t, "INTERRUPTED")
	a.finish(t, 130)
	_, _ = b.master.Write([]byte("b"))
	b.expect(t, "KEY 98")
	c := startTerminalFixture(t)
	if err := b.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	b.finish(t, 137)
	_, _ = c.master.Write([]byte("c"))
	c.expect(t, "KEY 99")
	_, _ = c.master.Write([]byte("q"))
	c.expect(t, "BYE")
	c.finish(t, 37)
}

func TestTerminalPipesRemainUnchanged(t *testing.T) {
	self, _ := os.Executable()
	cmd := exec.Command(self, "-test.run=^TestTerminalFixture$")
	cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE=pipes")
	cmd.Stdin = strings.NewReader("")
	var out, diag strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &diag
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "outPASS\n" || diag.String() != "err" {
		t.Fatalf("pipes = %q, %q", out.String(), diag.String())
	}
}

func TestTerminalLaunchFailureRestoresCaller(t *testing.T) {
	master, slave, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	saved, err := unix.IoctlGetTermios(terminalFD(slave), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing"))
	finish, err := bridgeTerminal(cmd, slave, slave)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if cmd.Run() == nil {
		t.Fatal("missing command succeeded")
	}
	finish()
	current, err := unix.IoctlGetTermios(terminalFD(slave), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(current, saved) {
		t.Fatalf("launch error did not restore caller termios: %v", err)
	}
}
