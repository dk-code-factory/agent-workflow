"""Offline checks for pinned scanner installation and image compression."""
import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

import install_release_scanners as scanners

spec = importlib.util.spec_from_file_location('compress_worker_image', Path(__file__).with_name('compress-worker-image.py'))
compression = importlib.util.module_from_spec(spec)
spec.loader.exec_module(compression)


class ScannerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='release-scanner-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.cache, self.bin = self.root / 'cache', self.root / 'bin'

    def bundle(self, members=None):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode='w:gz') as archive:
            for name, kind in members or [('syft', tarfile.REGTYPE)]:
                member = tarfile.TarInfo(name)
                member.type = kind
                member.linkname = '../outside'
                member.size = 6 if kind == tarfile.REGTYPE else 0
                archive.addfile(member, io.BytesIO(b'binary') if member.size else None)
        return output.getvalue()

    def pins(self, data):
        return {'syft': {'version': '1.57.0', 'release': 'https://fixture.invalid/',
                         'archives': {arch: {'name': f'syft-{arch}.tar.gz',
                                            'sha256': hashlib.sha256(data).hexdigest()}
                                      for arch in ['amd64', 'arm64']}}}

    def install(self, data, pins=None, version='syft 1.57.0\n', arch='amd64'):
        with patch.object(scanners.urllib.request, 'urlopen', return_value=io.BytesIO(data)) as fetch, \
                patch.object(scanners.subprocess, 'run', return_value=SimpleNamespace(stdout=version)) as run:
            scanners.install(arch, self.cache, self.bin, pins or self.pins(data))
            return fetch, run

    def test_cold_download_and_verified_offline_cache_reinstall_only_binary(self):
        data = self.bundle([('syft', tarfile.REGTYPE), ('../outside', tarfile.REGTYPE)])
        fetch, run = self.install(data)
        fetch.assert_called_once_with('https://fixture.invalid/syft-amd64.tar.gz', timeout=120)
        run.assert_called_once_with([str(self.bin / 'syft'), '--version'], check=True, capture_output=True, text=True)
        self.assertEqual((self.bin / 'syft').read_bytes(), b'binary')
        self.assertEqual((self.bin / 'syft').stat().st_mode & 0o777, 0o755)
        self.assertFalse((self.root / 'outside').exists())
        (self.bin / 'syft').write_bytes(b'changed installed binary')
        fetch, _ = self.install(data)
        fetch.assert_not_called()
        self.assertEqual((self.bin / 'syft').read_bytes(), b'binary')

    def test_download_checksum_mismatch_never_installs_or_caches(self):
        data = self.bundle()
        pins = self.pins(data)
        pins['syft']['archives']['amd64']['sha256'] = '0' * 64
        with self.assertRaisesRegex(ValueError, 'download checksum'):
            self.install(data, pins)
        self.assertEqual(list(self.cache.iterdir()), [])
        self.assertEqual(list(self.bin.iterdir()), [])

    def test_changed_cache_is_rejected_without_execution(self):
        data = self.bundle()
        self.install(data)
        (self.cache / 'syft-amd64.tar.gz').write_bytes(b'corrupt cache')
        with patch.object(scanners.subprocess, 'run') as run, self.assertRaisesRegex(ValueError, 'cached archive checksum'):
            scanners.install('amd64', self.cache, self.bin, self.pins(data))
        run.assert_not_called()

    def test_binary_link_duplicate_and_absence_are_rejected(self):
        for members in [[('syft', tarfile.SYMTYPE)], [('syft', tarfile.LNKTYPE)],
                        [('syft', tarfile.REGTYPE), ('syft', tarfile.REGTYPE)],
                        [('../syft', tarfile.REGTYPE)]]:
            with self.subTest(members=members):
                for item in self.cache.glob('*') if self.cache.exists() else []:
                    item.unlink()
                data = self.bundle(members)
                with self.assertRaisesRegex(ValueError, 'regular file'):
                    self.install(data)

    def test_pinned_version_mismatch_fails(self):
        with self.assertRaisesRegex(ValueError, 'version mismatch'):
            self.install(self.bundle(), version='syft 9.9.9\n')

    def test_architecture_selects_distinct_archive(self):
        fetch, _ = self.install(self.bundle(), arch='arm64')
        fetch.assert_called_once_with('https://fixture.invalid/syft-arm64.tar.gz', timeout=120)


class CompressionTests(unittest.TestCase):
    def test_same_tar_bytes_stable_headers_inventory_and_cleanup(self):
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode='w') as archive:
            entry = tarfile.TarInfo('layer/contents')
            entry.size = 60000
            archive.addfile(entry, io.BytesIO(b'contents\n' * 6667))
        original = raw.getvalue()
        epoch = 1234567890
        outputs = []
        with tempfile.TemporaryDirectory(prefix='release-compression-test-') as directory:
            root = Path(directory)
            for index in range(2):
                stage = root / str(index)
                stage.mkdir()
                source = stage / 'worker-linux-amd64.docker.tar'
                source.write_bytes(original)
                report = {'archive': source.name, 'archive_sha256': hashlib.sha256(original).hexdigest(),
                          'image': 'sha256:original', 'architecture': 'amd64', 'published': False}
                metadata = stage / 'worker-linux-amd64.json'
                metadata.write_text(json.dumps(report))
                compression.compress(stage, 'amd64', epoch)
                updated = json.loads(metadata.read_text())
                compressed = (stage / updated['archive']).read_bytes()
                self.assertEqual(gzip.decompress(compressed), original)
                self.assertEqual(updated['archive_sha256'], hashlib.sha256(compressed).hexdigest())
                self.assertEqual({k: v for k, v in updated.items() if k not in ['archive', 'archive_sha256']},
                                 {k: v for k, v in report.items() if k not in ['archive', 'archive_sha256']})
                self.assertEqual(struct.unpack('<I', compressed[4:8])[0], epoch)
                self.assertEqual(compressed[3] & 8, 0)  # No variable source filename.
                self.assertFalse(source.exists())
                outputs.append(compressed)
        self.assertEqual(outputs[0], outputs[1])

    def test_malformed_inventory_preserves_source(self):
        with tempfile.TemporaryDirectory(prefix='release-compression-test-') as directory:
            stage = Path(directory)
            source = stage / 'worker-linux-amd64.docker.tar'
            source.write_bytes(b'original')
            (stage / 'worker-linux-amd64.json').write_text('{invalid')
            with self.assertRaises(json.JSONDecodeError):
                compression.compress(stage, 'amd64', 1234567890)
            self.assertEqual(source.read_bytes(), b'original')
            self.assertFalse(list(stage.glob('*.gz')))


if __name__ == '__main__':
    unittest.main()
