//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// prepareTerminal only adapts an interactive standalone launch. Pipes retain
// their original descriptors and stream separation.
func prepareTerminal(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd) (func() error, error) {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		if _, err := unix.IoctlGetTermios(terminalFD(f), unix.TCGETS); err != nil {
			return func() error { return nil }, nil
		}
	}
	return bridgeTerminal(ctx, cancel, cmd, os.Stdin, os.Stdout)
}

func openTerminal() (*os.File, *os.File, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	master := os.NewFile(uintptr(fd), "invocation-terminal")
	fail := func(err error) (*os.File, *os.File, error) { master.Close(); return nil, nil, err }
	if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return fail(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return fail(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	return master, slave, nil
}

// bridgeTerminal owns only its new PTY. The caller's saved termios is restored
// before returning to the shell, including launch errors and handled signals.
func bridgeTerminal(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, input, output *os.File) (func() error, error) {
	saved, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
	if err != nil {
		return nil, err
	}
	savedOutput, outputErr := unix.IoctlGetTermios(terminalFD(output), unix.TCGETS)
	outputDevice, outputDeviceErr := unix.IoctlGetInt(terminalFD(output), unix.TIOCGDEV)
	inputDevice, inputDeviceErr := unix.IoctlGetInt(terminalFD(input), unix.TIOCGDEV)
	distinctOutput := outputDeviceErr == nil && inputDeviceErr == nil && outputDevice != inputDevice
	master, slave, err := openTerminal()
	if err != nil {
		return nil, err
	}
	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	fail := func(err error) (func() error, error) {
		master.Close()
		slave.Close()
		wakeR.Close()
		wakeW.Close()
		return nil, err
	}
	// Input discipline comes from stdin, output processing from stdout. The
	// workload may change its private terminal's modes while it runs.
	inner := *saved
	if outputErr == nil {
		inner.Oflag = savedOutput.Oflag
	}
	if err = unix.IoctlSetTermios(terminalFD(slave), unix.TCSETS, &inner); err != nil {
		return fail(err)
	}
	resize := func() {
		if size, e := unix.IoctlGetWinsize(terminalFD(input), unix.TIOCGWINSZ); e == nil {
			_ = unix.IoctlSetWinsize(terminalFD(master), unix.TIOCSWINSZ, size)
		}
	}
	resize()
	raw := *saved
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	restoreModes := func() error {
		var restoreErr error
		if err := unix.IoctlSetTermios(terminalFD(input), unix.TCSETS, saved); err != nil {
			restoreErr = fmt.Errorf("restore caller input terminal: %w", err)
		}
		if outputErr == nil {
			if err := unix.IoctlSetTermios(terminalFD(output), unix.TCSETS, savedOutput); err != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("restore caller output terminal: %w", err))
			}
		}
		return restoreErr
	}
	rollback := func(err error) (func() error, error) {
		return fail(errors.Join(err, restoreModes()))
	}
	if err = unix.IoctlSetTermios(terminalFD(input), unix.TCSETS, &raw); err != nil {
		return rollback(err)
	}
	if outputErr == nil {
		// Read after setting input raw: stdin and stdout may be aliases of
		// one terminal. Disable only outer output processing in either case.
		relayOutput, err := unix.IoctlGetTermios(terminalFD(output), unix.TCGETS)
		if err != nil {
			return rollback(err)
		}
		relayOutput.Oflag &^= unix.OPOST
		if err := unix.IoctlSetTermios(terminalFD(output), unix.TCSETS, relayOutput); err != nil {
			return rollback(err)
		}
	}
	// Only diagnostics targeting the original stdout terminal share the new
	// PTY. A different terminal is an explicit stream redirection.
	cmd.Stdin, cmd.Stdout = slave, slave
	if diagnostics, ok := cmd.Stderr.(*os.File); ok && outputDeviceErr == nil {
		diagnosticDevice, err := unix.IoctlGetInt(terminalFD(diagnostics), unix.TIOCGDEV)
		if err == nil && diagnosticDevice == outputDevice {
			cmd.Stderr = slave
		}
	}
	cmd.Env = append(cmd.Environ(), "AGENT_WORKFLOW_NETWORK_TERMINAL=1")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	done := make(chan struct{})
	inputDone := make(chan struct{})
	outputDone := make(chan error, 1)
	resizeDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		fds := []unix.PollFd{{Fd: int32(terminalFD(input)), Events: unix.POLLIN}, {Fd: int32(terminalFD(wakeR)), Events: unix.POLLIN}}
		buf := make([]byte, 4096)
		for {
			if _, e := unix.Poll(fds, -1); e == unix.EINTR {
				continue
			} else if e != nil || fds[1].Revents != 0 || fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				return
			}
			if fds[0].Revents&unix.POLLIN != 0 {
				n, e := unix.Read(terminalFD(input), buf)
				if e != nil || n == 0 {
					return
				}
				if e = terminalInput(master, wakeR, buf[:n]); e != nil {
					return
				}
			}
		}
	}()
	go func() {
		_, err := io.Copy(output, terminalOutput{master})
		outputDone <- err
		if err != nil {
			cancel()
		}
	}()
	go func() {
		defer close(resizeDone)
		for {
			select {
			case <-signals:
				resize()
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	var restoreErr error
	return func() error {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
			<-resizeDone
			wakeW.Close()
			<-inputDone
			wakeR.Close()
			// Restore caller keyboard controls before a potentially blocked
			// drain. Shared output must still bypass outer processing; only
			// proved distinct input keeps its original output modes now.
			inputModes := *saved
			if outputErr == nil && !distinctOutput {
				inputModes.Oflag &^= unix.OPOST
			}
			if err := unix.IoctlSetTermios(terminalFD(input), unix.TCSETS, &inputModes); err != nil {
				restoreErr = fmt.Errorf("restore caller input terminal: %w", err)
			}
			slave.Close()
			// Closing the final slave drains buffered output before EIO on
			// the master. Guardian cleanup has already closed remote copies.
			var drainErr error
			select {
			case drainErr = <-outputDone:
			case <-ctx.Done():
				// Allow a responsive consumer to finish even when cancellation
				// preceded the final EOF. Only cancellation bounds this wait;
				// ordinary output backpressure must never truncate success.
				timer := time.NewTimer(time.Second)
				defer timer.Stop()
				select {
				case drainErr = <-outputDone:
				case <-timer.C:
					// Prefer a completed drain at the deadline, too. Closing
					// master cannot interrupt a caller writer.
					select {
					case drainErr = <-outputDone:
					default:
						drainErr = ctx.Err()
					}
				}
			}
			if drainErr != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("drain terminal output: %w", drainErr))
			}
			master.Close()
			restoreErr = errors.Join(restoreErr, restoreModes())
		})
		return restoreErr
	}, nil
}

// Linux PTYs report EIO when their final slave closes. Normalize only reads:
// an EIO from the caller output is a failed drain, not a terminal EOF.
type terminalOutput struct{ file *os.File }

func (r terminalOutput) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	if errors.Is(err, unix.EIO) {
		err = io.EOF
	}
	return n, err
}

func terminalInput(master, wake *os.File, data []byte) error {
	fds := []unix.PollFd{{Fd: int32(terminalFD(master)), Events: unix.POLLOUT}, {Fd: int32(terminalFD(wake)), Events: unix.POLLIN}}
	for len(data) != 0 {
		if _, err := unix.Poll(fds, -1); err == unix.EINTR {
			continue
		} else if err != nil {
			return err
		}
		if fds[1].Revents != 0 || fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return io.EOF
		}
		n, err := unix.Write(terminalFD(master), data)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// SyscallConn avoids File.Fd switching the PTY into blocking mode, which would
// prevent lifetime cancellation while a workload stops consuming input.
func terminalFD(f *os.File) int {
	fd := -1
	if raw, err := f.SyscallConn(); err == nil {
		_ = raw.Control(func(value uintptr) { fd = int(value) })
	}
	return fd
}
