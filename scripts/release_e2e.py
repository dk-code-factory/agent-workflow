"""Release-only provider orchestration. Imported tests never invoke providers."""
import argparse
import base64
import json
import os
from pathlib import Path
import re
import shutil
import tarfile
import subprocess
import tempfile
import time

from release_lib import ROOT, git, sha256, source_fingerprint

REPO = 'dk-code-factory/agent-workflow'
SMOKE = {'native-codex-smoke', 'native-claude-smoke', 'session-resume', 'session-interruption', 'claude-gateway-structured'}
RECOVERY = ['owner_loss', 'runtime_unavailable', 'runtime_restart', 'cleanup_recovery', 'stale_execution_ownership']

def require(ok, message):
    if not ok:
        raise ValueError(message)

def authorize():
    ref = os.environ.get('GITHUB_REF', '')
    require(os.environ.get('GITHUB_ACTIONS') == 'true' and os.environ.get('GITHUB_REPOSITORY') == REPO
            and os.environ.get('GITHUB_EVENT_NAME') == 'push' and re.fullmatch(r'refs/tags/v\d+\.\d+\.\d+', ref)
            and os.environ.get('GITHUB_WORKFLOW_REF') == REPO + '/.github/workflows/release.yml@' + ref,
            'provider E2E only accepts the repository version-tag release workflow')
    return {'repository': REPO, 'workflow': '.github/workflows/release.yml', 'run_id': os.environ['GITHUB_RUN_ID'],
            'run_attempt': os.environ['GITHUB_RUN_ATTEMPT'], 'ref': ref}

def jwt_expiry(token):
    parts = token.split('.')
    if len(parts) != 3:
        return
    try:
        payload = json.loads(base64.urlsafe_b64decode(parts[1] + '=' * (-len(parts[1]) % 4)))
        expiry = payload.get('exp')
        require(isinstance(expiry, (float, int)) and expiry > time.time(), 'subscription credential expired; rotate it explicitly')
    except (ValueError, TypeError, KeyError):
        raise ValueError('subscription credential invalid or expired; rotate it explicitly') from None

def credentials(codex, claude):
    require(bool(codex.strip()) and bool(claude.strip()), 'missing CODEX_AUTH_JSON or CLAUDE_CODE_OAUTH_TOKEN; configure pre-release secrets')
    try:
        value = json.loads(codex)
        tokens = value['tokens']
        require(not value.get('OPENAI_API_KEY') and all(isinstance(tokens.get(k), str) and tokens[k] for k in ['access_token', 'account_id']),
                'Codex subscription file authentication required')
        jwt_expiry(tokens['access_token'])
        jwt_expiry(claude.strip())
    except (ValueError, TypeError, KeyError):
        raise ValueError('subscription credential invalid or expired; replace pre-release secrets explicitly') from None
    return value, claude.strip()

def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for v in value.values():
            yield from strings(v)
    elif isinstance(value, list):
        for v in value:
            yield from strings(v)

def mask(value):
    for s in strings(value):
        if s:
            print('::add-mask::' + s.replace('%', '%25').replace('\r', '%0D').replace('\n', '%0A'), flush=True)

def private_write(path, value):
    with open(path, 'x', opener=lambda p, flags: os.open(p, flags, 0o600)) as stream:
        stream.write(value)

