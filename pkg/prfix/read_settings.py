"""Read-only bridge to pinned 5.2.0 readers. Never imports plugin run histories."""
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).parent / 'source/runtime'))
import skill_config
import config
import preferences
import settings
from shared import ci_exceptions, pr_partition_policy, pr_size

request = json.load(sys.stdin)
repository, upstream = request['repository'], request.get('upstream') or None
options = request.get('options') or {}
children, visiting = {}, set()

def resolve(identity, local=None):
    loaded = skill_config.effective(identity, repository, upstream, options=local or {})
    signature = json.dumps([identity, loaded['options']], sort_keys=True)
    if signature in visiting:
        raise ValueError('Recursive selection: ' + identity)
    visiting.add(signature)
    groups = {}
    for kind in ('tasks', 'finalizers', 'reviewers'):
        if kind not in loaded['options']:
            continue
        selected = skill_config.plan(identity, kind, repository, upstream, options=local or {}, resolved=True)
        groups[kind] = selected['resolved_groups']
        for group in groups[kind]:
            for child in group:
                call = child['invocation']
                if 'skill' in call:
                    resolve(call['skill'], call.get('options', {}))
    policy = loaded['options'].get('policy')
    if isinstance(policy, dict) and 'prompt' not in policy:
        call = skill_config.configured_call(policy, repository, upstream)
        if 'skill' not in call:
            call = dict(call, skill='dk-workflows:dk-commit-split', options={'policy': None})
        conf, _ = resolve(call['skill'], call.get('options', {}))
        groups['policy'] = [[{'invocation': call, 'configuration': conf}]]
    children[signature] = groups
    visiting.remove(signature)
    return loaded, groups

root_call = skill_config.configured_call({'skill': 'dk-workflows:dk-pr-fix', 'options': options}, repository, upstream)
loaded, groups = resolve('dk-workflows:dk-pr-fix', options)
legacy = config.effective(repository, upstream)['settings']
hooks = {}
for phase, entries in legacy['processes']['pr-fix'].get('hooks', {}).items():
    hooks[phase] = []
    for group in skill_config.groups(entries):
        values = []
        for raw in group:
            call = skill_config.configured_call(raw, repository, upstream)
            conf = None
            if 'skill' in call:
                conf, _ = resolve(call['skill'], call.get('options', {}))
            values.append({'invocation': call, 'configuration': conf})
        hooks[phase].append(values)
# Read original stores in place. The handoff retains conditional records; the
# assigned leaf must provide sourced applicability decisions before acceptance.
prefs = {mode: preferences.resolve(request['checkout'], mode, request['paths'])
         for mode in ('implementation', 'review')}
chain = ['remote:' + repository] + (['remote:' + upstream] if upstream else [])
domains = {domain: settings.domain_load(domain, chain)
           for domain in ('rules', 'drafting', 'completion', 'commit-policy')}
domains['pr_size'] = pr_size.load(chain)
domains['ci_exceptions'] = ci_exceptions.load('remote:' + repository)
domains['partition'] = pr_partition_policy.load(request['pr'])
domains['legacy_process'] = legacy['processes']['pr-fix']
print(json.dumps({'invocation': root_call, 'config': loaded, 'tasks': groups.get('tasks', []),
                  'finalizers': groups.get('finalizers', []), 'children': children,
                  'hooks': hooks, 'accounts': legacy['accounts'].get('personal', []),
                  'preferences': prefs, 'settings': domains}, sort_keys=True))
