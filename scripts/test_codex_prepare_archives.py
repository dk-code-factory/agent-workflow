import argparse
import hashlib
from http.client import HTTPConnection, HTTPResponse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
import multiprocessing
import os
from pathlib import Path
import signal
import tempfile
import threading
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location("archives", Path(__file__).with_name("codex_prepare_archives.py"))
ARCHIVES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ARCHIVES)


class PinnedArchiveTests(unittest.TestCase):
    def test_wrong_hash_and_oversized_archive_never_install(self):
        for limit, expected in [(100, "0" * 64), (2, hashlib.sha256(b"fixture").hexdigest())]:
            with self.subTest(limit=limit), tempfile.TemporaryDirectory() as directory:
                destination = Path(directory) / "fixture.crate"
                with patch.object(ARCHIVES, "MAX_ARCHIVE", limit), self.assertRaises(ValueError):
                    ARCHIVES.save_archive(io.BytesIO(b"fixture"), destination, expected, lambda *_, **__: None)
                self.assertEqual(list(Path(directory).iterdir()), [])

    def test_only_original_hash_is_installed(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "fixture.crate"
            size = ARCHIVES.save_archive(io.BytesIO(b"fixture"), destination, hashlib.sha256(b"fixture").hexdigest(), lambda *_, **__: None)
            self.assertEqual(size, 7)
            self.assertEqual(destination.read_bytes(), b"fixture")

    def test_cross_host_or_credential_redirect_is_rejected(self):
        redirects = []
        handler = ARCHIVES.Redirects(redirects)
        for url in ["http://static.crates.io/a", "https://example.com/a", "https://user@static.crates.io/a", "https://static.crates.io:444/a", "https://static.crates.io/a?token=x"]:
            with self.subTest(url=url), self.assertRaises(ValueError):
                handler.redirect_request(None, None, 302, "", {}, url)
        self.assertEqual(redirects, [])

    def test_redirect_bodies_are_closed_without_being_buffered(self):
        for code in (301, 302, 303, 307, 308):
            with self.subTest(code=code):
                wire = io.BytesIO(b"HTTP/1.1 302 Found\r\nContent-Length: 1073741824\r\nLocation: https://static.crates.io/crates/fixture/final.crate\r\n\r\n")
                transport = Mock()
                transport.makefile.return_value = wire
                response = HTTPResponse(transport)
                response.begin()
                request = ARCHIVES.urllib.request.Request("https://static.crates.io/crates/fixture/start.crate")
                request.timeout = 1
                redirects = []
                handler = ARCHIVES.Redirects(redirects)
                handler.parent = Mock()
                with patch.object(wire, "read", side_effect=AssertionError("redirect body was buffered")):
                    result = getattr(handler, "http_error_" + str(code))(request, response, code, "Redirect", response.headers)
                self.assertIs(result, handler.parent.open.return_value)
                self.assertTrue(wire.closed)
                self.assertEqual(redirects[0]["status"], code)

    def fixture(self, root):
        prepared, origin = root / "prepared", root / "original"
        archives = prepared / "cargo-home/registry/cache" / ARCHIVES.REGISTRY
        archives.mkdir(parents=True)
        source = prepared / "source/codex-rs"
        source.mkdir(parents=True)
        checksum = hashlib.sha256(b"fixture").hexdigest()
        (source / "Cargo.lock").write_text('\n[[package]]\nname = "fixture"\nversion = "1.0.0"\nsource = "' + ARCHIVES.SOURCE + '"\nchecksum = "' + checksum + '"\n')
        (prepared / "cache.json").write_text(json.dumps({"missing": ["archive:fixture-1.0.0.crate"]}))
        cached = origin / "registry/cache" / ARCHIVES.REGISTRY
        cached.mkdir(parents=True)
        (cached / "fixture-1.0.0.crate").write_bytes(b"fixture")
        return argparse.Namespace(output=prepared, from_cache=origin, receipt="archives.json"), archives

    def test_free_space_floor_is_reserved_before_archive_write(self):
        with tempfile.TemporaryDirectory() as directory:
            args, archives = self.fixture(Path(directory))
            lock_hash = ARCHIVES.digest(args.output / "source/codex-rs/Cargo.lock")
            scarce = type("Space", (), {"free": 30})()
            ample = type("Space", (), {"free": 24 + 2 * os.statvfs(args.output).f_frsize})()
            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 24), patch.object(ARCHIVES.shutil, "disk_usage", side_effect=[scarce, scarce, ample, ample]), self.assertRaisesRegex(ValueError, "resource limit"):
                ARCHIVES.prepare(args)
            self.assertEqual(list(archives.iterdir()), [])
            report = json.loads((args.output / args.receipt).read_text())
            self.assertEqual(report["exit_code"], 1)
            self.assertEqual(report["prepared_bytes"], 0)

    def test_archive_floor_checks_its_filesystem_separately_from_receipts(self):
        with tempfile.TemporaryDirectory() as directory:
            args, archives = self.fixture(Path(directory))
            lock_hash = ARCHIVES.digest(args.output / "source/codex-rs/Cargo.lock")
            checked = []

            def free_space(path):
                checked.append(path)
                return type("Space", (), {"free": 24 if path == archives else 24 + 2 * os.statvfs(path).f_frsize})()

            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 24), patch.object(ARCHIVES.shutil, "disk_usage", side_effect=free_space), self.assertRaisesRegex(ValueError, "resource limit"):
                ARCHIVES.prepare(args)
            self.assertIn(archives, checked)
            self.assertIn(args.output, checked)
            self.assertEqual(list(archives.iterdir()), [])
            self.assertEqual(json.loads((args.output / args.receipt).read_text())["exit_code"], 1)

    @unittest.skipUnless(hasattr(os, "mkfifo"), "POSIX FIFO prerequisite")
    def test_total_deadline_covers_blocking_preflight_inputs(self):
        for relative in ("source/codex-rs/Cargo.lock", "cache.json"):
            with self.subTest(relative=relative), tempfile.TemporaryDirectory() as directory:
                args, archives = self.fixture(Path(directory))
                lock_hash = ARCHIVES.digest(args.output / "source/codex-rs/Cargo.lock")
                path = args.output / relative
                path.unlink()
                os.mkfifo(path)
                with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MAX_SECONDS", 0.1), self.assertRaisesRegex(TimeoutError, "preparation deadline exceeded"):
                    ARCHIVES.prepare(args)
                self.assertEqual(list(archives.iterdir()), [])

    @unittest.skipUnless(hasattr(os, "mkfifo"), "POSIX FIFO prerequisite")
    def test_total_deadline_covers_final_lock_validation(self):
        with tempfile.TemporaryDirectory() as directory:
            args, _ = self.fixture(Path(directory))
            lock = args.output / "source/codex-rs/Cargo.lock"
            lock_hash = ARCHIVES.digest(lock)
            save = ARCHIVES.save_archive

            def changed_lock(*params):
                size = save(*params)
                lock.unlink()
                os.mkfifo(lock)
                return size

            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 0), patch.object(ARCHIVES, "MAX_SECONDS", 0.1), patch.object(ARCHIVES, "save_archive", side_effect=changed_lock), self.assertRaisesRegex(TimeoutError, "preparation deadline exceeded"):
                ARCHIVES.prepare(args)

    @unittest.skipUnless("fork" in multiprocessing.get_all_start_methods(), "POSIX fork prerequisite")
    def test_expired_deadline_does_not_reopen_a_replaced_fifo_lock(self):
        with tempfile.TemporaryDirectory() as directory:
            args, archives = self.fixture(Path(directory))
            lock = args.output / "source/codex-rs/Cargo.lock"
            lock_hash = ARCHIVES.digest(lock)

            def prepare_until_timeout():
                def wait_for_deadline(*_):
                    lock.unlink()
                    os.mkfifo(lock)
                    signal.pause()

                with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 0), patch.object(ARCHIVES, "MAX_SECONDS", 0.1), patch.object(ARCHIVES, "save_archive", side_effect=wait_for_deadline):
                    try:
                        ARCHIVES.prepare(args)
                    except TimeoutError:
                        return
                    raise AssertionError("preparation did not expire")

            process = multiprocessing.get_context("fork").Process(target=prepare_until_timeout)
            process.start()
            try:
                process.join(timeout=2)
                self.assertFalse(process.is_alive(), "expired preparation blocked during final lock validation")
                self.assertEqual(process.exitcode, 0)
            finally:
                if process.is_alive():
                    process.kill()
                process.join(timeout=2)
                process.close()
            self.assertEqual(list(archives.iterdir()), [])
            report = json.loads((args.output / args.receipt).read_text())
            self.assertEqual(report["exit_code"], 1)
            self.assertIn("deadline exceeded", report["error"])
            self.assertIsNone(report["final_lock_sha256"])

    def test_parsed_packages_are_bound_to_the_approved_lock_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            args, archives = self.fixture(Path(directory))
            lock = args.output / "source/codex-rs/Cargo.lock"
            lock_hash = ARCHIVES.digest(lock)
            replacement = b"unapproved archive"
            alternate = lock.read_text().replace(hashlib.sha256(b"fixture").hexdigest(), hashlib.sha256(replacement).hexdigest())
            (args.from_cache / "registry/cache" / ARCHIVES.REGISTRY / "fixture-1.0.0.crate").write_bytes(replacement)
            read_text = Path.read_text

            def inconsistent_read(path, *params, **options):
                return alternate if path == lock else read_text(path, *params, **options)

            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 0), patch.object(Path, "read_text", inconsistent_read), self.assertRaisesRegex(ValueError, "original locked SHA-256"):
                ARCHIVES.prepare(args)
            self.assertEqual(list(archives.iterdir()), [])
            report = json.loads((args.output / args.receipt).read_text())
            self.assertEqual(report["exit_code"], 1)
            self.assertEqual(report["original_lock_sha256"], report["final_lock_sha256"])

    def test_lock_snapshot_rejects_oversized_input_before_archive_work(self):
        with tempfile.TemporaryDirectory() as directory:
            args, archives = self.fixture(Path(directory))
            lock = args.output / "source/codex-rs/Cargo.lock"
            content = lock.read_bytes()
            stream = io.BytesIO(content)
            read = stream.read

            def bounded_read(size=-1):
                self.assertEqual(size, 33)
                return read(size)

            with patch.object(ARCHIVES, "MAX_LOCK", 32), patch.object(Path, "open", return_value=stream), patch.object(stream, "read", side_effect=bounded_read), self.assertRaisesRegex(ValueError, "source lock exceeds its byte limit"):
                ARCHIVES.prepare(args)
            self.assertEqual(list(archives.iterdir()), [])
            self.assertFalse((args.output / args.receipt).exists())

    def test_space_reservations_cover_large_filesystem_blocks(self):
        for block in (4096, 16384, 65536):
            with self.subTest(block=block), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                metadata = type("Filesystem", (), {"f_frsize": block})()
                with patch.object(ARCHIVES.os, "statvfs", return_value=metadata):
                    self.test_free_space_floor_is_reserved_before_archive_write()
                    self.test_archive_floor_checks_its_filesystem_separately_from_receipts()
                    self.test_receipt_cannot_spend_the_floor_remaining_after_an_archive()
                    space = type("Space", (), {"free": 24 + block - 1})()
                    with patch.object(ARCHIVES, "MIN_FREE", 24), patch.object(ARCHIVES.shutil, "disk_usage", return_value=space), self.assertRaisesRegex(ValueError, "resource limit"):
                        ARCHIVES.reserve_space(root, 1)

    def test_dangling_receipt_alias_cannot_write_outside_owned_storage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            args, archives = self.fixture(root)
            unrelated = root / "unrelated.json"
            (args.output / args.receipt).symlink_to(unrelated)
            with self.assertRaisesRegex(ValueError, "receipt must be a new file"):
                ARCHIVES.prepare(args)
            self.assertFalse(unrelated.exists())
            self.assertEqual(list(archives.iterdir()), [])

    def test_owned_preparation_installs_original_bytes_and_records_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            args, archives = self.fixture(Path(directory))
            lock_hash = ARCHIVES.digest(args.output / "source/codex-rs/Cargo.lock")
            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 0), patch("sys.stdout", new=io.StringIO()):
                ARCHIVES.prepare(args)
            self.assertEqual((archives / "fixture-1.0.0.crate").read_bytes(), b"fixture")
            report = json.loads((args.output / args.receipt).read_text())
            self.assertEqual(report["exit_code"], 0)
            self.assertEqual(report["prepared_bytes"], 7)
            self.assertEqual(report["original_lock_sha256"], report["final_lock_sha256"])

    def test_receipt_reserves_space_even_when_no_archives_are_missing(self):
        with tempfile.TemporaryDirectory() as directory:
            args, archives = self.fixture(Path(directory))
            (args.output / "cache.json").write_text(json.dumps({"missing": []}))
            lock_hash = ARCHIVES.digest(args.output / "source/codex-rs/Cargo.lock")
            space = type("Space", (), {"free": 24})()
            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 24), patch.object(ARCHIVES.shutil, "disk_usage", return_value=space), self.assertRaisesRegex(ValueError, "resource limit"):
                ARCHIVES.prepare(args)
            self.assertFalse((args.output / args.receipt).exists())
            self.assertEqual(list(archives.iterdir()), [])

    def test_changed_lock_is_reported_as_failure_in_retained_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            args, _ = self.fixture(Path(directory))
            lock = args.output / "source/codex-rs/Cargo.lock"
            lock_hash = ARCHIVES.digest(lock)
            save = ARCHIVES.save_archive

            def changed_lock(*params):
                size = save(*params)
                lock.write_text(lock.read_text() + "\n")
                return size

            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 0), patch.object(ARCHIVES, "save_archive", side_effect=changed_lock), self.assertRaisesRegex(ValueError, "changed the source lock"):
                ARCHIVES.prepare(args)
            report = json.loads((args.output / args.receipt).read_text())
            self.assertEqual(report["exit_code"], 1)
            self.assertIn("changed the source lock", report["error"])
            self.assertNotEqual(report["original_lock_sha256"], report["final_lock_sha256"])

    def test_receipt_cannot_spend_the_floor_remaining_after_an_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            args, archives = self.fixture(Path(directory))
            lock_hash = ARCHIVES.digest(args.output / "source/codex-rs/Cargo.lock")
            ample = type("Space", (), {"free": 24 + 2 * os.statvfs(args.output).f_frsize})()
            floor = type("Space", (), {"free": 24})()
            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 24), patch.object(ARCHIVES.shutil, "disk_usage", side_effect=[ample, ample, floor]), self.assertRaisesRegex(ValueError, "resource limit"):
                ARCHIVES.prepare(args)
            self.assertEqual((archives / "fixture-1.0.0.crate").read_bytes(), b"fixture")
            self.assertFalse((args.output / args.receipt).exists())

    def test_overall_deadline_interrupts_a_pending_http_body_read(self):
        release = threading.Event()

        class SlowBody(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", str(1 << 20))
                self.end_headers()
                self.wfile.write(b"x")
                self.wfile.flush()
                release.wait(2)

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), SlowBody)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        connection = HTTPConnection("127.0.0.1", server.server_port, timeout=1)
        try:
            with tempfile.TemporaryDirectory() as directory:
                args, archives = self.fixture(Path(directory))
                (args.from_cache / "registry/cache" / ARCHIVES.REGISTRY / "fixture-1.0.0.crate").unlink()
                lock_hash = ARCHIVES.digest(args.output / "source/codex-rs/Cargo.lock")

                def open_response(*_, **__):
                    connection.request("GET", "/fixture")
                    response = connection.getresponse()
                    response.url = "https://static.crates.io/crates/fixture/fixture-1.0.0.crate"
                    return response

                with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 0), patch.object(ARCHIVES, "MAX_SECONDS", 0.1), patch.object(ARCHIVES.urllib.request, "build_opener") as opener, self.assertRaisesRegex(TimeoutError, "preparation deadline exceeded"):
                    opener.return_value.open.side_effect = open_response
                    ARCHIVES.prepare(args)
                self.assertEqual(list(archives.iterdir()), [])
                report = json.loads((args.output / args.receipt).read_text())
                self.assertEqual(report["exit_code"], 1)
                self.assertIn("deadline exceeded", report["error"])
        finally:
            release.set()
            connection.close()
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)


if __name__ == "__main__":
    unittest.main()