def setup(state, dist):
    authorize()
    codex, claude = credentials(os.environ.get('CODEX_AUTH_JSON', ''), os.environ.get('CLAUDE_CODE_OAUTH_TOKEN', ''))
    mask(codex)
    mask(claude)
    state.mkdir(mode=0o700)  # Exclusive ownership; never reuse ambient provider state.
    private_write(state / "owner.json", json.dumps(authorize()))
    (state / 'codex').mkdir(mode=0o700)
    (state / 'claude').mkdir(mode=0o700)
    (state / 'home').mkdir(mode=0o700)
    private_write(state / 'codex/auth.json', json.dumps(codex))
    private_write(state / 'codex/config.toml', 'cli_auth_credentials_store = "file"\nforced_login_method = "chatgpt"\n')
    private_write(state / 'claude-token', claude)
    version = json.loads((ROOT / 'release/policy.json').read_text())['version']
    archive = dist / f'agent-workflow-{version}-linux-amd64.tar.gz'
    # Reject traversal and special files while extracting the distribution.
    with tarfile.open(archive) as bundle:
        bundle.extractall(state, filter='data')
    package = state / f'agent-workflow-{version}-linux-amd64'
    build = json.loads((package / 'build.json').read_text())
    require(build['commit'] == git('rev-parse', 'HEAD') == os.environ['GITHUB_SHA'] and build['dirty'] == 'false'
            and build['source_fingerprint'] == source_fingerprint(), 'archive does not match clean release candidate')
    subprocess.run(['npm', 'ci', '--prefix', str(package / 'bridge/claude'), '--ignore-scripts', '--no-audit', '--no-fund'], check=True)
    private_write(state / 'package', str(package))
    private_write(state / 'origin.json', json.dumps(authorize()))

def provider_env(state):
    env = {k: v for k, v in os.environ.items() if k not in ['GH_TOKEN', 'GITHUB_TOKEN', 'CODEX_AUTH_JSON', 'CLAUDE_CODE_OAUTH_TOKEN',
            'ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_BASE_URL', 'OPENAI_API_KEY', 'CODEX_API_KEY', 'OPENAI_BASE_URL',
            'DOCKER_HOST', 'DOCKER_CONTEXT', 'CONTAINER_HOST', 'CONTAINER_CONNECTION']}
    env.update(CODEX_HOME=str(state / 'codex'), CLAUDE_CONFIG_DIR=str(state / 'claude'), HOME=str(state / 'home'),
               CLAUDE_CODE_OAUTH_TOKEN=(state / 'claude-token').read_text())
    env['PATH'] = os.pathsep.join([str(ROOT / 'conformance/providers/node_modules/.bin'), str(ROOT / 'images/codex/node_modules/.bin'), env['PATH']])
    return env

def configuration(state, package, runtime=None, image=None):
    agents = {
        'claude_cli': {'provider': 'claude-cli', 'authentication': {'mode': 'native'}},
        'codex_cli': {'provider': 'codex-cli', 'authentication': {'mode': 'native'}},
        'codex': {'provider': 'codex-app-server', 'authentication': {'mode': 'native', 'credential-file': str(state / 'codex/auth.json')}, 'sandbox': 'workspace-write'},
        'claude': {'provider': 'claude-sdk', 'bridge': str(package / 'bridge/claude/bridge.mjs'),
                   'authentication': {'mode': 'native', 'credential-env': 'CLAUDE_CODE_OAUTH_TOKEN'}, 'tools': ['Bash', 'Read', 'Write', 'Edit'],
                   'mcp-grants': [{'server': 'engine', 'tool': t} for t in ['worker_spawn', 'worker_send', 'worker_wait', 'worker_status', 'worker_interrupt', 'worker_close']],
                   'limits': {'max-workers': 4, 'max-depth': 3, 'max-turns': 8, 'turn-timeout': '5m'}}}
    if runtime:
        agents['claude']['execution'] = {'mode': 'container', 'runtime': runtime, 'image': image, 'writable': True, 'cleanup': 'strict'}
    return {'agents': agents}

def rows(policy, phase):
    return [r for r in policy['required_evidence'] if r['scenario'] != 'disposable-github-publication' and ((r['scenario'] in SMOKE) == (phase == 'smoke'))]

