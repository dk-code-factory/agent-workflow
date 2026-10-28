"""Deterministic release rejection tests; no live services or maintainer keys."""
import copy
import fnmatch
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tarfile
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

class NetworkPluginTests(unittest.TestCase):
    root = Path(__file__).resolve().parents[1]

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='network-plugin-')
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        # Only the test copy changes the fixed protected registry prefix.
        self.registry = self.directory / 'fixture registry'
        self.plugin = self.directory / "plugin space ' literal $(touch SHOULD_NOT_EXIST)"
        shutil.copytree(self.root / 'plugins/network-runtime', self.plugin)
        wrapper = self.plugin / 'hooks/pre-tool-use.sh'
        production = wrapper.read_text()
        self.assertEqual(production.count('/var/lib/agent-workflow/runtimes/'), 1)
        wrapper.write_text(production.replace('/var/lib/agent-workflow/runtimes/', str(self.registry) + '/'))
        self.input_path = self.directory / 'input'
        self.environment = {'PATH': '/nonexistent', 'PLUGIN_ROOT': str(self.plugin),
                            'FIXTURE_INPUT': str(self.input_path)}
        hooks = json.loads((self.plugin / 'hooks/hooks.json').read_text())['hooks']
        self.assertEqual(set(hooks), {'PreToolUse'})
        registration, = hooks['PreToolUse']
        self.assertEqual(registration['matcher'], '^Bash$')
        handler, = registration['hooks']
        self.assertEqual(handler['type'], 'command')
        self.assertFalse(handler.get('async', False))
        self.assertGreaterEqual(handler['timeout'], 10)
        self.command = handler['command']
        self.assertEqual(self.command, '/bin/sh "$PLUGIN_ROOT/hooks/pre-tool-use.sh"')

    def run_hook(self, payload=b'{"tool_name":"Bash","tool_input":{"command":"printf original","yield_time_ms":20}}\n', **environment):
        result = subprocess.run(['/bin/sh', '-c', self.command], input=payload, capture_output=True,
                                cwd=self.directory, env=dict(self.environment, **environment), timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, b'')
        self.assertFalse((self.directory / 'SHOULD_NOT_EXIST').exists())
        self.assertLess(len(result.stdout), 1024 * 1024 + 1)
        return json.loads(result.stdout)

    def helper(self, body, name='selected'):
        launcher = self.registry / name / 'launcher'
        launcher.parent.mkdir(parents=True, exist_ok=True)
        launcher.write_text('#!/bin/sh\n' + body)
        launcher.chmod(0o755)

    def assert_denied(self, **environment):
        output = self.run_hook(**environment)
        self.assertEqual(output['hookSpecificOutput']['permissionDecision'], 'deny')
        self.assertNotIn('updatedInput', output['hookSpecificOutput'])

    def test_host_noop_and_native_catalog(self):
        self.assertEqual(self.run_hook(DOCKER_HOST='unix:///ordinary-host', CONTAINER_HOST='ssh://ordinary'), {})
        manifest = json.loads((self.plugin / '.codex-plugin/plugin.json').read_text())
        catalog = json.loads((self.root / '.agents/plugins/marketplace.json').read_text())
        self.assertEqual(manifest['name'], 'network-runtime')
        self.assertEqual(catalog['name'], 'agent-workflow')
        entry, = catalog['plugins']
        self.assertEqual(entry['name'], manifest['name'])
        self.assertEqual(entry['source'], {'source': 'local', 'path': './plugins/network-runtime'})
        self.assertEqual(entry['policy']['installation'], 'AVAILABLE')
        self.assertFalse(self.input_path.exists())

    def test_partial_empty_and_unsafe_context_denies_before_helper(self):
        self.helper('/bin/cat > "$FIXTURE_INPUT"\nexit 9\n')
        for environment in [dict(AGENT_WORKFLOW_NETWORK_RUNTIME='selected'),
                            dict(AGENT_WORKFLOW_NETWORK_REFERENCE='{}'),
                            dict(AGENT_WORKFLOW_NETWORK_RUNTIME='', AGENT_WORKFLOW_NETWORK_REFERENCE=''),
                            *[dict(AGENT_WORKFLOW_NETWORK_RUNTIME=name, AGENT_WORKFLOW_NETWORK_REFERENCE='{}')
                              for name in ['../selected', '/selected', 'x/../selected', '-selected', '_selected',
                                           'x' * 25, 'é', 'selected\n', 'selected;id']]]:
            with self.subTest(environment=environment):
                self.assert_denied(**environment)
        self.assertFalse(self.input_path.exists())

    def test_missing_old_nonzero_and_unrecognized_helpers_deny(self):
        selected = dict(AGENT_WORKFLOW_NETWORK_RUNTIME='selected', AGENT_WORKFLOW_NETWORK_REFERENCE='{}')
        self.assert_denied(**selected)
        for body in ['exit 0\n', "printf '%s\\n' 'usage: old launcher'\n",
                     "printf '%s\\n' 'unsafe diagnostic' >&2\nexit 9\n",
                     "printf '%s\\n' '{malformed'\n",
                     "printf '%s\\n' 'AGENT_WORKFLOW_CODEX_HOOK_V1'\n",
                     "printf '%s\\n' 'AGENT_WORKFLOW_CODEX_HOOK_V1' '{}'\nexit 1\n"]:
            with self.subTest(body=body):
                self.helper(body)
                self.assert_denied(**selected)

    def test_matching_helper_receives_original_stdin_and_mode(self):
        expected = {'hookSpecificOutput': {'hookEventName': 'PreToolUse', 'permissionDecision': 'allow',
                                        'updatedInput': {'command': "export DOCKER_HOST='unix:///selected'; printf original", 'yield_time_ms': 20}}}
        body = '[ "$1" = __agent_workflow_codex_hook_v1 ] && [ "$#" = 1 ] || exit 3\n'
        body += '/bin/cat > "$FIXTURE_INPUT"\n'
        body += "printf '%s\\n' AGENT_WORKFLOW_CODEX_HOOK_V1 " + shlex.quote(json.dumps(expected)) + '\n'
        payload = b' { "tool_name": "Bash", "tool_input": {"command":"printf original", "yield_time_ms":20} } \n'
        for name in ['selected', 'A', 'a_0-' + 'x' * 20]:
            self.helper(body, name)
            for selectors in [{}, dict(DOCKER_HOST='unix:///stale', CONTAINER_HOST='unix:///missing')]:
                with self.subTest(name=name, selectors=selectors):
                    output = self.run_hook(payload, AGENT_WORKFLOW_NETWORK_RUNTIME=name,
                                           AGENT_WORKFLOW_NETWORK_REFERENCE='{}', **selectors)
                    self.assertEqual(output, expected)
                    self.assertEqual(self.input_path.read_bytes(), payload)

    def test_oversized_helper_output_is_bounded_denial(self):
        self.helper("printf '%s\\n' AGENT_WORKFLOW_CODEX_HOOK_V1 " + shlex.quote('x' * (1024 * 1024 + 100)) + '\n')
        self.assert_denied(AGENT_WORKFLOW_NETWORK_RUNTIME='selected', AGENT_WORKFLOW_NETWORK_REFERENCE='{}')


