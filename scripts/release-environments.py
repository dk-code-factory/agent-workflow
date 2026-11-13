#!/usr/bin/env python3
"""Set up tag-only release environments; credentials only travel through stdin."""
import argparse
import json
from pathlib import Path
import subprocess
from urllib.parse import quote

REPO = 'dk-code-factory/agent-workflow'
CODEX = Path('/home/dkropachev/.codex/accounts/dmitry.kropachev.dev+codex+1/auth.json')
CLAUDE = Path('/home/dkropachev/.config/agent-workflow/claude-oauth-token')

def api(method, path, data=None, missing=False):
    command = ['gh', 'api', '--method', method, path]
    if data is not None:
        command += ['--input', '-']
    result = subprocess.run(command, input=json.dumps(data) if data is not None else None, text=True, capture_output=True)
    if missing and result.returncode and 'HTTP 404' in result.stderr:
        return None
    if result.returncode:
        raise ValueError('GitHub environment operation failed: ' + method + ' ' + path)
    return json.loads(result.stdout) if result.stdout.strip() else None

def pages(path, field):
    result = []
    page = 1
    while True:
        values = api('GET', path + ('&' if '?' in path else '?') + 'per_page=100&page=' + str(page))[field]
        result.extend(values)
        if len(values) < 100:
            return result
        page += 1

def configure(name, reviewer=None):
    path = 'repos/' + REPO + '/environments/' + quote(name, safe='')
    current = api('GET', path, missing=True) or {}
    rules = current.get('protection_rules', [])
    # Custom protection rules are not overwritten by the environment PUT API.
    body = {'deployment_branch_policy': {'protected_branches': False, 'custom_branch_policies': True},
            'can_admins_bypass': current.get('can_admins_bypass', False)}
    for rule in rules:
        if rule['type'] == 'wait_timer':
            body['wait_timer'] = rule['wait_timer']
        if rule['type'] == 'required_reviewers':
            body['reviewers'] = [{'type': r['type'], 'id': r['reviewer']['id']} for r in rule['reviewers']]
            body['prevent_self_review'] = rule.get('prevent_self_review', False)
    if reviewer:
        reviewers = body.setdefault('reviewers', [])
        entry = {'type': 'User', 'id': reviewer}
        if entry not in reviewers:
            reviewers.append(entry)
        body['prevent_self_review'] = False
    existing = pages(path + '/deployment-branch-policies', 'branch_policies') if (current.get('deployment_branch_policy') or {}).get('custom_branch_policies') else []
    if any(r.get('type') != 'tag' or r['name'] != 'v*' for r in existing):
        raise ValueError(name + ' has unrelated deployment policies; reconcile them explicitly before setup')
    api('PUT', path, body)
    if not existing:
        api('POST', path + '/deployment-branch-policies', {'name': 'v*', 'type': 'tag'})
    return path

def upload(name, path):
    try:
        value = path.read_bytes()
    except OSError:
        raise ValueError('credential file unavailable for ' + name) from None
    if not value.strip():
        raise ValueError('credential file empty for ' + name)
    # Never include credentials in argv, errors, logs or environment variables.
    result = subprocess.run(['gh', 'secret', 'set', name, '--repo', REPO, '--env', 'pre-release'], input=value, capture_output=True)
    if result.returncode:
        raise ValueError('secret upload failed for ' + name)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--replace-credentials', action='store_true')
    parser.add_argument('--codex-auth-file', type=Path, default=CODEX)
    parser.add_argument('--claude-token-file', type=Path, default=CLAUDE)
    args = parser.parse_args()
    if not args.apply:
        print('Configure pre-release (two subscription secrets) and release-certification (dkropachev, self-review allowed); tag policies v*. Use --apply; existing secrets require --replace-credentials.')
        return
    info = api('GET', 'repos/' + REPO)
    if info['full_name'] != REPO or not info['permissions'].get('admin'):
        raise ValueError('repository administrator access required')
    reviewer = api('GET', 'users/dkropachev')['id']
    path = configure('pre-release')
    configure('release-certification', reviewer)
    names = {s['name'] for s in pages(path + '/secrets', 'secrets')}
    for name, file in [('CODEX_AUTH_JSON', args.codex_auth_file), ('CLAUDE_CODE_OAUTH_TOKEN', args.claude_token_file)]:
        if name not in names or args.replace_credentials:
            upload(name, file)
    print('Release environments configured; existing credentials preserved unless replacement was requested.')

if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError) as error:
        raise SystemExit('environment setup blocked: ' + str(error))