def sanitized(record, secrets):
    require(not record.get('error'), 'failed provider scenario; inspect private runner state')
    allowed = ['scenario', 'provider', 'authentication', 'os', 'arch', 'configuration_fingerprint', 'provider_versions',
               'runtime', 'runtime_version', 'image_digest', 'assertions', 'started', 'finished', 'invocations']
    value = {k: record[k] for k in allowed if k in record}
    value['invocations'] = [{k: r[k] for k in ['id', 'provider', 'version']} for r in record.get('invocations', [])]
    value['engine'] = {k: record['engine'][k] for k in ['version', 'commit', 'dirty', 'source_fingerprint']}
    # Never upload arbitrary outputs, configuration, diagnostic text, transcripts or paths.
    require(value.get('invocations') and all(re.fullmatch(r'[A-Za-z0-9@_./+ :;(),=\-]{1,240}', v)
            for v in value.get('provider_versions', {}).values()), 'missing or invalid observed provider identity')
    raw = json.dumps(value)
    require(all(s not in raw for s in secrets if len(s) >= 6), 'credential material found in evidence')
    require(not any(x in raw for x in ['/home/', '/tmp/', '/run/', 'refresh_token', 'access_token', 'id_token']), 'private state found in evidence')
    return value

def run_phase(state, dist, phase):
    origin = authorize()
    require(origin == json.loads((state / 'origin.json').read_text()), 'E2E phases must share private state and workflow attempt')
    policy = json.loads((ROOT / 'release/policy.json').read_text())
    if phase == 'matrix':
        require((state / 'smoke.json').is_file(), 'smoke/session phase must pass before matrix')
    package = Path((state / 'package').read_text())
    env = provider_env(state)
    records, recovery = [], []
    image = json.loads((dist / 'worker-linux-amd64.json').read_text())
    require(sha256(dist / image['archive']) == image['archive_sha256'], 'distribution image archive changed')
    codex = json.loads((state / 'codex/auth.json').read_text())
    secrets = list(strings(codex)) + [(state / 'claude-token').read_text()]
    from release_runtime import DisposableRuntime
    for row in rows(policy, phase):
        runtime = row.get('runtime')
        with DisposableRuntime(state, runtime, env) as fixture:
            call_env = fixture.environment
            if runtime:
                subprocess.run([runtime, 'load', '--input', str(dist / image['archive'])], env=call_env, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=120)
                inspected = json.loads(subprocess.check_output([runtime, 'image', 'inspect', image['image']], env=call_env))
                require(inspected[0].get('Id', inspected[0].get('ID', '')).removeprefix('sha256:') == image['image'].removeprefix('sha256:'), 'runtime loaded a different distribution image')
            cfg = state / ('configuration-' + str(len(list(state.glob('configuration-*')))) + '.json')
            private_write(cfg, json.dumps(configuration(state, package, runtime, image['image'])))
            output = state / ('evidence-' + str(len(list(state.glob('evidence-*')))))
            role = {'claude-cli': 'claude_cli', 'codex-cli': 'codex_cli', 'codex-app-server': 'codex', 'claude-sdk': 'claude'}[row['provider']]
            command = [str(package / 'workflow-conformance'), '--release-ci', '--allow-live', '--scenario', str(ROOT / 'conformance/scenarios' / (row['scenario'] + '.json')),
                       '--config', str(cfg), '--role', role, '--engine', str(package / 'agent-workflow'), '--evidence', str(output), '--timeout', '5m']
            if row['scenario'] == 'nested-workers-capacity-one':
                command += ['--child-role', 'claude']
            if row['scenario'].startswith('mixed-development-'):
                repo = make_mixed_fixture(state)
                command += ['--repository-path', str(repo), '--workflow', 'workflows/mixed-development.yml']
            if row['scenario'] == 'container-owner-loss':
                command += ['--runtime-control', str(fixture.control)]
            print('Validating ' + row['scenario'] + ' / ' + row['provider'] + (' / ' + runtime if runtime else ''), flush=True)
            result = subprocess.run(command, env=call_env, capture_output=True, timeout=315)
            paths = list(output.glob('scenario-*/evidence.json'))
            if result.returncode != 0:
                failure = json.loads(paths[0].read_text()) if len(paths) == 1 else {}
                diagnostic = str(failure.get('error', '')).lower()
                rejected = any(marker in diagnostic for marker in ['401', '403', 'unauthorized', 'authentication', 'invalid oauth', 'token expired', 'credential'])
                raise ValueError('provider authentication rejected; explicitly rotate pre-release credentials' if rejected else 'required scenario failed: ' + row['scenario'] + '; no retry or synthetic coverage is permitted')
            require(len(paths) == 1, 'missing unique scenario result')
            raw = json.loads(paths[0].read_text())
            current_auth = json.loads((state / 'codex/auth.json').read_text())
            mask(current_auth)
            secrets.extend(strings(current_auth))
            records.append(sanitized(raw, secrets))
            if row['scenario'] == 'container-owner-loss':
                assertions = raw.get('runtime_recovery', {})
                require(all(assertions.get(a) is True for a in RECOVERY), 'missing runtime recovery boundaries')
                recovery.append(dict(os='linux', arch='amd64', runtime=runtime, candidate=raw['engine']['commit'],
                                     source_fingerprint=raw['engine']['source_fingerprint'], runtime_version=raw['runtime_version'],
                                     image_digest=raw['image_digest'], assertions=assertions))
    private_write(state / (phase + '.json'), json.dumps({'records': records, 'runtime_recovery': recovery}))

