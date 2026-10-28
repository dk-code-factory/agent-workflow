#!/usr/bin/env python3
"""Disposable model-free terminal proof: SCRIPT RUNTIME_BINARY RUNTIME_NAME.

Run as an already-enrolled runtime operator. It never prepares/starts/stops a
runtime, changes login, calls providers, or touches the invoking shell's tty.
Use --native-output-self-check for the owned output fixtures without a runtime.
"""
import errno
import fcntl
import hashlib
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


def wait_file(path, proc):
    deadline = time.monotonic() + 20
    while not os.path.exists(path):
        assert time.monotonic() < deadline, f'missing fixture marker {os.path.basename(path)}'
        assert proc is None or proc.poll() is None, 'fixture exited before marker'
        time.sleep(.02)


def output_fixture(mode, barrier=None):
    saved = termios.tcgetattr(1)
    try:
        if barrier:
            with open(os.path.join(barrier, 'ready'), 'wb') as marker:
                marker.write(b'ready')
            wait_file(os.path.join(barrier, 'release'), None)
        chunks = [b'first\r\n'] if mode != 'pending' else [b'final\n' * 2048]
        if mode == 'dynamic':
            chunks += [b'raw\r\n', b'processed\r\n']
        for index, chunk in enumerate(chunks):
            if index:
                attrs = termios.tcgetattr(1)
                if index == 1:
                    attrs[1] &= ~termios.OPOST
                else:
                    attrs[1] |= termios.OPOST | termios.ONLCR
                termios.tcsetattr(1, termios.TCSANOW, attrs)
            while chunk:
                chunk = chunk[os.write(1, chunk):]
        if barrier:
            with open(os.path.join(barrier, 'written'), 'wb') as marker:
                marker.write(b'written')
    finally:
        termios.tcsetattr(1, termios.TCSANOW, saved)
    sys.exit(37)


def stop_child(proc):
    if proc is not None and proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)


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
        stop_child(self.proc)
        os.close(self.master)
        os.close(self.slave)
        if self.diagnostics:
            self.diagnostics.close()
        if self.diagnostic_pty:
            for fd in self.diagnostic_pty:
                os.close(fd)


class OutputCaller:
    """A native or complete runtime exec with independently owned terminal FDs."""
    def __init__(self, binary, runtime, profile, mode, barrier=None):
        self.fds = []
        self.proc = None
        self.diagnostics = tempfile.TemporaryFile()
        try:
            _, same, input_flags, output_flags = profile
            self.master, self.output = pty.openpty()
            self.fds += [self.master, self.output]
            self.input = self.output
            if not same:
                input_master, self.input = pty.openpty()
                self.fds += [input_master, self.input]
            for fd, flags in [(self.input, input_flags), (self.output, output_flags)]:
                attrs = termios.tcgetattr(fd)
                attrs[1] = flags
                termios.tcsetattr(fd, termios.TCSANOW, attrs)
            self.saved_input = termios.tcgetattr(self.input)
            self.saved_output = termios.tcgetattr(self.output)
            command = [sys.executable, os.path.abspath(__file__), '--output-fixture', mode]
            if barrier:
                command.append(barrier)
            if binary:
                command = [binary, 'exec', runtime, '--'] + command

            def own_disposable_tty():
                os.setsid()
                fcntl.ioctl(0, termios.TIOCSCTTY, 0)

            self.proc = subprocess.Popen(command, stdin=self.input, stdout=self.output,
                                         stderr=self.diagnostics, preexec_fn=own_disposable_tty)
        except BaseException:
            self.close()
            raise

    def capture(self):
        deadline = time.monotonic() + 20
        data = b''
        while True:
            assert time.monotonic() < deadline, 'output fixture did not complete'
            if select.select([self.master], [], [], .05)[0]:
                chunk = os.read(self.master, 65536)
                assert chunk, 'output terminal closed before completion'
                data += chunk
                assert len(data) < 1024 * 1024, 'unexpected fixture output volume'
            elif self.proc.poll() is not None:
                break
        actual = self.proc.wait(timeout=20)
        self.diagnostics.seek(0)
        diagnostic = self.diagnostics.read(256)
        assert actual == 37, (actual, diagnostic)
        assert termios.tcgetattr(self.input) == self.saved_input, 'input modes changed'
        assert termios.tcgetattr(self.output) == self.saved_output, 'output modes changed'
        return data

    def close(self):
        try:
            stop_child(self.proc)
        finally:
            for fd in self.fds:
                os.close(fd)
            self.fds = []
            self.diagnostics.close()


def capture_output(binary, runtime, profile, mode):
    caller = OutputCaller(binary, runtime, profile, mode)
    try:
        return caller.capture()
    finally:
        caller.close()


