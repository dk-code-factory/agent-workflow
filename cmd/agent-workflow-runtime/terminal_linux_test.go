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
	case "utf8-workload":
		os.Exit(terminalUTF8Workload())
	case "sink-ctty":
		device, err := unix.IoctlGetInt(0, unix.TIOCGDEV)
		if err != nil {
			os.Exit(120)
		}
		sink, owned := terminalSink(device, os.Stdin, os.Stdout, os.Stderr)
		if sink == nil || !owned || sink.Name() != "/dev/tty" {
			os.Exit(121)
		}
		fmt.Fprint(sink, "CTTY-SINK")
		sink.Close()
		countAliases := func() int {
			entries, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, entry := range entries {
				fd, _ := strconv.Atoi(entry.Name())
				if actual, err := unix.IoctlGetInt(fd, unix.TIOCGDEV); err == nil && actual == device {
					count++
				}
			}
			return count
		}
		before := countAliases()
		_, launchOutput, _ := terminalPair(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		missing := exec.Command(filepath.Join(t.TempDir(), "missing"))
		finishLaunch, err := bridgeTerminal(ctx, cancel, missing, os.Stdin, launchOutput)
		if err != nil {
			t.Fatal(err)
		}
		defer finishLaunch()
		if countAliases() != before+1 {
			t.Fatal("read-only bridge did not own exactly one reopened sink")
		}
		if missing.Run() == nil {
			t.Fatal("missing command succeeded")
		}
		cancel()
		if err := finishLaunch(); err != nil || countAliases() != before {
			t.Fatalf("failed Start leaked owned sink: %v", err)
		}
		os.Exit(0)
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
	workload := "workload"
	if os.Getenv("AW_TERMINAL_UTF8") != "" {
		workload = "utf8-workload"
	}
	cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE="+workload)
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
	master, input, _ := terminalPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf control >&2")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, input, input
	finish, err := bridgeTerminal(ctx, cancel, cmd, input, input)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	master.Close() // Only the owned input/control relay destination now fails.
	_ = cmd.Run()
	done := make(chan error, 1)
	go func() { done <- finish() }()
	if err := awaitTerminalDrain(t, done); !errors.Is(err, unix.EIO) || !strings.Contains(err.Error(), "drain terminal output") {
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
	outputReader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outputReader.Close()
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
	if !strings.Contains(diagnostics.String(), "drain terminal output:") || !strings.Contains(diagnostics.String(), "broken pipe") {
		t.Fatalf("missing original destination failure: %q", diagnostics.String())
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

func terminalPair(t *testing.T) (*os.File, *os.File, *unix.Termios) {
	t.Helper()
	master, slave, err := openTerminal()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	modes, err := unix.IoctlGetTermios(terminalFD(slave), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave, modes
}

func terminalSetOutput(t *testing.T, file *os.File, flags uint32) *unix.Termios {
	t.Helper()
	modes, err := unix.IoctlGetTermios(terminalFD(file), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	modes.Oflag = flags
	if err := unix.IoctlSetTermios(terminalFD(file), unix.TCSETS, modes); err != nil {
		t.Fatal(err)
	}
	return modes
}

func terminalAssertModes(t *testing.T, file *os.File, want *unix.Termios) {
	t.Helper()
	got, err := unix.IoctlGetTermios(terminalFD(file), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal %s modes = %+v, want %+v; error %v", file.Name(), got, want, err)
	}
}

// Called only after a completed workload/drain, when all expected writes have
// reached this owned PTY. A finite poll also captures unexpected extra bytes.
func terminalReadBuffered(t *testing.T, master *os.File) []byte {
	t.Helper()
	var got []byte
	for {
		fds := []unix.PollFd{{Fd: int32(terminalFD(master)), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 20)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			return got
		}
		buf := make([]byte, 4096)
		n, err := unix.Read(terminalFD(master), buf)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
}

func TestTerminalOutputBytesMatchNative(t *testing.T) {
	for _, same := range []bool{false, true} {
		for _, inputPost := range []bool{false, true} {
			for _, outputPost := range []bool{false, true} {
				if same && inputPost != outputPost {
					continue // One terminal has one shared set of output modes.
				}
				for _, dynamic := range []bool{false, true} {
					name := fmt.Sprintf("same=%t/input-post=%t/output-post=%t/dynamic=%t", same, inputPost, outputPost, dynamic)
					t.Run(name, func(t *testing.T) {
						capture := func(bridged bool) []byte {
							outMaster, output, _ := terminalPair(t)
							input := output
							if !same {
								_, input, _ = terminalPair(t)
							}
							inputFlags, outputFlags := uint32(unix.ONLCR), uint32(unix.ONLCR)
							if inputPost {
								inputFlags |= unix.OPOST
							}
							if outputPost {
								outputFlags |= unix.OPOST
								if !same {
									outputFlags |= unix.OCRNL
								}
							}
							savedInput := terminalSetOutput(t, input, inputFlags)
							savedOutput := terminalSetOutput(t, output, outputFlags)
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							script := "printf 'first\r\n'"
							if dynamic {
								script += "; stty -opost <&1; printf 'raw\r\n'; stty opost onlcr <&1; printf 'processed\r\n'"
							}
							cmd := exec.CommandContext(ctx, "sh", "-c", script)
							cmd.Stdin, cmd.Stdout = input, output
							if bridged {
								finish, err := bridgeTerminal(ctx, cancel, cmd, input, output)
								if err != nil {
									t.Fatal(err)
								}
								defer finish()
								if err := cmd.Run(); err != nil {
									t.Fatal(err)
								}
								if err := finish(); err != nil {
									t.Fatal(err)
								}
								terminalAssertModes(t, input, savedInput)
								terminalAssertModes(t, output, savedOutput)
							} else if err := cmd.Run(); err != nil {
								t.Fatal(err)
							}
							return terminalReadBuffered(t, outMaster)
						}
						native, bridged := capture(false), capture(true)
						if !bytes.Equal(bridged, native) {
							t.Fatalf("bridge output %q (%x), native %q (%x)", bridged, bridged, native, native)
						}
					})
				}
			}
		}
	}
}

// Fill only a disposable PTY through a separately opened nonblocking writer;
// flags on the descriptors supplied to bridgeTerminal never change.
func terminalFillOutput(t *testing.T, output *os.File) []byte {
	t.Helper()
	fd, err := unix.Open(output.Name(), unix.O_WRONLY|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var filled []byte
	buf := bytes.Repeat([]byte("."), 1024)
	for len(filled) < 1024*1024 {
		n, err := unix.Write(fd, buf)
		if err == unix.EAGAIN {
			// The PTY flip-buffer worker may still move accepted bytes to
			// its bounded reader queue. Wait for that work before deciding
			// the destination applies steady backpressure.
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
			if n, err := unix.Poll(fds, 50); err == unix.EINTR {
				continue
			} else if err != nil {
				t.Fatal(err)
			} else if n == 0 {
				return filled
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		filled = append(filled, buf[:n]...)
	}
	t.Fatal("owned PTY did not apply bounded backpressure")
	return nil
}

func terminalControlFixture(t *testing.T) bool {
	t.Helper()
	if os.Getenv("AW_TERMINAL_CONTROL_FIXTURE") == t.Name() {
		return false
	}
	master, slave, _ := terminalPair(t)
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "AW_TERMINAL_CONTROL_FIXTURE="+t.Name())
	cmd.Stdin = slave
	cmd.ExtraFiles = []*os.File{master}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("owned controlling-tty fixture: %v, %s", err, output)
	}
	return true
}

func terminalFixtureInput(t *testing.T) (*os.File, *os.File, *unix.Termios) {
	t.Helper()
	fd, err := unix.FcntlInt(3, unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "owned-ctty-master")
	t.Cleanup(func() { master.Close() })
	modes, err := unix.IoctlGetTermios(0, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return master, os.Stdin, modes
}

func TestTerminalOutputDrainRestoresModesAfterFinalNewline(t *testing.T) {
	if terminalControlFixture(t) {
		return
	}
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same=%t", same), func(t *testing.T) {
			master, output, savedOutput := terminalPair(t)
			input, savedInput := output, savedOutput
			if !same {
				master, input, savedInput = terminalFixtureInput(t)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			payload := strings.Repeat("final\n", 2048)
			cmd := exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$1\" >&2; exit 37", "fixture", payload)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, input
			finish, err := bridgeTerminal(ctx, cancel, cmd, input, output)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			filler := terminalFillOutput(t, input)
			if err := cmd.Run(); err == nil || err.(*exec.ExitError).ExitCode() != 37 {
				t.Fatalf("workload exit = %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- finish() }()
			select {
			case err := <-done:
				t.Fatalf("backpressured drain returned early: %v", err)
			case <-time.After(1200 * time.Millisecond):
			}
			modes, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
			if err != nil || modes.Oflag&unix.OPOST != 0 {
				t.Fatalf("outer output processing enabled before drain: %+v, %v", modes, err)
			}
			inputModes := *savedInput
			inputModes.Oflag &^= unix.OPOST
			terminalAssertModes(t, input, &inputModes)
			want := append(filler, []byte(strings.ReplaceAll(payload, "\n", "\r\n"))...)
			if err := master.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(master, got); err != nil {
				t.Fatal(err)
			}
			if err := awaitTerminalDrain(t, done); err != nil {
				t.Fatal(err)
			}
			got = append(got, terminalReadBuffered(t, master)...)
			if !bytes.Equal(got, want) {
				t.Fatalf("final output = %q, want %q", got[len(filler):], want[len(filler):])
			}
			terminalAssertModes(t, input, savedInput)
			terminalAssertModes(t, output, savedOutput)
		})
	}
}

func TestTerminalBlockedOutputCancellationRestoresBothTerminals(t *testing.T) {
	if terminalControlFixture(t) {
		return
	}
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same=%t", same), func(t *testing.T) {
			_, output, savedOutput := terminalPair(t)
			input, savedInput := output, savedOutput
			if !same {
				_, input, _ = terminalFixtureInput(t)
				savedInput = terminalSetOutput(t, input, unix.ONLCR)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$1\" >&2", "fixture", strings.Repeat("final\n", 2048))
			cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, input
			finish, err := bridgeTerminal(ctx, cancel, cmd, input, output)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			_ = terminalFillOutput(t, input)
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- finish() }()
			select {
			case err := <-done:
				t.Fatalf("blocked drain returned before cancellation: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			inputModes := *savedInput
			inputModes.Oflag &^= unix.OPOST
			terminalAssertModes(t, input, &inputModes) // ISIG/ICANON/ECHO restored during drain.
			cancel()
			if err := awaitTerminalDrain(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("blocked TTY drain cancellation = %v", err)
			}
			terminalAssertModes(t, input, savedInput)
			terminalAssertModes(t, output, savedOutput)
		})
	}
}

func TestTerminalSetupAndLaunchFailuresPreserveBothModes(t *testing.T) {
	_, output, savedOutput := terminalPair(t)
	inputR, inputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputR.Close()
	defer inputW.Close()
	cmd := exec.Command("true")
	cmd.Stdin, cmd.Stdout = inputR, output
	if _, err := bridgeTerminal(context.Background(), func() {}, cmd, inputR, output); err == nil {
		t.Fatal("nonterminal input was accepted by terminal-only bridge")
	}
	if cmd.Stdin != inputR || cmd.Stdout != output {
		t.Fatal("initial setup failure changed command streams")
	}
	terminalAssertModes(t, output, savedOutput)
	_, input, _ := terminalPair(t)
	savedInput := terminalSetOutput(t, input, unix.ONLCR)
	cmd = exec.Command(filepath.Join(t.TempDir(), "missing"))
	cmd.Stdin, cmd.Stdout = input, output
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finish, err := bridgeTerminal(ctx, cancel, cmd, input, output)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if err := cmd.Run(); err == nil {
		t.Fatal("missing command succeeded")
	}
	cancel()
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	terminalAssertModes(t, input, savedInput)
	terminalAssertModes(t, output, savedOutput)
}

func TestTerminalDistinctInputRetainsDirectStderrOutputModes(t *testing.T) {
	if terminalControlFixture(t) {
		return
	}
	inputMaster, input, savedInput := terminalFixtureInput(t)
	outputMaster, output, _ := terminalPair(t)
	savedOutput := terminalSetOutput(t, output, unix.ONLCR)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'stdout\n'; printf 'stderr\n' >&2")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, input
	finish, err := bridgeTerminal(ctx, cancel, cmd, input, output)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if cmd.Stderr != cmd.Stdin.(*os.File) {
		t.Fatal("stderr targeting distinct stdin terminal was not routed to private input")
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if got := terminalReadBuffered(t, outputMaster); string(got) != "stdout\n" {
		t.Fatalf("raw stdout = %q", got)
	}
	if got := terminalReadBuffered(t, inputMaster); string(got) != "stderr\r\n" {
		t.Fatalf("processed stderr on input terminal = %q", got)
	}
	terminalAssertModes(t, input, savedInput)
	terminalAssertModes(t, output, savedOutput)
}

func terminalUTF8Workload() int {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return 120
	}
	defer tty.Close()
	fmt.Fprint(tty, "CONTROL\n")
	fmt.Fprint(os.Stderr, "DIAGNOSTIC\n")
	prefix := os.Getenv("AW_TERMINAL_UTF8")
	for phase := 0; phase < 2; phase++ {
		if phase == 1 {
			attrs, err := unix.IoctlGetTermios(1, unix.TCGETS)
			if err != nil {
				return 121
			}
			attrs.Iflag ^= unix.IUTF8
			if unix.IoctlSetTermios(1, unix.TCSETS, attrs) != nil {
				return 121
			}
		}
		fmt.Fprint(os.Stdout, "é\t\n")
		name := prefix + strconv.Itoa(phase)
		if os.WriteFile(name+".ready", nil, 0600) != nil {
			return 122
		}
		buf := make([]byte, 32)
		n, err := os.Stdin.Read(buf)
		// Publish completion only after all bytes are written and closed.
		// Creating .read directly lets the observer see an empty result.
		pending := name + ".read.pending"
		if err != nil || os.WriteFile(pending, buf[:n], 0600) != nil || os.Rename(pending, name+".read") != nil {
			return 122
		}
	}
	return 37
}

func waitTerminalFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return data
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal fixture marker missing: %s", path)
	return nil
}

func TestTerminalDistinctUTF8NativeAndGuardian(t *testing.T) {
	for _, inputUTF8 := range []bool{false, true} {
		for _, echo := range []bool{false, true} {
			for _, diagnostic := range []string{"input", "output", "third"} {
				t.Run(fmt.Sprintf("input-utf8=%t/echo=%t/stderr=%s", inputUTF8, echo, diagnostic), func(t *testing.T) {
					capture := func(guarded bool) [3][]byte {
						inputMaster, input, savedInput := terminalPair(t)
						outputMaster, output, savedOutput := terminalPair(t)
						thirdMaster, third, _ := terminalPair(t)
						savedInput.Iflag &^= unix.IUTF8
						savedOutput.Iflag |= unix.IUTF8
						if inputUTF8 {
							savedInput.Iflag |= unix.IUTF8
							savedOutput.Iflag &^= unix.IUTF8
						}
						savedInput.Lflag &^= unix.ECHO
						if echo {
							savedInput.Lflag |= unix.ECHO
						}
						savedInput.Cc[unix.VERASE] = 127
						savedOutput.Oflag = unix.OPOST | unix.ONLCR | unix.TAB3
						for file, modes := range map[*os.File]*unix.Termios{input: savedInput, output: savedOutput} {
							if err := unix.IoctlSetTermios(terminalFD(file), unix.TCSETS, modes); err != nil {
								t.Fatal(err)
							}
						}
						if _, err := output.Write([]byte("xxx")); err != nil {
							t.Fatal(err)
						}
						prefix := filepath.Join(t.TempDir(), "utf8")
						self, _ := os.Executable()
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						cmd := exec.CommandContext(ctx, self, "-test.run=^TestTerminalFixture$")
						fixture := "utf8-workload"
						if guarded {
							fixture = "frontend"
						}
						cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE="+fixture, "AW_TERMINAL_UTF8="+prefix)
						cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, third
						if diagnostic == "input" {
							cmd.Stderr = input
						} else if diagnostic == "output" {
							cmd.Stderr = output
						}
						cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
						if err := cmd.Start(); err != nil {
							t.Fatal(err)
						}
						done := make(chan error, 1)
						go func() { done <- cmd.Wait(); close(done) }()
						t.Cleanup(func() {
							cancel()
							_ = cmd.Process.Kill()
							select {
							case <-done:
							case <-time.After(5 * time.Second):
								t.Error("UTF8 fixture did not join")
							}
						})
						for phase := 0; phase < 2; phase++ {
							name := prefix + strconv.Itoa(phase)
							waitTerminalFile(t, name+".ready")
							if _, err := inputMaster.Write([]byte("é\x7f\n")); err != nil {
								t.Fatal(err)
							}
							read := waitTerminalFile(t, name+".read")
							want := []byte("\xc3\n")
							if inputUTF8 {
								want = []byte("\n")
							}
							if !bytes.Equal(read, want) {
								t.Fatalf("canonical input after FD1 phase%d (guarded=%t) = %x, want %x", phase, guarded, read, want)
							}
						}
						if err := awaitTerminalDrain(t, done); err == nil || err.(*exec.ExitError).ExitCode() != 37 {
							t.Fatalf("UTF8 fixture exit = %v", err)
						}
						if guarded {
							terminalAssertModes(t, input, savedInput)
							terminalAssertModes(t, output, savedOutput)
						}
						return [3][]byte{terminalReadBuffered(t, inputMaster), terminalReadBuffered(t, outputMaster), terminalReadBuffered(t, thirdMaster)}
					}
					native, guarded := capture(false), capture(true)
					if !reflect.DeepEqual(native, guarded) {
						t.Fatalf("guardian stream bytes %x, native %x", guarded, native)
					}
				})
			}
		}
	}
}

func TestTerminalInputSinkAuthority(t *testing.T) {
	_, input, _ := terminalPair(t)
	device, err := unix.IoctlGetInt(terminalFD(input), unix.TIOCGDEV)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := os.OpenFile(input.Name(), os.O_RDONLY|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	flags, _ := unix.FcntlInt(uintptr(terminalFD(reader)), unix.F_GETFL, 0)
	alias, owned := terminalSink(device, reader, input)
	if alias != input || owned {
		t.Fatal("existing writable same-device alias was not reused")
	}
	reopened, owned := terminalSink(device, reader)
	if reopened == nil || !owned {
		t.Fatal("permitted read-only input reopen was not acquired")
	}
	reopened.Close()
	if after, _ := unix.FcntlInt(uintptr(terminalFD(reader)), unix.F_GETFL, 0); after != flags {
		t.Fatal("caller read-only descriptor flags changed")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission-denied fixture requires unprivileged caller")
	}
	if err := os.Chmod(input.Name(), 0400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(input.Name(), 0620)
	if sink, owned := terminalSink(device, reader); sink != nil || owned {
		if owned {
			sink.Close()
		}
		t.Fatal("unavailable sink gained write authority")
	}
	_, output, _ := terminalPair(t)
	cmd := exec.Command("true")
	cmd.Stdin, cmd.Stdout = reader, output
	finish, err := bridgeTerminal(context.Background(), func() {}, cmd, reader, output)
	if err != nil {
		t.Fatalf("existing admitted read-only input newly rejected: %v", err)
	}
	defer finish()
	if cmd.Run() != nil || finish() != nil {
		t.Fatal("no-sink fallback no longer launches/cleans up")
	}
	// No assertion blesses fallback UTF8/echo bytes: that class remains a
	// recorded unresolved preservation defect, not a compatibility exclusion.
}

func TestTerminalReadOnlyControllingInputSink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied fixture requires unprivileged caller")
	}
	master, input, _ := terminalPair(t)
	reader, err := os.OpenFile(input.Name(), os.O_RDONLY|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := os.Chmod(input.Name(), 0400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(input.Name(), 0620)
	if writer, err := os.OpenFile(input.Name(), os.O_WRONLY|unix.O_NOCTTY, 0); err == nil {
		writer.Close()
		t.Fatal("disposable node unexpectedly permits reopen")
	}
	self, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestTerminalFixture$")
	cmd.Env = append(os.Environ(), "AW_TERMINAL_FIXTURE=sink-ctty")
	cmd.Stdin = reader
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("read-only controlling input sink: %v, %q", err, out)
	}
	if got := terminalReadBuffered(t, master); string(got) != "CTTY-SINK" {
		t.Fatalf("matching controlling input destination = %q", got)
	}
}

func TestTerminalDirectStdoutBackpressure(t *testing.T) {
	if terminalControlFixture(t) {
		return
	}
	for _, code := range []int{0, 37, 125, -1} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			_, input, savedInput := terminalFixtureInput(t)
			master, outputTTY, savedOutput := terminalPair(t)
			// Like inherited CLI stdio, this description is blocking and was
			// not made nonblocking by os.OpenFile's internal Go poller setup.
			fd, err := unix.Open(outputTTY.Name(), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			output := os.NewFile(uintptr(fd), outputTTY.Name())
			defer output.Close()
			inputFlags, _ := unix.FcntlInt(uintptr(terminalFD(input)), unix.F_GETFL, 0)
			outputFlags, _ := unix.FcntlInt(uintptr(terminalFD(output)), unix.F_GETFL, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			payload := strings.Repeat("final\n", 2048)
			exitCode := code
			if code == -1 {
				exitCode = 37
			}
			cmd := exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$1\"; exit "+strconv.Itoa(exitCode), "fixture", payload)
			cmd.Stdin, cmd.Stdout = input, output
			finish, err := bridgeTerminal(ctx, cancel, cmd, input, output)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			if cmd.Stdout != output {
				t.Fatal("distinct stdout is not the original descriptor")
			}
			filler := terminalFillOutput(t, output)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				err := cmd.Wait()
				if cleanup := finish(); cleanup != nil {
					err = errors.Join(err, cleanup)
				}
				done <- err
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				_ = cmd.Process.Kill()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("direct stdout fixture did not join")
				}
			})
			select {
			case err := <-done:
				t.Fatalf("direct write did not apply native workload backpressure: %v", err)
			case <-time.After(1200 * time.Millisecond):
			}
			terminalAssertModes(t, output, savedOutput)
			if code == -1 {
				cancel()
				if err := awaitTerminalDrain(t, done); err == nil {
					t.Fatal("cancelled blocked workload succeeded")
				}
			} else {
				want := append(filler, []byte(strings.ReplaceAll(payload, "\n", "\r\n"))...)
				if err := master.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(want))
				if _, err := io.ReadFull(master, got); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("direct stdout lost bytes: %v", err)
				}
				err := awaitTerminalDrain(t, done)
				if code == 0 && err != nil {
					t.Fatal(err)
				} else if code != 0 {
					if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != code {
						t.Fatalf("direct exit = %v, want %d", err, code)
					}
				}
			}
			terminalAssertModes(t, input, savedInput)
			terminalAssertModes(t, output, savedOutput)
			for file, want := range map[*os.File]int{input: inputFlags, output: outputFlags} {
				if flags, _ := unix.FcntlInt(uintptr(terminalFD(file)), unix.F_GETFL, 0); flags != want {
					t.Fatal("caller descriptor flags changed")
				}
			}
		})
	}
}
