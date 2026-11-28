import hashlib
import importlib.util
import io
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
                    ARCHIVES.save_archive(io.BytesIO(b"fixture"), destination, expected, lambda _: None)
                self.assertEqual(list(Path(directory).iterdir()), [])

    def test_only_original_hash_is_installed(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "fixture.crate"
            size = ARCHIVES.save_archive(io.BytesIO(b"fixture"), destination, hashlib.sha256(b"fixture").hexdigest(), lambda _: None)
            self.assertEqual(size, 7)
            self.assertEqual(destination.read_bytes(), b"fixture")

    def test_cross_host_or_credential_redirect_is_rejected(self):
        redirects = []
        handler = ARCHIVES.Redirects(redirects)
        for url in ["http://static.crates.io/a", "https://example.com/a", "https://user@static.crates.io/a", "https://static.crates.io:444/a", "https://static.crates.io/a?token=x"]:
            with self.subTest(url=url), self.assertRaises(ValueError):
                handler.redirect_request(None, None, 302, "", {}, url)
        self.assertEqual(redirects, [])


if __name__ == "__main__":
    unittest.main()