class PluginArchiveTests(unittest.TestCase):
    root = Path(__file__).resolve().parents[1]
    assets = {'.agents/plugins/marketplace.json': 0o644,
              'plugins/network-runtime/.codex-plugin/plugin.json': 0o644,
              'plugins/network-runtime/hooks/hooks.json': 0o644,
              'plugins/network-runtime/hooks/pre-tool-use.sh': 0o755,
              'plugins/network-runtime/README.md': 0o644}

    def verify_fixture(self, directory, change=None):
        directory = Path(directory)
        for os_name in ['linux', 'darwin']:
            for arch in ['amd64', 'arm64']:
                files = {name: ((self.root / name).read_bytes(), mode) for name, mode in self.assets.items()}
                for name in ['agent-workflow', 'agent-workflow-runtime', 'agent-workflow-guardian', 'workflow-conformance']:
                    files[name] = (b'fixture executable', 0o755)
                for name in ['agents.yml', 'LICENSE', 'NOTICE', 'licenses/index.json', 'bridge/claude/bridge.mjs', 'bridge/claude/package-lock.json']:
                    files[name] = (b'fixture', 0o644)
                files['build.json'] = (json.dumps({'source_fingerprint': 'a' * 64, 'os': os_name, 'arch': arch}).encode(), 0o644)
                if change:
                    change(files)
                archive = directory / f'agent-workflow-fixture-{os_name}-{arch}.tar.gz'
                with tarfile.open(archive, 'w:gz') as bundle:
                    prefix = tarfile.TarInfo('fixture')
                    prefix.type = tarfile.DIRTYPE
                    bundle.addfile(prefix)
                    for name, (data, mode) in files.items():
                        entry = tarfile.TarInfo('fixture/' + name)
                        entry.size, entry.mode = len(data), mode
                        bundle.addfile(entry, io.BytesIO(data))
        (directory / 'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest() + '  ' + p.name + '\n'
                                                    for p in sorted(directory.glob('*.tar.gz'))))
        return subprocess.run(['python3', str(self.root / 'scripts/verify-release.py'), str(directory)], capture_output=True, text=True)

    def test_all_platform_assets_and_helper_modes(self):
        with tempfile.TemporaryDirectory() as directory:
            result = self.verify_fixture(directory)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(len(json.loads(result.stdout)['verified_archives']), 4)

    def test_reject_missing_changed_extra_and_nonexecutable_assets(self):
        hook = 'plugins/network-runtime/hooks/pre-tool-use.sh'
        changes = [lambda files: files.pop(hook),
                   lambda files: files.__setitem__(hook, (b'changed', 0o755)),
                   lambda files: files.__setitem__(hook, (files[hook][0], 0o644)),
                   lambda files: files.__setitem__('plugins/network-runtime/unexpected', (b'extra', 0o644)),
                   lambda files: files.__setitem__('agent-workflow-runtime', (b'fixture', 0o644)),
                   lambda files: files.pop('agent-workflow-runtime'),
                   lambda files: files.__setitem__('agent-workflow', (b'', 0o755))]
        for change in changes:
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                result = self.verify_fixture(directory, change)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('archive ', result.stderr)

