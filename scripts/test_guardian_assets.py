"""Deterministic guardian platform, checksum and archive policy checks."""
import hashlib
import json
from pathlib import Path
import struct
import tempfile
import unittest
from guardian_assets import ARCHITECTURES, verify_guardians


def fixture_guardians(directory):
    checksums = {}
    directory.mkdir(parents=True, exist_ok=True)
    for arch, machine in ARCHITECTURES.items():
        data = bytearray(64)
        data[:6] = b"\x7fELF\x02\x01"
        struct.pack_into("<H", data, 18, machine)
        name = "linux-" + arch
        (directory / name).write_bytes(data)
        (directory / name).chmod(0o755)
        checksums[name] = hashlib.sha256(data).hexdigest()
    (directory / "checksums.json").write_text(json.dumps(checksums))

class GuardianAssetsTest(unittest.TestCase):
    def test_verify_both_platforms_and_reject_corruption(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fixture_guardians(directory)
            self.assertEqual(len(verify_guardians(directory)), 2)
            (directory / "linux-arm64").write_bytes(b"changed")
            with self.assertRaisesRegex(ValueError, "checksum"):
                verify_guardians(directory)

    def test_reject_platform_swap_even_with_matching_checksum(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fixture_guardians(directory)
            data = (directory / "linux-amd64").read_bytes()
            (directory / "linux-arm64").write_bytes(data)
            checksums = json.loads((directory / "checksums.json").read_text())
            checksums["linux-arm64"] = hashlib.sha256(data).hexdigest()
            (directory / "checksums.json").write_text(json.dumps(checksums))
            with self.assertRaisesRegex(ValueError, "platform"):
                verify_guardians(directory)
