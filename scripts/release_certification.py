"""Match maintainer-signed certification to immutable same-run E2E evidence."""
import copy
import importlib.util
import json
import os
import re

from release_lib import ROOT, git, source_fingerprint


def validator():
    spec = importlib.util.spec_from_file_location('release_gate', ROOT / 'scripts/validate-release.py')
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


def require(ok, message):
    if not ok:
        raise ValueError(message)


def validate_automated(report, policy=None, commit=None, fingerprint=None, environment=None):
    policy = policy or json.loads((ROOT / 'release/policy.json').read_text())
    commit = commit or git('rev-parse', 'HEAD')
    fingerprint = fingerprint or source_fingerprint()
    environment = os.environ if environment is None else environment
    origin = report.get('origin', {})
    require(origin.get('repository') == policy['repository'] and origin.get('workflow') == '.github/workflows/release.yml'
            and origin.get('ref') == 'refs/tags/v' + policy['version'] and str(origin.get('run_id')) == environment.get('GITHUB_RUN_ID')
            and re.fullmatch(r'[1-9][0-9]*', str(origin.get('run_attempt', '')))
            and int(origin['run_attempt']) <= int(environment.get('GITHUB_RUN_ATTEMPT', '0')), 'E2E evidence belongs to another workflow run or attempt')
    automated_policy = copy.deepcopy(policy)
    automated_policy['required_evidence'] = [r for r in policy['required_evidence'] if r['scenario'] != 'disposable-github-publication']
    require(len(report.get('records', [])) == len(automated_policy['required_evidence']), 'missing, duplicate or unexpected automated coverage')
    require(len(report.get('runtime_recovery', [])) == len(policy['required_runtime_recovery']), 'missing runtime recovery coverage')
    value = dict(report, reviewed=True)
    validator().validate_evidence(value, automated_policy, commit, fingerprint)
    images = report.get('images', {})
    require(set(images) == {'amd64', 'arm64'} and all(re.fullmatch(r'sha256:[0-9a-f]{64}', d) for d in images.values()), 'missing exact distribution image identities')
    for row in report['records']:
        invocations = row.get('invocations', [])
        require(invocations and all(r.get('id') and r.get('provider') and r.get('version') == row['provider_versions'].get(r.get('provider')) for r in invocations), 'provider execution identities missing')
        require(len({(r['provider'], r['id']) for r in invocations}) == len(invocations), 'duplicate physical provider invocation')
        if row['scenario'].startswith('mixed-development-'):
            pins = json.loads((ROOT / 'images/pins.json').read_text())
            require(re.search(r'(?<![0-9.])' + re.escape(pins['codex_cli']) + r'(?![0-9.])', row['provider_versions'].get('codex-app-server', '')), 'mixed scenario used another Codex version')
            require({'claude-sdk', 'codex-app-server'} <= {r['provider'] for r in invocations}, 'mixed coverage did not execute both providers')
    for row in report['records'] + report['runtime_recovery']:
        if row.get('runtime'):
            require(row['image_digest'] == images[row['arch']], 'certified runtime used a different distribution image')
    return report


def match_signed(report, automated, **kwargs):
    validate_automated(automated, **kwargs)
    require(report.get('reviewed') is True, 'maintainer must review the sanitized aggregate')
    for key in ['candidate', 'source_fingerprint', 'origin', 'images', 'runtime_recovery']:
        require(report.get(key) == automated.get(key), 'signed report changed automated field: ' + key)
    auto_records = [r for r in report.get('records', []) if r.get('scenario') != 'disposable-github-publication']
    require(auto_records == automated['records'], 'signed automated records differ from immutable release-run artifact')
