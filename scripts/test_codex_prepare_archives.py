import argparse
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

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
            space = type("Space", (), {"free": 30})()
            with patch.object(ARCHIVES, "LOCK_HASHES", {lock_hash}), patch.object(ARCHIVES, "MIN_FREE", 24), patch.object(ARCHIVES.shutil, "disk_usage", return_value=space), self.assertRaisesRegex(ValueError, "resource limit"):
                ARCHIVES.prepare(args)
            self.assertEqual(list(archives.iterdir()), [])
            report = json.loads((args.output / args.receipt).read_text())
            self.assertEqual(report["exit_code"], 1)
            self.assertEqual(report["prepared_bytes"], 0)

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


if __name__ == "__main__":
    unittest.main()
