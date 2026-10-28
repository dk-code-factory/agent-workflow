//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	case "flood-workload":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 128*1024))
		os.Exit(0)
	case "drain-frontend":
		os.Exit(terminalDrainFrontend())
	case "frontend":
		os.Exit(terminalFrontend())
	case "workload":
		os.Exit(terminalWorkload())
	case "pipes":
		cmd := exec.Command("sh", "-c", "printf out; printf err >&2")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		finish, err := prepareTerminal(context.Background(), func() {}, cmd)
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
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signalCtx)
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
	finish, err := prepareTerminal(ctx, cancel, cmd)
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
	if drainErr := finish(); drainErr != nil {
		fmt.Fprintln(os.Stderr, drainErr)
		return 122
	}
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
	finish, err := bridgeTerminal(context.Background(), func() {}, cmd, slave, slave)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if cmd.Run() == nil {
		t.Fatal("missing command succeeded")
	}
	if err := finish(); err != nil {
		t.Fatalf("launch failure terminal cleanup: %v", err)
	}
	current, err := unix.IoctlGetTermios(terminalFD(slave), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(current, saved) {
		t.Fatalf("launch error did not restore caller termios: %v", err)
	}
}

const terminalFinalOutput = "accepted final terminal output"

// The output pipe is owned by this test. Filling its bounded kernel buffer
// blocks the relay without changing any caller descriptor flags.
func fullTerminalOutputPipe(t *testing.T) (*os.File, *os.File, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close(); writer.Close() })
	capacity, err := unix.FcntlInt(uintptr(terminalFD(writer)), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat(".", capacity)
	if _, err := io.WriteString(writer, filler); err != nil {
		t.Fatal(err)
	}
	return reader, writer, filler
}

func startTerminalDrain(t *testing.T, output *os.File) (context.CancelFunc, func() error, *os.File, *unix.Termios) {
	t.Helper()
	return startTerminalDrainExit(t, output, 0)
}

func startTerminalDrainExit(t *testing.T, output *os.File, code int) (context.CancelFunc, func() error, *os.File, *unix.Termios) {
	t.Helper()
	master, input, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); input.Close() })
	saved, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command("sh", "-c", "printf '%s' '"+terminalFinalOutput+"'; exit "+strconv.Itoa(code))
	finish, err := bridgeTerminal(ctx, cancel, cmd, input, output)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); output.Close(); _ = finish() })
	err = cmd.Run()
	if code == 0 && err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != code {
			t.Fatalf("workload exit = %v, want %d", err, code)
		}
	}
	return cancel, finish, input, saved
}

func awaitTerminalDrain(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("terminal drain did not finish within 5 seconds")
		return nil
	}
}

func TestTerminalDrainWaitsForBackpressure(t *testing.T) {
	for _, code := range []int{0, 37, 125} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			reader, writer, filler := fullTerminalOutputPipe(t)
			_, finish, input, saved := startTerminalDrainExit(t, writer, code)
			done := make(chan error, 1)
			go func() { done <- finish() }()
			select {
			case err := <-done:
				t.Fatalf("drain returned before output resumed: %v", err)
			case <-time.After(1200 * time.Millisecond):
			}
			current, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
			if err != nil || !reflect.DeepEqual(current, saved) {
				t.Fatalf("caller termios not restored while drain blocked: %v", err)
			}
			if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			want := filler + terminalFinalOutput
			got := make([]byte, len(want))
			if _, err := io.ReadFull(reader, got); err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Fatalf("drained output differs: got %d bytes, want %d", len(got), len(want))
			}
			if err := awaitTerminalDrain(t, done); err != nil {
				t.Fatal(err)
			}
			if err := finish(); err != nil {
				t.Fatalf("repeated cleanup: %v", err)
			}
		})
	}
}

func TestTerminalDrainCancellationReturnsError(t *testing.T) {
	_, writer, _ := fullTerminalOutputPipe(t)
	cancel, finish, input, saved := startTerminalDrain(t, writer)
	done := make(chan error, 1)
	go func() { done <- finish() }()
	cancel()
	err := awaitTerminalDrain(t, done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked drain cancellation = %v", err)
	}
	if repeated := finish(); repeated != err {
		t.Fatalf("cleanup error changed: %v, then %v", err, repeated)
	}
	current, termErr := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
	if termErr != nil || !reflect.DeepEqual(current, saved) {
		t.Fatalf("caller termios not restored after cancellation: %v", termErr)
	}
}

func TestTerminalDrainReportsDestinationEIO(t *testing.T) {
	master, output, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	master.Close() // Writes to this owned slave now fail with EIO.
	_, finish, _, _ := startTerminalDrain(t, output)
	done := make(chan error, 1)
	go func() { done <- finish() }()
	if err := awaitTerminalDrain(t, done); !errors.Is(err, unix.EIO) {
		t.Fatalf("destination EIO was hidden: %v", err)
	}
}

