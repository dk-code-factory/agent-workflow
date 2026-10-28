//go:build linux

package main

import (
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
func prepareTerminal(cmd *exec.Cmd) (func(), error) {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		if _, err := unix.IoctlGetTermios(terminalFD(f), unix.TCGETS); err != nil {
			return func() {}, nil
		}
	}
	return bridgeTerminal(cmd, os.Stdin, os.Stdout)
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
func bridgeTerminal(cmd *exec.Cmd, input, output *os.File) (func(), error) {
	saved, err := unix.IoctlGetTermios(terminalFD(input), unix.TCGETS)
	if err != nil {
		return nil, err
	}
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
	fail := func(err error) (func(), error) {
		master.Close()
		slave.Close()
		wakeR.Close()
		wakeW.Close()
		return nil, err
	}
	// The inner terminal starts with the caller's ordinary line discipline.
	// Only the host relay runs raw so every key reaches that discipline.
	if err = unix.IoctlSetTermios(terminalFD(slave), unix.TCSETS, saved); err != nil {
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
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err = unix.IoctlSetTermios(terminalFD(input), unix.TCSETS, &raw); err != nil {
		return fail(err)
	}
	cmd.Stdin, cmd.Stdout = slave, slave
	if diagnostics, ok := cmd.Stderr.(*os.File); ok {
		if _, err := unix.IoctlGetTermios(terminalFD(diagnostics), unix.TCGETS); err == nil {
			cmd.Stderr = slave
		}
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "AGENT_WORKFLOW_NETWORK_TERMINAL=1")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	done := make(chan struct{})
	inputDone := make(chan struct{})
	outputDone := make(chan struct{})
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
	go func() { _, _ = io.Copy(output, master); close(outputDone) }()
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
	return func() {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
			<-resizeDone
			wakeW.Close()
			<-inputDone
			wakeR.Close()
			_ = unix.IoctlSetTermios(terminalFD(input), unix.TCSETS, saved)
			slave.Close()
			// Closing the final slave drains buffered output before EIO on
			// the master. Guardian cleanup has already closed remote copies.
			select {
			case <-outputDone:
			case <-time.After(time.Second):
			}
			master.Close()
		})
	}, nil
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
