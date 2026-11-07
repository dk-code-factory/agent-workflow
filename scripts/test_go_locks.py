"""Exercise dependency-lock checks with a local file proxy, without live services."""
import contextlib
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from go_locks import check_go_locks

ROOT = Path(__file__).resolve().parents[1]


class GoLockTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='go-lock-fixture-')
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.root = self.directory / 'source'
        self.root.mkdir()
        proxy = self.directory / 'proxy'
        for name, requirements in [('direct', 'require example.com/transitive v1.0.0\n'),
                                   ('transitive', '')]:
            module = f'example.com/{name}'
            manifest = f'module {module}\n\ngo 1.23.0\n\n{requirements}'
            versions = proxy / module / '@v'
            versions.mkdir(parents=True)
            (versions / 'v1.0.0.mod').write_text(manifest)
            (versions / 'v1.0.0.info').write_text(
                '{"Version":"v1.0.0","Time":"2025-01-01T00:00:00Z"}\n')
            with zipfile.ZipFile(versions / 'v1.0.0.zip', 'w') as archive:
                archive.writestr(f'{module}@v1.0.0/go.mod', manifest)
                archive.writestr(f'{module}@v1.0.0/{name}.go', f'package {name}\n')
        (self.root / 'go.mod').write_text(
            'module example.com/main\n\ngo 1.23.0\n\nrequire example.com/direct v1.0.0\n')
        (self.root / 'main.go').write_text('package main\nimport _ "example.com/direct"\nfunc main() {}\n')
        self.environment = patch.dict(os.environ, GOPROXY=proxy.as_uri(), GOSUMDB='off',
                                      GOMODCACHE=str(self.directory / 'cache'), GOWORK='off',
                                      GOTOOLCHAIN='local', GOFLAGS='')
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.download('all')

    def download(self, *args):
        subprocess.run(['go', 'mod', 'download', *args], cwd=self.root, check=True,
                       capture_output=True, text=True, timeout=30)

    def test_complete_locks_pass_without_modifying_source(self):
        before = {name: (self.root / name).read_bytes() for name in ('go.mod', 'go.sum')}
        self.assertTrue(check_go_locks(self.root))
        self.assertEqual(before, {name: (self.root / name).read_bytes() for name in before})

    def test_pruned_transitive_checksums_fail_before_packaging_and_can_be_repaired(self):
        sums = self.root / 'go.sum'
        sums.write_text(''.join(line for line in sums.read_text().splitlines(keepends=True)
                                if not line.startswith('example.com/transitive ')))
        # Ordinary downloads pass while leaving the release inventory incomplete.
        self.download()
        before = sums.read_bytes()
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors):
            self.assertFalse(check_go_locks(self.root))
        self.assertEqual(sums.read_bytes(), before)
        self.assertIn('make go-deps', errors.getvalue())
        self.assertIn('+example.com/transitive v1.0.0 h1:', errors.getvalue())
        subprocess.run(['make', '-f', str(ROOT / 'Makefile'), 'go-deps'], cwd=self.root,
                       check=True, capture_output=True, text=True, timeout=30)
        self.assertTrue(check_go_locks(self.root))

    def test_download_failure_is_not_accepted_as_complete(self):
        (self.root / 'go.mod').write_text(
            'module example.com/main\n\ngo 1.23.0\n\nrequire example.com/missing v1.0.0\n')
        with self.assertRaises(subprocess.CalledProcessError):
            check_go_locks(self.root)


if __name__ == '__main__':
    unittest.main()
