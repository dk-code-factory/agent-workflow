import ctypes.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import unittest


WRAPPER = Path(__file__).with_name("codex_offline_exec.py").resolve()


@unittest.skipUnless(sys.platform == "linux" and ctypes.util.find_library("seccomp"), "Linux libseccomp prerequisite")
class OfflineCompilerTests(unittest.TestCase):
    def run_offline(self, root, command):
        receipt = root / "filter.json"
        result = subprocess.run([sys.executable, str(WRAPPER), "--receipt", str(receipt), "--", *command], env={"PATH": os.environ["PATH"], "HOME": str(root), "TMPDIR": str(root), "UNRELATED_SECRET": "must-not-reach-child"}, cwd=root, capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads(receipt.read_text())
        self.assertTrue(evidence["installed"])
        self.assertEqual(len(evidence["bpf_sha256"]), 64)
        return result

    def test_inherited_internet_descriptor_is_closed_before_exec(self):
        with socket.socket() as listener, tempfile.TemporaryDirectory() as directory:
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            with socket.create_connection(listener.getsockname()) as outgoing, listener.accept()[0] as incoming:
                root = Path(directory)
                code = "import errno,os,sys\ntry: os.write(int(sys.argv[1]),b'leaked')\nexcept OSError as error: assert error.errno == errno.EBADF\nelse: raise AssertionError('inherited Internet descriptor remains usable')"
                result = subprocess.run([sys.executable, str(WRAPPER), "--receipt", str(root / "filter.json"), "--", sys.executable, "-c", code, str(outgoing.fileno())], pass_fds=(outgoing.fileno(),), capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                incoming.settimeout(0.1)
                with self.assertRaises(TimeoutError):
                    incoming.recv(32)

    def test_internet_standard_stream_is_rejected(self):
        with socket.socket() as listener, tempfile.TemporaryDirectory() as directory:
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            with socket.create_connection(listener.getsockname()) as outgoing, listener.accept()[0] as incoming:
                receipt = Path(directory) / "filter.json"
                result = subprocess.run([sys.executable, str(WRAPPER), "--receipt", str(receipt), "--", sys.executable, "-c", "import os; os.write(1,b'leaked')"], stdout=outgoing, stderr=subprocess.PIPE, text=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("cannot inherit Internet standard streams", result.stderr)
                self.assertFalse(receipt.exists())
                incoming.settimeout(0.1)
                with self.assertRaises(TimeoutError):
                    incoming.recv(32)

    def test_unix_standard_stream_remains_usable(self):
        with tempfile.TemporaryDirectory() as directory:
            left, right = socket.socketpair()
            with left, right:
                receipt = Path(directory) / "filter.json"
                result = subprocess.run([sys.executable, str(WRAPPER), "--receipt", str(receipt), "--", sys.executable, "-c", "import os; os.write(1,b'unix-ipc')"], stdout=left, stderr=subprocess.PIPE, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                right.settimeout(1)
                self.assertEqual(right.recv(32), b"unix-ipc")

    def test_forced_download_and_child_network_are_denied_unix_ipc_works(self):
        requests = []

        class Gateway(BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"unexpected network access")

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                code = """import errno,os,socket,subprocess,sys,urllib.request
assert 'UNRELATED_SECRET' not in os.environ
for family in (socket.AF_INET, socket.AF_INET6):
    try:
        socket.socket(family, socket.SOCK_STREAM)
    except PermissionError as error:
        assert error.errno == errno.EPERM
    else:
        raise AssertionError('network socket was allowed')
try:
    urllib.request.urlopen(sys.argv[1], timeout=1)
except urllib.error.URLError as error:
    assert error.reason.errno == errno.EPERM
else:
    raise AssertionError('download was allowed')
left,right=socket.socketpair()
left.send(b'pipe'); assert right.recv(4)==b'pipe'
left.close(); right.close()
child=subprocess.run([sys.executable,'-c','import socket; socket.socket(socket.AF_INET)'],capture_output=True,text=True)
assert child.returncode != 0 and 'Operation not permitted' in child.stderr
"""
                self.run_offline(root, [sys.executable, "-c", code, f"http://127.0.0.1:{server.server_port}/forced-native-archive"])
                self.assertEqual(requests, [])
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)

    @unittest.skipUnless(shutil.which("cc"), "local compiler prerequisite")
    def test_ordinary_compiler_child_and_linker_succeed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, output = root / "fixture.c", root / "fixture"
            source.write_text("int main(void) { return 0; }\n")
            code = "import subprocess,sys; subprocess.run(sys.argv[1:],check=True)"
            self.run_offline(root, [sys.executable, "-c", code, shutil.which("cc"), str(source), "-o", str(output)])
            self.assertTrue(output.is_file())


if __name__ == "__main__":
    unittest.main()