// This frontend must exit despite its output goroutine remaining blocked in a
// caller-owned writer. The parent owns that writer and releases it after exit.
func terminalDrainFrontend() int {
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()
	output := os.NewFile(3, "owned-blocked-output")
	defer output.Close()
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+terminalFinalOutput+"'")
	if os.Getenv("AW_TERMINAL_DRAIN") == "failed-output" {
		self, _ := os.Executable()
		cmd = exec.CommandContext(ctx, self, "-test.run=^TestTerminalFixture$")
		cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE=flood-workload")
	}
	finish, err := bridgeTerminal(ctx, cancel, cmd, os.Stdin, output)
	if err != nil {
		return 120
	}
	defer finish()
	runErr := cmd.Run()
	if runErr != nil && cmd.Process == nil {
		cancel()
	}
	fmt.Println("ACCEPTED")
	if err := finish(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 122
	}
	if runErr != nil {
		return 121
	}
	return 0
}

func TestTerminalCancelledFrontendExitsWithBlockedOutput(t *testing.T) {
	_, writer, _ := fullTerminalOutputPipe(t)
	master, input, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer input.Close()
	saved, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	defer readyW.Close()
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestTerminalFixture$")
	cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE=drain-frontend")
	cmd.Stdin, cmd.Stdout = input, readyW
	cmd.ExtraFiles = []*os.File{writer}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
	})
	if err := readyR.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	accepted := make([]byte, len("ACCEPTED\n"))
	if _, err := io.ReadFull(readyR, accepted); err != nil || string(accepted) != "ACCEPTED\n" {
		t.Fatalf("frontend did not accept final output: %q, %v", accepted, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = awaitTerminalDrain(t, done)
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 122 {
		t.Fatalf("cancelled frontend exit = %v; diagnostics %q", err, diagnostics.String())
	}
	if !strings.Contains(diagnostics.String(), "drain terminal output: context canceled") {
		t.Fatalf("missing explicit drain failure: %q", diagnostics.String())
	}
	current, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(current, saved) {
		t.Fatalf("cancelled frontend did not restore termios: %v", err)
	}
}

func TestTerminalDestinationFailureCancelsLargeOutputFrontend(t *testing.T) {
	master, input, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer input.Close()
	saved, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	outputMaster, output, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	outputMaster.Close()
	defer output.Close()
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestTerminalFixture$")
	cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE=drain-frontend", "AW_TERMINAL_DRAIN=failed-output")
	cmd.Stdin = input
	cmd.ExtraFiles = []*os.File{output}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cancel(); _ = cmd.Process.Kill() })
	err = awaitTerminalDrain(t, done)
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 122 {
		t.Fatalf("failed-output frontend exit = %v; diagnostics %q", err, diagnostics.String())
	}
	if !strings.Contains(diagnostics.String(), "drain terminal output:") || !strings.Contains(diagnostics.String(), "input/output error") {
		t.Fatalf("missing original destination EIO: %q", diagnostics.String())
	}
	current, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(current, saved) {
		t.Fatalf("failed-output frontend did not restore termios: %v", err)
	}
}

func TestTerminalFailedLaunchWithRetainedSlaveBoundsCleanup(t *testing.T) {
	master, input, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer input.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing"))
	finish, err := bridgeTerminal(ctx, cancel, cmd, input, input)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	fd, err := unix.Dup(terminalFD(cmd.Stdout.(*os.File)))
	if err != nil {
		t.Fatal(err)
	}
	retained := os.NewFile(uintptr(fd), "owned-retained-slave")
	defer retained.Close()
	if err := cmd.Run(); err == nil {
		t.Fatal("missing workload succeeded")
	}
	cancel() // The CLI cancels failed launch/supervision before cleanup.
	done := make(chan error, 1)
	go func() { done <- finish() }()
	if err := awaitTerminalDrain(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("retained-slave cleanup = %v", err)
	}
}

func TestTerminalStderrTerminalIdentity(t *testing.T) {
	for _, sameTerminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-terminal=%t", sameTerminal), func(t *testing.T) {
			stdoutMaster, stdout, err := openTerminal()
			if err != nil {
				t.Fatal(err)
			}
			defer stdoutMaster.Close()
			defer stdout.Close()
			var stderrMaster, stderr *os.File
			if sameTerminal {
				fd, err := unix.Dup(terminalFD(stdout))
				if err != nil {
					t.Fatal(err)
				}
				stderr = os.NewFile(uintptr(fd), "owned-duplicate-terminal")
			} else {
				stderrMaster, stderr, err = openTerminal()
				if err != nil {
					t.Fatal(err)
				}
				defer stderrMaster.Close()
			}
			defer stderr.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", "printf stdout-payload; printf stderr-payload >&2")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = stdout, stdout, stderr
			finish, err := bridgeTerminal(ctx, cancel, cmd, stdout, stdout)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			if sameTerminal {
				if cmd.Stderr != cmd.Stdout || cmd.Stderr == stderr {
					t.Fatal("duplicate descriptor for same terminal was not bridged")
				}
			} else if cmd.Stderr != stderr {
				t.Fatal("separate terminal descriptor was replaced")
			}
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			if err := finish(); err != nil {
				t.Fatal(err)
			}
			want := "stdout-payload"
			if sameTerminal {
				want += "stderr-payload"
			}
			out := &terminalFixture{master: stdoutMaster}
			out.expect(t, want)
			if out.output.String() != want {
				t.Fatalf("stdout terminal = %q, want %q", out.output.String(), want)
			}
			if !sameTerminal {
				diag := &terminalFixture{master: stderrMaster}
				diag.expect(t, "stderr-payload")
				if diag.output.String() != "stderr-payload" {
					t.Fatalf("stderr terminal = %q", diag.output.String())
				}
			}
		})
	}
}
