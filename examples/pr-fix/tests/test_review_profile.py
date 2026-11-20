import importlib.util
from contextlib import contextmanager
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

path = Path(__file__).resolve().parents[1] / 'profiles/code_review_ad.py'
spec = importlib.util.spec_from_file_location('code_review_ad_profile', path)
profile = importlib.util.module_from_spec(spec)
spec.loader.exec_module(profile)


class ReviewProfileTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='review-profile-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.plugin = self.root / 'package'
        self.workspace = self.root / 'repo'
        self.workspace.mkdir()
        for name in (*profile.REQUIRED, 'common/codex/checks/claims.md'):
            path = self.plugin / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('Original package bytes: ' + name + '\n')

    def configure(self, **kwargs):
        return profile.configure(self.plugin, self.workspace, kwargs.get('output', 'review.yml'),
                                 kwargs.get('resource_dir', 'review-resources'), kwargs.get('contexts', ()))

    def test_native_passes_are_independent_read_only_and_use_original_package(self):
        with patch('subprocess.run', side_effect=AssertionError('no provider or process allowed')):
            selected, manifest = self.configure()
        controls = selected['jobs']['fix']['with']
        self.assertEqual(controls['reviewers'], [['code-review-ad-' + role for role in profile.ROLES]])
        self.assertEqual(controls['finalizers'], [])
        self.assertEqual(controls['limits']['review_cycles'], 2)
        for role in profile.ROLES:
            adapter = controls['adapters']['code-review-ad-' + role]
            self.assertFalse(adapter['writes'])
            self.assertEqual(adapter['kind'], 'review')
            self.assertIn('--pass ' + role + ' --base BASE --head HEAD', adapter['assignment'])
            self.assertIn('Inspect no prior reviewer reports or sibling findings', adapter['assignment'])
            self.assertEqual(adapter['resources'], controls['resources'])
        for name in manifest['files']:
            self.assertEqual((self.workspace / 'review-resources' / name).read_bytes(), (self.plugin / name).read_bytes())

    def test_incomplete_or_symlinked_package_leaves_no_partial_profile(self):
        (self.plugin / profile.REQUIRED[0]).unlink()
        with self.assertRaisesRegex(ValueError, 'incomplete'):
            self.configure()
        self.assertEqual(list(self.workspace.iterdir()), [])
        target = self.plugin / profile.REQUIRED[1]
        (self.plugin / profile.REQUIRED[0]).symlink_to(target)
        with self.assertRaisesRegex(ValueError, 'symlinks'):
            self.configure()
        self.assertEqual(list(self.workspace.iterdir()), [])

    def test_file_limit_stops_inventory_before_reading_excess_files(self):
        for index in range(4097):
            path = self.plugin / 'common' / f'extra-{index:04d}'
            path.touch()

        common = self.plugin / 'common'
        original_scandir = os.scandir
        with original_scandir(common) as entries:
            root_entries = sorted(entries, key=lambda entry: entry.name)

        def inventory():
            yield from root_entries
            self.fail('materialized the complete directory before enforcing the file limit')

        @contextmanager
        def scandir(directory):
            if Path(directory) == common:
                yield inventory()
            else:
                with original_scandir(directory) as entries:
                    yield entries

        with patch.object(os, 'scandir', scandir):
            with self.assertRaisesRegex(ValueError, 'bounded snapshot size'):
                self.configure()
        self.assertEqual(list(self.workspace.iterdir()), [])

    def test_directory_queue_is_bounded_before_descent(self):
        common = self.plugin / 'common'
        for index in range(4097):
            (common / f'directory-{index:04d}').mkdir()
        original_scandir = os.scandir

        def scandir(directory):
            self.assertEqual(Path(directory), common, 'descended before bounding the directory queue')
            return original_scandir(directory)

        with patch.object(os, 'scandir', scandir):
            with self.assertRaisesRegex(ValueError, 'bounded snapshot size'):
                self.configure()
        self.assertEqual(list(self.workspace.iterdir()), [])

    def test_byte_limit_stops_reading_before_loading_the_remaining_package(self):
        paths = [path for path in (self.plugin / 'common').rglob('*') if path.is_file()]
        for index in range(5):
            path = self.plugin / 'common' / f'oversized-{index:04d}'
            with path.open('wb') as stream:
                stream.truncate(4 << 20)
            paths.append(path)
        last = paths[-1]
        original_open = Path.open

        def bounded_open(path, *args, **kwargs):
            self.assertNotEqual(path, last, 'loaded files after exceeding the byte limit')
            return original_open(path, *args, **kwargs)

        with patch.object(profile, '_resource_files', return_value=iter(paths), create=True), patch.object(Path, 'open', bounded_open):
            with self.assertRaisesRegex(ValueError, 'bounded snapshot size'):
                self.configure()
        self.assertEqual(list(self.workspace.iterdir()), [])

    def test_existing_profile_and_workspace_escape_are_rejected(self):
        self.configure()
        before = (self.workspace / 'review.yml').read_bytes()
        with self.assertRaisesRegex(ValueError, 'existing'):
            self.configure()
        self.assertEqual((self.workspace / 'review.yml').read_bytes(), before)
        with self.assertRaisesRegex(ValueError, 'must not overlap'):
            self.configure(output='review-resources/report.yml')
        with self.assertRaisesRegex(ValueError, 'workspace-relative'):
            self.configure(output='../outside.yml')

    def test_supplemental_context_is_captured_as_governing_resource(self):
        (self.workspace / 'spec.json').write_text(json.dumps({'body': 'Frozen original specification'}))
        selected, _ = self.configure(contexts=['spec.json'])
        controls = selected['jobs']['fix']['with']
        self.assertIn('spec.json', controls['resources'])
        for adapter in controls['adapters'].values():
            self.assertIn('spec.json', adapter['resources'])
            self.assertIn('spec.json', adapter['assignment'])

    def test_destination_symlink_is_rejected_without_writes(self):
        (self.workspace / 'linked').symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, 'symlinks'):
            self.configure(output='linked/review.yml')
        self.assertFalse((self.root / 'review.yml').exists())

    def test_regular_file_destination_ancestor_is_rejected_without_writes(self):
        for field in ('output', 'resource_dir'):
            with self.subTest(field=field):
                workspace = self.workspace / field
                workspace.mkdir()
                with patch.object(self, 'workspace', workspace):
                    blocker = workspace / 'blocked'
                    blocker.write_text('Preserve this workspace file\n')
                    before = {path.name: path.read_bytes() for path in workspace.iterdir()}
                    with self.assertRaisesRegex(ValueError, 'directory'):
                        self.configure(**{field: blocker.name + '/nested/profile'})
                    self.assertEqual({path.name: path.read_bytes() for path in workspace.iterdir()}, before)
