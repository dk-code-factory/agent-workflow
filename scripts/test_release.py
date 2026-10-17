"""Deterministic release rejection tests; no live services or maintainer keys."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from release_lib import source_fingerprint

def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result

gate = module('validate-release')
publisher = module('publish-release')

class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.expected = {'scenario': 'native-codex-smoke', 'provider': 'codex-cli', 'os': 'linux', 'arch': 'amd64'}
        self.policy = {'version': '0.2.0', 'required_evidence': [self.expected], 'publication_repositories': []}
        self.record = dict(self.expected, engine={'commit': 'a' * 40, 'version': '0.2.0', 'dirty': 'false', 'source_fingerprint': 'b' * 64},
                           configuration_fingerprint='c' * 64, started='2026-10-17T01:00:00Z', finished='2026-10-17T01:01:00Z',
                           provider_versions={'codex-cli': '0.161.0'}, assertions={'workflow_succeeded': True, 'response_marker': True, 'usage_observed': True})
        self.report = {'candidate': 'a' * 40, 'source_fingerprint': 'b' * 64, 'reviewed': True, 'records': [self.record]}

    def validate(self):
        return gate.validate_evidence(self.report, self.policy, 'a' * 40, 'b' * 64)

    def test_complete_evidence(self):
        self.assertEqual(self.validate(), 1)

    def test_reject_missing_failed_stale_dirty_and_private_evidence(self):
        cases = []
        for key, value in [('candidate', 'other'), ('source_fingerprint', 'other'), ('reviewed', False), ('records', [])]:
            report = copy.deepcopy(self.report)
            report[key] = value
            cases.append(report)
        for change in [{'error': 'failed'}, {'artifacts': ['/private/path']}, {'assertions': {}},
                       {'engine': dict(self.record['engine'], dirty='true')},
                       {'engine': dict(self.record['engine'], version='0.1.0')}, {'provider_versions': {}}]:
            report = copy.deepcopy(self.report)
            report['records'][0].update(change)
            cases.append(report)
        report = copy.deepcopy(self.report)
        report['records'].append(copy.deepcopy(self.record))
        cases.append(report)
        for report in cases:
            with self.subTest(report=report), self.assertRaises(gate.GateError):
                gate.validate_evidence(report, self.policy, 'a' * 40, 'b' * 64)

    def test_policy_does_not_claim_deferred_or_unsupported_capabilities(self):
        policy = json.loads((gate.ROOT / 'release/policy.json').read_text())
        for row in policy['required_evidence']:
            scenario = json.loads((gate.ROOT / 'conformance/scenarios' / (row['scenario'] + '.json')).read_text())
            self.assertFalse(row['os'] == 'darwin' and row.get('runtime'))
            self.assertFalse(row['provider'] == 'codex-cli' and scenario.get('managed_policy_required'))
            self.assertIn(row['arch'], ['amd64', 'arm64'])

class IdentityTests(unittest.TestCase):
    def test_fingerprint_covers_dockerfiles_modes_and_arbitrary_tracked_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(['git', 'init', '-q', directory], check=True)
            path = root / 'worker.Dockerfile'
            path.write_text('FROM scratch\n')
            subprocess.run(['git', 'add', '.'], cwd=root, check=True)
            original = source_fingerprint(root)
            path.write_text('FROM changed\n')
            self.assertNotEqual(original, source_fingerprint(root))
            path.write_text('FROM scratch\n')
            path.chmod(0o755)
            self.assertNotEqual(original, source_fingerprint(root))
            path.chmod(0o644)
            self.assertEqual(original, source_fingerprint(root))
            (root / 'untracked').write_text('unrelated')
            self.assertEqual(original, source_fingerprint(root))

    def test_signed_unsigned_and_untrusted_tags(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / 'gpg'
            home.mkdir(mode=0o700)
            env = dict(os.environ, GNUPGHOME=str(home))
            def run(*args):
                return subprocess.check_output(args, cwd=root, env=env, stderr=subprocess.DEVNULL, text=True)
            run('gpg', '--batch', '--passphrase', '', '--quick-generate-key', 'Release Test <test@example.invalid>', 'rsa2048', 'sign', '1d')
            fingerprint = next(line.split(':')[9] for line in run('gpg', '--with-colons', '--list-keys').splitlines() if line.startswith('fpr:'))
            key = root / 'key.asc'
            key.write_text(run('gpg', '--armor', '--export', fingerprint))
            run('git', 'init', '-q')
            run('git', '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid', 'commit', '--no-gpg-sign', '--allow-empty', '-qm', 'fixture')
            run('git', '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid', '-c', 'user.signingkey=' + fingerprint, 'tag', '-s', 'v0.2.0', '-m', 'signed fixture')
            run('git', 'tag', 'unsigned')
            with patch.object(gate, 'ROOT', root):
                command = ['git', 'verify-tag', '--raw', 'v0.2.0']
                self.assertEqual(gate.trusted_signature(command, [key], [fingerprint]), fingerprint)
                with self.assertRaises(gate.GateError):
                    gate.trusted_signature(command, [key], ['0' * 40])
                with self.assertRaises(gate.GateError):
                    gate.trusted_signature(['git', 'verify-tag', '--raw', 'unsigned'], [key], [fingerprint])
            run('gpgconf', '--kill', 'gpg-agent')

class PublicationTests(unittest.TestCase):
    def test_draft_retry_and_conflicts(self):
        draft = {'draft': True, 'tag_name': 'v0.2.0', 'target_commitish': 'abc', 'assets': [{'name': 'one'}]}
        publisher.compatible_draft(draft, 'v0.2.0', 'abc', {'one': 'digest', 'two': 'digest'})
        for change in [{'draft': False}, {'target_commitish': 'wrong'}, {'assets': [{'name': 'foreign'}]},
                       {'assets': [{'name': 'one'}, {'name': 'one'}]}]:
            with self.subTest(change=change), self.assertRaises(ValueError):
                publisher.compatible_draft(dict(draft, **change), 'v0.2.0', 'abc', {'one': 'digest'})

    def test_registry_network_errors_do_not_mean_absence(self):
        import urllib.error
        with patch.dict(os.environ, GITHUB_ACTOR='fixture', GH_TOKEN='fixture'):
            with patch('urllib.request.urlopen', side_effect=urllib.error.HTTPError('url', 403, 'denied', {}, None)):
                with self.assertRaises(urllib.error.HTTPError):
                    publisher.manifest('v0.2.0')


class GateRejectionTests(unittest.TestCase):
    def test_version_mismatch(self):
        for tag, version in [('v0.1.0', '0.2.0'), ('v0.2.0', '0.1.0'), ('v0.2.0-rc1', '0.2.0')]:
            with self.subTest(tag=tag, version=version), self.assertRaises(gate.GateError):
                gate.validate_version(tag, {'version': '0.2.0'}, 'const Version = "' + version + '"')

    def test_remote_checks_settings_and_existing_publication(self):
        settings = module('repository-settings')
        protection = copy.deepcopy(settings.BRANCH)
        for key in ['enforce_admins', 'required_conversation_resolution', 'allow_force_pushes', 'allow_deletions']:
            protection[key] = {'enabled': protection[key]}
        response = {
            'repos/example/repo': {'full_name': 'example/repo', 'private': False, 'default_branch': 'main'},
            'repos/example/repo/immutable-releases': {'enabled': True},
            'repos/example/repo/branches/main/protection': protection,
            'repos/example/repo/rulesets?per_page=100': [{'id': 1}, {'id': 2}],
            'repos/example/repo/rulesets/1': settings.RULES[0],
            'repos/example/repo/rulesets/2': settings.RULES[1],
            'repos/example/repo/actions/workflows/test.yml/runs?head_sha=abc&event=push&per_page=100': {
                'workflow_runs': [{'head_branch': 'main', 'status': 'completed', 'conclusion': 'success'}]},
        }
        policy = {'repository': 'example/repo', 'version': '0.2.0'}
        with patch.object(gate, 'api', side_effect=lambda path: response[path]), patch.object(gate, 'existing_release', return_value=None):
            gate.validate_remote(policy, 'abc')
            key = 'repos/example/repo/actions/workflows/test.yml/runs?head_sha=abc&event=push&per_page=100'
            for conclusion in ['failure', 'cancelled', 'skipped', None]:
                response[key]['workflow_runs'][0]['conclusion'] = conclusion
                with self.subTest(conclusion=conclusion), self.assertRaises(gate.GateError):
                    gate.validate_remote(policy, 'abc')
            response[key]['workflow_runs'][0]['conclusion'] = 'success'
            response['repos/example/repo/immutable-releases']['enabled'] = False
            with self.assertRaises(gate.GateError):
                gate.validate_remote(policy, 'abc')
        with patch.object(gate, 'existing_release', return_value={'draft': False}):
            with self.assertRaises(gate.GateError):
                gate.validate_remote(policy, 'abc')

    def test_missing_runtime_certification(self):
        fixture = EvidenceTests()
        fixture.setUp()
        fixture.policy['required_runtime_recovery'] = [{'os': 'linux', 'arch': 'amd64', 'runtime': 'docker'}]
        with self.assertRaises(gate.GateError):
            fixture.validate()

    def test_altered_archive_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'asset.tar.gz').write_bytes(b'changed')
            (root / 'SHA256SUMS').write_text('0' * 64 + '  asset.tar.gz\n')
            result = subprocess.run(['python3', str(Path(__file__).with_name('verify-release.py')), directory], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('checksum mismatch', result.stderr)

if __name__ == '__main__':
    unittest.main()
