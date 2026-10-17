#!/usr/bin/env python3
"""Read-only stable release gate. Does not sign, publish or invoke providers."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

from release_lib import ROOT, git, source_fingerprint

class GateError(ValueError):
    pass

def require(condition, message):
    if not condition:
        raise GateError(message)

def trusted_signature(command, keys, signers):
    with tempfile.TemporaryDirectory(prefix='release-keyring-') as home:
        os.chmod(home, 0o700)
        env = dict(os.environ, GNUPGHOME=home)
        for key in keys:
            subprocess.run(['gpg', '--batch', '--import', str(key)], env=env,
                           check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        result = subprocess.run(command, env=env, cwd=ROOT, capture_output=True, text=True)
        require(result.returncode == 0, 'invalid or missing GPG signature')
        require(not any(code in (result.stdout + result.stderr) for code in
                        ['[GNUPG:] EXPKEYSIG ', '[GNUPG:] EXPSIG ', '[GNUPG:] REVKEYSIG ']),
                'expired or revoked signature')
        valid = [line.split() for line in (result.stdout + result.stderr).splitlines()
                 if line.startswith('[GNUPG:] VALIDSIG ')]
        require(len(valid) == 1 and (valid[0][2] in signers or valid[0][-1] in signers),
                'signature is not from an allowlisted signer')
        return valid[0][2]

def known_version(value):
    return isinstance(value, str) and bool(value.strip()) and value.strip().lower() not in ['unknown', 'unavailable', 'development']

def validate_evidence(report, policy, commit, fingerprint):
    require(report.get('candidate') == commit, 'evidence belongs to another commit')
    require(report.get('source_fingerprint') == fingerprint, 'evidence source fingerprint mismatch')
    require(report.get('reviewed') is True, 'evidence must be reviewed and sanitized')
    records = report.get('records', [])
    require(isinstance(records, list), 'invalid evidence records')
    for expected in policy['required_evidence']:
        matches = [r for r in records if all(r.get(k) == v for k, v in expected.items())]
        require(len(matches) == 1, 'missing or duplicate evidence: ' + json.dumps(expected, sort_keys=True))
        record = matches[0]
        scenario = json.loads((ROOT / 'conformance/scenarios' / (expected['scenario'] + '.json')).read_text())
        require(not record.get('error') and record.get('engine', {}).get('commit') == commit
                and record['engine'].get('dirty') == 'false'
                and record['engine'].get('version') == policy['version']
                and record['engine'].get('source_fingerprint') == fingerprint,
                'failed, dirty or mismatched evidence build')
        require(record.get('configuration_fingerprint') and record.get('started') and record.get('finished'),
                'evidence lacks configuration identity or timestamps')
        require(all(record.get('assertions', {}).get(a) is True for a in scenario['assertions']),
                'failed or incomplete assertions: ' + expected['scenario'])
        versions = record.get('provider_versions', {})
        require(versions and all(known_version(v) for v in versions.values()), 'provider versions missing')
        pins = json.loads((ROOT / 'images/pins.json').read_text())
        if expected['provider'] in ['codex-cli', 'codex-app-server']:
            require(any(re.search(r'(?<![0-9.])' + re.escape(pins['codex_cli']) + r'(?![0-9.])', v) for v in versions.values()), 'evidence uses another Codex protocol version')
        if expected['provider'] == 'claude-sdk':
            require('claude-agent-sdk@' + pins['claude_sdk'] in versions.values(), 'evidence uses another Claude SDK version')
        require('artifacts' not in record, 'remove private artifact paths before signing evidence')
        if expected.get('runtime'):
            require(re.fullmatch(r'sha256:[0-9a-f]{64}', record.get('image_digest', ''))
                    and known_version(record.get('runtime_version')), 'container identity or runtime version missing')
        if scenario.get('publication'):
            require(record.get('publication_repository') in policy['publication_repositories'],
                    'publication evidence target was not designated in release policy')
    recovery = report.get('runtime_recovery', [])
    for expected in policy.get('required_runtime_recovery', []):
        matches = [r for r in recovery if all(r.get(k) == v for k, v in expected.items())]
        require(len(matches) == 1, 'missing runtime recovery certification: ' + json.dumps(expected))
        record = matches[0]
        require(record.get('candidate') == commit and record.get('source_fingerprint') == fingerprint
                and known_version(record.get('runtime_version')) and re.fullmatch(r'sha256:[0-9a-f]{64}', record.get('image_digest', ''))
                and not record.get('error') and 'artifacts' not in record, 'invalid runtime recovery identity')
        require(all(record.get('assertions', {}).get(a) is True for a in [
            'owner_loss', 'runtime_unavailable', 'runtime_restart', 'cleanup_recovery', 'stale_execution_ownership']),
            'incomplete runtime failure-boundary certification')
    return len(policy['required_evidence']) + len(policy.get('required_runtime_recovery', []))

def api(path):
    return json.loads(subprocess.check_output(['gh', 'api', path], text=True,
        env=dict(os.environ, GH_TOKEN=os.environ.get('RELEASE_POLICY_TOKEN') or os.environ.get('GH_TOKEN', ''))))

def existing_release(repo, tag):
    result = subprocess.run(['gh', 'api', 'repos/' + repo + '/releases/tags/' + tag], capture_output=True, text=True)
    if result.returncode:
        require('HTTP 404' in result.stderr, 'cannot inspect existing release')
        return None
    return json.loads(result.stdout)

def validate_remote(policy, commit):
    repo = policy['repository']
    release = existing_release(repo, 'v' + policy['version'])
    require(release is None or (release.get('draft') is True and release.get('target_commitish') == commit),
            'version is already published or its draft targets another commit')
    info = api('repos/' + repo)
    require(info['full_name'] == repo and not info['private'] and info['default_branch'] == 'main',
            'expected public repository with main as default')
    require(api('repos/' + repo + '/immutable-releases').get('enabled') is True,
            'immutable releases must be enabled before publication')
    protection = api('repos/' + repo + '/branches/main/protection')
    reviews = protection.get('required_pull_request_reviews') or {}
    require(reviews.get('required_approving_review_count') == 0
            and protection.get('required_conversation_resolution', {}).get('enabled') is True
            and protection.get('enforce_admins', {}).get('enabled') is True
            and not protection.get('allow_force_pushes', {}).get('enabled', False)
            and not protection.get('allow_deletions', {}).get('enabled', False), 'main protection is incomplete')
    statuses = protection.get('required_status_checks') or {}
    require(statuses.get('strict') is True and 'Required' in statuses.get('contexts', []),
            'main must require the aggregate Required check')
    rules = api('repos/' + repo + '/rulesets?per_page=100')
    rules = [api('repos/' + repo + '/rulesets/' + str(r['id'])) for r in rules]
    tag_rules = [r for r in rules if r.get('target') == 'tag' and r.get('enforcement') == 'active'
                 and r.get('conditions', {}).get('ref_name', {}).get('include') == ['refs/tags/v*']
                 and not r.get('conditions', {}).get('ref_name', {}).get('exclude')]
    require(any({r['type'] for r in rule['rules']} >= {'update', 'deletion'}
                and not rule.get('bypass_actors') for rule in tag_rules), 'tag mutation must have no bypass')
    require(any('creation' in {r['type'] for r in rule['rules']}
                and rule.get('bypass_actors') == [{'actor_id': 5, 'actor_type': 'RepositoryRole', 'bypass_mode': 'always'}]
                for rule in tag_rules), 'tag creation must be restricted to repository administrators')
    # Pin both workflow and commit; an unrelated job named Required cannot certify a release.
    runs = api('repos/' + repo + '/actions/workflows/test.yml/runs?head_sha=' + commit + '&event=push&per_page=100')['workflow_runs']
    require(runs and runs[0]['head_branch'] == 'main' and runs[0]['status'] == 'completed'
            and runs[0]['conclusion'] == 'success', 'current main CI has not passed')

def validate_version(tag, policy, source):
    require(tag == 'v' + policy['version'] and re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', tag), 'stable tag/version mismatch')
    version = re.search(r'const Version = "([^"]+)"', source)
    require(version and version[1] == policy['version'], 'engine version mismatch')

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('tag')
    parser.add_argument('--evidence-ref', default='refs/remotes/origin/release-evidence')
    parser.add_argument('--offline', action='store_true', help='local gates only; never authorizes publication')
    args = parser.parse_args()
    policy = json.loads((ROOT / 'release/policy.json').read_text())
    validate_version(args.tag, policy, (ROOT / 'internal/buildinfo/info.go').read_text())
    require(not git('status', '--porcelain', '--untracked-files=normal'), 'release checkout must be clean')
    require(git('cat-file', '-t', 'refs/tags/' + args.tag) == 'tag', 'release tag must be annotated')
    keys = sorted((ROOT / 'release/keys').glob('*.asc'))
    tag_payload = git('cat-file', 'tag', 'refs/tags/' + args.tag)
    tag_header = dict(line.split(' ', 1) for line in tag_payload.split('\n\n', 1)[0].splitlines())
    require(tag_header.get('tag') == args.tag and tag_header.get('type') == 'commit', 'signed tag name/type mismatch')
    signer = trusted_signature(['git', '-c', 'gpg.format=openpgp', '-c', 'gpg.program=gpg', 'verify-tag', '--raw', 'refs/tags/' + args.tag], keys, policy['signers'])
    commit = git('rev-parse', 'refs/tags/' + args.tag + '^{commit}')
    require(commit == git('rev-parse', 'HEAD'), 'checkout must match tag commit')
    subprocess.run(['git', 'merge-base', '--is-ancestor', commit, 'refs/remotes/origin/main'], cwd=ROOT, check=True)
    fingerprint = source_fingerprint()
    prefix = args.evidence_ref + ':' + commit + '/'
    with tempfile.TemporaryDirectory(prefix='release-evidence-') as directory:
        report = Path(directory) / 'report.json'
        signature = Path(directory) / 'report.json.asc'
        for path in [report, signature]:
            path.write_bytes(subprocess.check_output(['git', 'show', prefix + path.name], cwd=ROOT))
        trusted_signature(['gpg', '--batch', '--status-fd=1', '--verify', str(signature), str(report)], keys, policy['signers'])
        count = validate_evidence(json.loads(report.read_text()), policy, commit, fingerprint)
    if not args.offline:
        validate_remote(policy, commit)
    print(json.dumps({'tag': args.tag, 'commit': commit, 'signer': signer, 'source_fingerprint': fingerprint,
                      'evidence_records': count, 'publication_authorized': not args.offline}))

if __name__ == '__main__':
    try:
        main()
    except (GateError, subprocess.CalledProcessError, OSError, KeyError, ValueError) as error:
        raise SystemExit('release blocked: ' + str(error))