class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.expected = {'scenario': 'native-codex-smoke', 'provider': 'codex-cli', 'authentication': 'native', 'os': 'linux', 'arch': 'amd64'}
        self.policy = {'version': '0.2.0', 'required_evidence': [self.expected], 'publication_repositories': []}
        self.record = dict(self.expected, engine={'commit': 'a' * 40, 'version': '0.2.0', 'dirty': 'false', 'source_fingerprint': 'b' * 64},
                           configuration_fingerprint='c' * 64, started='2026-10-17T01:00:00Z', finished='2026-10-17T01:01:00Z',
                           provider_versions={'codex-cli': '0.164.0'}, assertions={'workflow_succeeded': True, 'response_marker': True, 'usage_observed': True})
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
        for change in [{'authentication': 'gateway'}, {'provider': 'claude-sdk'}, {'error': 'failed'}, {'artifacts': ['/private/path']}, {'assertions': {}},
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

    def test_unreported_native_claude_version_is_rejected(self):
        self.expected.update(provider='claude-cli', scenario='native-claude-smoke')
        self.record.update(self.expected)
        self.record['provider_versions'] = {'claude-cli': 'unavailable'}
        with self.assertRaises(gate.GateError):
            self.validate()

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

class WorkerPackagingTests(unittest.TestCase):
    def test_guardian_builds_from_dockerfile_copy_context(self):
        root = Path(__file__).resolve().parents[1]
        rules = [line.strip() for line in (root / '.dockerignore').read_text().splitlines()
                 if line.strip() and not line.lstrip().startswith('#')]
        def ignored(path):
            relative = path.relative_to(root).as_posix()
            excluded = False
            for rule in rules:
                if fnmatch.fnmatchcase(relative, rule.lstrip('!').rstrip('/')):
                    excluded = not rule.startswith('!')
            return excluded
        with tempfile.TemporaryDirectory(prefix='worker-guardian-context-') as directory:
            context = Path(directory) / 'src'
            context.mkdir()
            stage = None
            copies = 0
            # Materialize the Dockerfile's restricted sources, so the checkout
            # cannot accidentally supply a missing transitive package.
            for line in (root / 'images/worker.Dockerfile').read_text().splitlines():
                if not line.startswith(('FROM ', 'COPY ')):
                    continue
                words = shlex.split(line, comments=True)
                if words[0] == 'FROM':
                    stage = words[-1]
                if stage != 'guardian' or words[0] != 'COPY':
                    continue
                self.assertFalse(words[1].startswith('--'), 'unsupported guardian COPY syntax')
                destination = context / words[-1]
                self.assertTrue(destination.resolve().is_relative_to(context))
                for source in words[1:-1]:
                    source = root / source
                    for path in [source, *source.parents]:
                        if path == root:
                            break
                        self.assertFalse(ignored(path), f'{path.relative_to(root)} excluded by .dockerignore')
                    if source.is_dir():
                        shutil.copytree(source, destination, dirs_exist_ok=True,
                                        ignore=lambda directory, names: [name for name in names
                                            if ignored(Path(directory) / name)])
                    else:
                        destination.mkdir(parents=True, exist_ok=True)
                        shutil.copy2(source, destination / source.name)
                    copies += 1
            self.assertGreater(copies, 0, 'guardian stage must copy its build sources')
            # The copied Docker context has no Git metadata. Ignore unrelated
            # ancestor repositories while testing its offline dependency closure.
            result = subprocess.run(
                ['go', 'build', '-trimpath', '-buildvcs=false', '-o', str(Path(directory) / 'guardian'), './cmd/agent-workflow-guardian'],
                cwd=context, capture_output=True, text=True, timeout=120,
                env=dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH='amd64',
                         GOWORK='off', GOPROXY='off', GOTOOLCHAIN='auto'))
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

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

    def test_newer_toolchain_is_rejected_before_packaging(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            command = root / 'go'
            command.write_text('#!/bin/sh\nif [ "$1" = run ]; then printf \'%s\\n\' \'{"version":"0.2.0","go":"go99.0.0"}\'; fi\n')
            command.chmod(0o755)
            result = subprocess.run(['python3', str(Path(__file__).with_name('build-release.py')), '--output', str(root / 'dist')],
                env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH']), capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('release requires pinned Go toolchain', result.stderr)
            self.assertFalse(list((root / 'dist').glob('*.tar.gz')))

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