def fill_output(output):
    # A new file description changes no flags on the caller's stdout FD.
    fd = os.open(os.ttyname(output), os.O_WRONLY | os.O_NOCTTY | os.O_NONBLOCK)
    filled = b''
    try:
        while len(filled) < 1024 * 1024:
            try:
                filled += b'.' * os.write(fd, b'.' * 1024)
            except BlockingIOError:
                if not select.select([], [fd], [], .05)[1]:
                    return filled
        raise AssertionError('owned PTY did not apply bounded backpressure')
    finally:
        os.close(fd)


def backpressured_output(binary, runtime, profile, native):
    with tempfile.TemporaryDirectory(prefix='aw-terminal-output-') as barrier:
        caller = OutputCaller(binary, runtime, profile, 'pending', barrier)
        try:
            wait_file(os.path.join(barrier, 'ready'), caller.proc)
            prefix = fill_output(caller.output)
            with open(os.path.join(barrier, 'release'), 'wb') as marker:
                marker.write(b'release')
            if binary:
                # The private PTY accepted the entire final payload. Wait for
                # restored caller keyboard controls while its relay is blocked.
                wait_file(os.path.join(barrier, 'written'), caller.proc)
            deadline = time.monotonic() + 20
            while True:
                attrs = termios.tcgetattr(caller.input)
                attrs[1] = caller.saved_input[1]
                if attrs == caller.saved_input:
                    break
                assert time.monotonic() < deadline, 'input controls not restored during drain'
                assert caller.proc.poll() is None, 'frontend exited before output drain'
                time.sleep(.02)
            if binary:
                assert not termios.tcgetattr(caller.output)[1] & termios.OPOST, 'outer OPOST enabled during drain'
            time.sleep(1.2)
            assert caller.proc.poll() is None, 'frontend completed during consumer pause'
            if binary:
                assert not termios.tcgetattr(caller.output)[1] & termios.OPOST, 'outer OPOST enabled before resumed drain'
            data = caller.capture()
            wait_file(os.path.join(barrier, 'written'), None)
            assert data == prefix + native, (len(data), len(prefix) + len(native), data[-32:].hex())
            return dict(consumerPauseSeconds=1.2, nativePayloadBytes=len(native),
                        nativePayloadSha256=hashlib.sha256(native).hexdigest(),
                        exactBytes=True, exitStatus=37, callerTermiosRestored=True,
                        earlyInputControlsRestored=True,
                        outerOpostHeldOffDuringDrain=bool(binary))
        finally:
            caller.close()


def output_acceptance(binary=None, runtime=None):
    processed = termios.OPOST | termios.ONLCR
    profiles = [('shared-processed', True, processed, processed),
                ('shared-raw', True, termios.ONLCR, termios.ONLCR),
                ('distinct-processed', False, processed, processed),
                ('distinct-raw-output', False, processed, termios.ONLCR),
                ('distinct-processed-output', False, termios.ONLCR, processed | termios.OCRNL)]
    rows = []
    for profile in profiles:
        for mode in ['static', 'dynamic']:
            native = capture_output(None, None, profile, mode)
            actual = capture_output(binary, runtime, profile, mode) if binary else native
            assert actual == native, (profile[0], mode, actual.hex(), native.hex())
            rows.append(dict(case=profile[0], mode=mode, nativeHex=native.hex(),
                             actualHex=actual.hex(), exactBytes=True, exitStatus=37,
                             callerTermiosRestored=True))
    native = capture_output(None, None, profiles[0], 'pending')
    return dict(outputOracleUsesRuntime=bool(binary), outputByteOracle=rows,
                backpressuredFinalNewline=backpressured_output(binary, runtime, profiles[0], native))


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
        summary = dict(separateStderrPtyPreserved=True, controllingTerminal=True, foregroundGroup=True, byteInput=True, resize=True, exitStatus=37, ctrlC=130, sigterm=True, detachedDescendantCleanup=True, independentCallerSurvived=True, callerTermiosRestored=True, redirectedStderrPreserved=True)
        summary.update(output_acceptance(binary, runtime))
        print(json.dumps(summary))
    finally:
        for caller in callers:
            caller.close()


if __name__ == '__main__':
    if sys.argv[1:] == ['--fixture']:
        fixture()
    elif len(sys.argv) in [3, 4] and sys.argv[1] == '--output-fixture':
        output_fixture(sys.argv[2], sys.argv[3] if len(sys.argv) == 4 else None)
    elif sys.argv[1:] == ['--native-output-self-check']:
        print(json.dumps(output_acceptance()))
    elif len(sys.argv) == 3:
        acceptance(os.path.abspath(sys.argv[1]), sys.argv[2])
    else:
        sys.exit(__doc__)
