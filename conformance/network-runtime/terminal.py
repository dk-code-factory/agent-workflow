#!/usr/bin/env python3
"""Disposable model-free terminal proof: SCRIPT RUNTIME_BINARY RUNTIME_NAME.

Run as an already-enrolled runtime operator. It never prepares/starts/stops a
runtime, changes login, calls providers, or touches the invoking shell's tty.
"""
import errno
import fcntl
import json
import os
import pty
import select
import signal
import struct
import subprocess
import sys
import termios
import tempfile
import time


def fixture():
    tty = os.open('/dev/tty', os.O_RDWR)
    assert os.tcgetpgrp(tty) == os.getpgrp(), 'workload lacks foreground terminal'
    attrs = termios.tcgetattr(tty)
    attrs[3] &= ~(termios.ICANON | termios.ECHO)
    attrs[6][termios.VMIN] = 1
    attrs[6][termios.VTIME] = 0
    termios.tcsetattr(tty, termios.TCSANOW, attrs)
    child = subprocess.Popen(['/usr/bin/setsid', '/usr/bin/sleep', '300'],
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL)

    def resized(*_):
        rows, cols, _, _ = struct.unpack('HHHH', fcntl.ioctl(tty, termios.TIOCGWINSZ, b'\0' * 8))
        print('SIZE', rows, cols, flush=True)

    def interrupted(*_):
        print('INTERRUPTED', flush=True)
        sys.exit(130)

    signal.signal(signal.SIGWINCH, resized)
    signal.signal(signal.SIGINT, interrupted)
    print('DIAGNOSTIC', file=sys.stderr, flush=True)
    print('READY', child.pid, flush=True)
    resized()
    while True:
        key = os.read(tty, 1)
        if key == b'q':
            print('BYE', flush=True)
            sys.exit(37)
        print('KEY', key.hex(), flush=True)


class Caller:
    def __init__(self, binary, runtime, redirected_stderr=False):
        self.master, self.slave = pty.openpty()
        self.saved = termios.tcgetattr(self.slave)
        self.diagnostic_pty = pty.openpty() if redirected_stderr == "pty" else None
        self.diagnostics = tempfile.TemporaryFile() if redirected_stderr is True else None
        fcntl.ioctl(self.slave, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 80, 0, 0))

        def own_disposable_tty():
            os.setsid()
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)

        self.proc = subprocess.Popen([binary, 'exec', runtime, '--', sys.executable,
                                      os.path.abspath(__file__), '--fixture'],
                                     stdin=self.slave, stdout=self.slave,
                                     stderr=(self.diagnostic_pty[1] if self.diagnostic_pty else
                                             self.diagnostics if self.diagnostics else self.slave),
                                     preexec_fn=own_disposable_tty)
        self.buffer = b''
        try:
            self.expect(b'READY ')
            self.expect(b'SIZE 24 80')
            ready = self.buffer.split(b'READY ', 1)[1].splitlines()[0]
            self.descendant = int(ready.strip())
        except BaseException:
            self.close()
            raise

    def expect(self, text):
        deadline = time.monotonic() + 20
        while text not in self.buffer:
            if time.monotonic() > deadline:
                raise AssertionError(f'missing {text!r}: {self.buffer!r}')
            if select.select([self.master], [], [], .1)[0]:
                try:
                    self.buffer += os.read(self.master, 65536)
                except OSError as error:
                    raise AssertionError(f'terminal read failed: {self.buffer!r}') from error

    def finish(self, code=None):
        actual = self.proc.wait(timeout=20)
        assert actual == code if code is not None else actual != 0, (actual, code, self.buffer)
        assert termios.tcgetattr(self.slave) == self.saved, 'caller termios changed'
        if self.diagnostics:
            self.diagnostics.seek(0)
            assert self.diagnostics.read() == b'DIAGNOSTIC\n', 'redirected stderr was not preserved'
            assert b'DIAGNOSTIC' not in self.buffer, 'redirected diagnostics leaked to terminal'
        if self.diagnostic_pty:
            diagnostic = b''
            deadline = time.monotonic() + 5
            while b'DIAGNOSTIC\r\n' not in diagnostic and time.monotonic() < deadline:
                if select.select([self.diagnostic_pty[0]], [], [], .1)[0]:
                    diagnostic += os.read(self.diagnostic_pty[0], 65536)
            assert diagnostic == b'DIAGNOSTIC\r\n', diagnostic
            assert b'DIAGNOSTIC' not in self.buffer, 'separate stderr PTY merged into stdout'
        try:
            os.kill(self.descendant, 0)
        except OSError as error:
            assert error.errno == errno.ESRCH
        else:
            raise AssertionError(f'descendant {self.descendant} survived')

    def close(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            self.proc.wait(timeout=20)
        os.close(self.master)
        os.close(self.slave)
        if self.diagnostics:
            self.diagnostics.close()
        if self.diagnostic_pty:
            for fd in self.diagnostic_pty:
                os.close(fd)


def acceptance(binary, runtime):
    callers = []
    try:
        a = Caller(binary, runtime); callers.append(a)
        b = Caller(binary, runtime); callers.append(b)
        os.write(a.master, b'a')
        a.expect(b'KEY 61')
        fcntl.ioctl(a.slave, termios.TIOCSWINSZ, struct.pack('HHHH', 41, 103, 0, 0))
        a.proc.send_signal(signal.SIGWINCH)
        a.expect(b'SIZE 41 103')
        os.write(a.master, b'q')
        a.expect(b'BYE')
        a.finish(37)
        os.write(b.master, b'b')
        b.expect(b'KEY 62')
        c = Caller(binary, runtime); callers.append(c)
        os.write(b.master, b'\x03')
        b.expect(b'INTERRUPTED')
        b.finish(130)
        os.write(c.master, b'c')
        c.expect(b'KEY 63')
        d = Caller(binary, runtime); callers.append(d)
        c.proc.terminate()
        c.finish()
        os.write(d.master, b'd')
        d.expect(b'KEY 64')
        os.write(d.master, b'q')
        d.expect(b'BYE')
        d.finish(37)
        e = Caller(binary, runtime, redirected_stderr=True); callers.append(e)
        os.write(e.master, b'e')
        e.expect(b'KEY 65')
        fcntl.ioctl(e.slave, termios.TIOCSWINSZ, struct.pack('HHHH', 39, 107, 0, 0))
        e.proc.send_signal(signal.SIGWINCH)
        e.expect(b'SIZE 39 107')
        os.write(e.master, b'q')
        e.expect(b'BYE')
        e.finish(37)
        f = Caller(binary, runtime, redirected_stderr="pty"); callers.append(f)
        os.write(f.master, b'q')
        f.expect(b'BYE')
        f.finish(37)
        print(json.dumps(dict(separateStderrPtyPreserved=True, controllingTerminal=True, foregroundGroup=True, byteInput=True, resize=True, exitStatus=37, ctrlC=130, sigterm=True, detachedDescendantCleanup=True, independentCallerSurvived=True, callerTermiosRestored=True, redirectedStderrPreserved=True)))
    finally:
        for caller in callers:
            caller.close()


if __name__ == '__main__':
    if sys.argv[1:] == ['--fixture']:
        fixture()
    elif len(sys.argv) == 3:
        acceptance(os.path.abspath(sys.argv[1]), sys.argv[2])
    else:
        sys.exit(__doc__)
