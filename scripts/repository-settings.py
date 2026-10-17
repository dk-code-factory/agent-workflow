#!/usr/bin/env python3
"""Print or explicitly apply settings to an existing repository. Never creates it."""
import argparse
import json
import subprocess

REPO = 'dk-code-factory/agent-workflow'
BASE = 'repos/' + REPO
BRANCH = {
    'required_status_checks': {'strict': True, 'contexts': ['Required']},
    'enforce_admins': True,
    'required_pull_request_reviews': {'required_approving_review_count': 0, 'dismiss_stale_reviews': True},
    'restrictions': None,
    'required_conversation_resolution': True,
    'allow_force_pushes': False,
    'allow_deletions': False,
}
RULES = [
    {'name': 'Version tag creation', 'target': 'tag', 'enforcement': 'active',
     'bypass_actors': [{'actor_id': 5, 'actor_type': 'RepositoryRole', 'bypass_mode': 'always'}],
     'conditions': {'ref_name': {'include': ['refs/tags/v*'], 'exclude': []}}, 'rules': [{'type': 'creation'}]},
    {'name': 'Version tag permanence', 'target': 'tag', 'enforcement': 'active',
     'bypass_actors': [], 'conditions': {'ref_name': {'include': ['refs/tags/v*'], 'exclude': []}},
     'rules': [{'type': 'update'}, {'type': 'deletion'}]},
]

def api(method, path, data=None):
    command = ['gh', 'api', '--method', method, path]
    if data is not None:
        command += ['--input', '-']
    result = subprocess.check_output(command, input=json.dumps(data).encode() if data is not None else None)
    return json.loads(result) if result else None

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', help='explicitly change existing repository settings')
    args = parser.parse_args()
    if not args.apply:
        print(json.dumps({'repository': REPO, 'main_protection': BRANCH, 'rulesets': RULES, 'immutable_releases': True}, indent=2))
    else:
        info = api('GET', BASE)
        if info['private'] or info['default_branch'] != 'main':
            raise SystemExit('expected existing public repository with main')
        api('PUT', BASE + '/immutable-releases')
        if not api('GET', BASE + '/immutable-releases')['enabled']:
            raise SystemExit('immutable releases not enabled')
        api('PUT', BASE + '/branches/main/protection', BRANCH)
        existing = api('GET', BASE + '/rulesets?per_page=100')
        for rule in RULES:
            matching = [r for r in existing if r['name'] == rule['name']]
            if len(matching) > 1:
                raise SystemExit('duplicate ruleset name')
            path = BASE + '/rulesets' + ('/' + str(matching[0]['id']) if matching else '')
            api('PUT' if matching else 'POST', path, rule)
        print(json.dumps({'repository': REPO, 'settings_applied': True}))