def make_mixed_fixture(state):
    repo = Path(tempfile.mkdtemp(prefix='mixed-', dir=state))
    (repo / 'workflows').mkdir()
    shutil.copyfile(ROOT / 'conformance/workflows/mixed-development.yml', repo / 'workflows/mixed-development.yml')
    for role in ['builder', 'reviewer']:
        (repo / 'workflows' / (role + '.toml')).write_text("name='" + role + "'\ndescription='Bounded release conformance'\ndeveloper_instructions='Complete only the disposable conformance task.'\n")
    subprocess.run(['git', 'init', '-q', '-b', 'main', str(repo)], check=True)
    subprocess.run(['git', '-C', str(repo), 'add', '.'], check=True)
    subprocess.run(['git', '-C', str(repo), '-c', 'user.name=Conformance', '-c', 'user.email=conformance@example.invalid',
                    'commit', '--no-gpg-sign', '-qm', 'Disposable mixed provider fixture'], check=True)
    return repo

def aggregate(state, dist, output):
    origin = authorize()
    require(origin == json.loads((state / 'origin.json').read_text()), 'aggregate workflow identity changed')
    phases = [json.loads((state / (p + '.json')).read_text()) for p in ['smoke', 'matrix']]
    report = {'candidate': git('rev-parse', 'HEAD'), 'source_fingerprint': source_fingerprint(), 'reviewed': False, 'origin': origin,
              'records': sum([p['records'] for p in phases], []), 'runtime_recovery': sum([p['runtime_recovery'] for p in phases], []),
              'images': {a: json.loads((dist / ('worker-linux-' + a + '.json')).read_text())['image'] for a in ['amd64', 'arm64']}}
    from release_certification import validate_automated
    validate_automated(report)
    output.mkdir(parents=True, exist_ok=True)
    (output / 'automated-evidence.json').write_text(json.dumps(report, indent=2, sort_keys=True) + '\n')

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=['setup', 'smoke', 'matrix', 'aggregate', 'cleanup'])
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--dist', type=Path, default=ROOT / 'dist')
    parser.add_argument('--output', type=Path, default=ROOT / 'e2e-artifact')
    args = parser.parse_args()
    try:
        if args.phase == 'setup':
            setup(args.state, args.dist)
        elif args.phase in ['smoke', 'matrix']:
            run_phase(args.state, args.dist, args.phase)
        elif args.phase == 'aggregate':
            aggregate(args.state, args.dist, args.output)
        elif args.state.exists():
            # The state is created exclusively by setup, outside the checkout.
            require(args.state.resolve().parent == Path(os.environ['RUNNER_TEMP']).resolve() and args.state.name == 'release-provider-e2e', 'refusing cleanup outside owned runner state')
            require(json.loads((args.state / 'owner.json').read_text()) == authorize(), 'refusing cleanup of another run or attempt')
            subprocess.run(['sudo', 'rm', '-rf', '--', str(args.state)], check=True)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        # Subprocess args/output can include private state. Keep diagnostics bounded.
        raise SystemExit('provider E2E blocked: ' + (str(error) if isinstance(error, ValueError) else type(error).__name__))
