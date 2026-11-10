"""Private preference catalog and deterministic repository-relative scope matching.

Semantic admission and conditional applicability belong to native agents. Nothing
in this module ranks instructions or infers contradictions.
"""
from copy import deepcopy
import fcntl
import fnmatch
import json
import os
from pathlib import Path
import re
import uuid

from shared.private_state import write
from shared.repository_scope import inside_checkout, repository
from shared.storage_paths import location

VERSION = 1
MODES = ('implementation', 'review')


def catalog_path(checkout):
    target = location('config')/'preferences.json'
    if (target.resolve().is_relative_to(Path(__file__).resolve().parents[1])
            or inside_checkout(target, checkout)):
        raise ValueError('Private preferences must stay outside the plugin and repository')
    return target


def relative_path(value, *, root=False):
    if (not isinstance(value, str) or not value or value.startswith('/')
            or '\\' in value or any(ord(c) < 32 or ord(c) == 127 for c in value)
            or re.match(r'^[A-Za-z]:', value)):
        raise ValueError('Paths must use repository-relative POSIX notation')
    if root and value == '.':
        return value
    if any(part in {'', '.', '..'} for part in value.split('/')):
        raise ValueError('Empty components, dot components and parent traversal are forbidden')
    return value


def selector(value):
    if (not isinstance(value, dict) or set(value) != {'type', 'path'}
            or not isinstance(value['type'], str)
            or value['type'] not in {'file', 'directory', 'glob'}):
        raise ValueError('Selector requires type file, directory or glob and path')
    path = relative_path(value['path'], root=value['type'] == 'directory')
    if value['type'] == 'glob':
        for component in path.split('/'):
            if '**' in component and component != '**':
                raise ValueError('Glob ** must occupy a complete component')
    return {'type': value['type'], 'path': path}


def record(value):
    if (not isinstance(value, dict)
            or set(value) - {'id', 'owner', 'prompt', 'applies_to', 'scope'}
            or not {'id', 'owner', 'prompt', 'scope'} <= value.keys()):
        raise ValueError('Preference requires id, owner, prompt and scope')
    if not isinstance(value['id'], str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', value['id']):
        raise ValueError('Invalid preference id')
    owner = value['owner']
    if not isinstance(owner, dict) or not (
            owner == {'type': 'user'} or
            (set(owner) == {'type', 'repository'} and owner['type'] == 'repo'
             and isinstance(owner['repository'], str)
             and owner['repository'].startswith(('remote:', 'path:'))
             and owner['repository'].split(':', 1)[1])):
        raise ValueError('Owner must identify the user or a checkout repository identity')
    prompt = value['prompt']
    if not isinstance(prompt, str) or not prompt.strip():
        raise ValueError('Instruction must be nonempty')
    modes = value.get('applies_to', list(MODES))
    if (not isinstance(modes, list) or not modes
            or any(not isinstance(mode, str) or mode not in MODES for mode in modes)
            or len(set(modes)) != len(modes)):
        raise ValueError('applies_to must contain implementation and/or review')
    scope = value['scope']
    if (not isinstance(scope, dict) or set(scope) - {'include', 'exclude', 'condition'}
            or 'include' not in scope):
        raise ValueError('Scope requires include, optional exclude and condition')
    normalized = {}
    for key in ('include', 'exclude'):
        if not isinstance(scope.get(key, []), list):
            raise ValueError(key + ' must be a selector list')
        items = [selector(item) for item in scope.get(key, [])]
        normalized[key] = [dict(type=kind, path=path) for kind, path in
                           sorted({(item['type'], item['path']) for item in items})]
    condition = scope.get('condition')
    if condition is not None and (not isinstance(condition, str) or not condition.strip()):
        raise ValueError('Condition must be null or nonempty text')
    normalized['condition'] = condition.strip() if condition is not None else None
    if not normalized['include'] and condition is None:
        raise ValueError('Prompt-only scope requires a condition')
    return {'id': value['id'], 'owner': deepcopy(owner), 'prompt': prompt.strip(),
            'applies_to': sorted(modes), 'scope': normalized}


def _signature(value):
    return json.dumps({k: v for k, v in value.items() if k != 'id'}, sort_keys=True)


def _catalog(value):
    if (not isinstance(value, dict) or set(value) != {'version', 'revision', 'records'}
            or type(value['version']) is not int or value['version'] != VERSION
            or type(value['revision']) is not int or value['revision'] < 0
            or not isinstance(value['records'], list)):
        raise ValueError('Malformed or unsupported preference catalog')
    records = [record(item) for item in value['records']]
    if len({r['id'] for r in records}) != len(records):
        raise ValueError('Duplicate preference ids')
    if len({_signature(r) for r in records}) != len(records):
        raise ValueError('Duplicate preference records')
    return {**value, 'records': records}


def _read(checkout):
    path = catalog_path(checkout)
    # Only a missing catalog starts empty; corrupt catalogs are never reset.
    try:
        content = path.read_text(encoding='utf-8')
    except FileNotFoundError:
        return {'version': VERSION, 'revision': 0, 'records': []}
    return _catalog(json.loads(content))


def _owned(item, identity):
    return item['owner'] in ({'type': 'user'}, {'type': 'repo', 'repository': identity})


def list_records(checkout):
    identity = repository(checkout)
    catalog = _read(checkout)
    return {'repository': identity, 'revision': catalog['revision'],
            'records': sorted((r for r in catalog['records'] if _owned(r, identity)), key=lambda r: r['id'])}


def show(checkout, preference_id):
    context = list_records(checkout)
    for item in context['records']:
        if item['id'] == preference_id:
            return {'revision': context['revision'], 'record': item}
    raise ValueError('Unknown preference in current repository context: ' + str(preference_id))


def _apply(catalog, changes, identity):
    if (not isinstance(changes, dict) or set(changes) - {'upsert', 'remove'}
            or any(not isinstance(changes.get(key, []), list) for key in ('upsert', 'remove'))):
        raise ValueError('Changes require upsert and/or remove lists')
    upsert = [record(item) for item in changes.get('upsert', [])]
    remove = changes.get('remove', [])
    ids = [r['id'] for r in upsert] + remove
    if any(not isinstance(v, str) for v in ids) or len(set(ids)) != len(ids):
        raise ValueError('Each changed id must appear exactly once')
    current = {r['id']: r for r in catalog['records']}
    for preference_id in ids:
        if preference_id in current and not _owned(current[preference_id], identity):
            raise ValueError('Cannot change another repository preference')
    if any(v not in current for v in remove):
        raise ValueError('Cannot remove an unknown preference')
    if any(not _owned(r, identity) for r in upsert):
        raise ValueError('Admission is limited to personal and current repository preferences')
    proposed = {k: v for k, v in current.items() if k not in ids}
    # Keep an unchanged existing record when an update duplicates it.
    signatures = {_signature(r): r['id'] for r in proposed.values()}
    reused = []
    for item in upsert:
        signature = _signature(item)
        if signature in signatures:
            reused.append({'proposed_id': item['id'], 'id': signatures[signature]})
        else:
            proposed[item['id']] = item
            signatures[signature] = item['id']
    return sorted(proposed.values(), key=lambda r: r['id']), reused


def prepare(checkout, changes):
    """Normalize a proposal; the skill must admit it against returned context."""
    identity = repository(checkout)
    catalog = _read(checkout)
    changes = deepcopy(changes)
    if isinstance(changes, dict) and isinstance(changes.get('upsert', []), list):
        import skill_config
        modes = skill_config.for_context('dk-workflows:dk-preferences', cwd=checkout)['options']['default_applies_to']
        current = {item['id']: item for item in catalog['records']}
        for item in changes.get('upsert', []):
            if isinstance(item, dict) and 'applies_to' not in item:
                # Defaults affect admission, never interpretation of saved records.
                item['applies_to'] = deepcopy(current.get(item.get('id'), {}).get('applies_to', modes))
            if isinstance(item, dict) and 'id' not in item:
                item['id'] = 'pref-' + uuid.uuid4().hex
    proposed, reused = _apply(catalog, changes, identity)
    before = {r['id']: r for r in catalog['records']}
    after = {r['id']: r for r in proposed}
    return {'repository': identity, 'revision': catalog['revision'],
            'context': {'records': sorted((r for r in catalog['records'] if _owned(r, identity)),
                                          key=lambda r: r['id'])},
            'changes': {'upsert': [r for r in proposed if before.get(r['id']) != r],
                        'remove': sorted(before.keys() - after.keys())}, 'reused': reused}


def commit(checkout, proposal):
    """Compare revision under the catalog lock and atomically save all changes."""
    identity = repository(checkout)
    if (not isinstance(proposal, dict) or proposal.get('repository') != identity
            or set(proposal) - {'repository', 'revision', 'changes', 'context', 'reused'}
            or type(proposal.get('revision')) is not int or proposal['revision'] < 0
            or 'changes' not in proposal):
        raise ValueError('Commit requires prepared repository, revision and changes')
    duplicate_reuse = proposal.get('reused', [])
    if (not isinstance(duplicate_reuse, list) or any(
            not isinstance(item, dict) or set(item) != {'proposed_id', 'id'}
            or any(not isinstance(value, str) or not value for value in item.values())
            for item in duplicate_reuse)):
        raise ValueError('Malformed duplicate reuse metadata')
    target = catalog_path(checkout)
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock_path = target.with_suffix('.lock')
    fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o600)
    with os.fdopen(fd, 'r+') as lock:
        os.fchmod(lock.fileno(), 0o600)
        fcntl.flock(lock, fcntl.LOCK_EX)
        catalog = _read(checkout)
        if catalog['revision'] != proposal['revision']:
            raise ValueError('Stale catalog revision; prepare and repeat admission against fresh context')
        records, reused = _apply(catalog, proposal['changes'], identity)
        if records != sorted(catalog['records'], key=lambda r: r['id']):
            catalog = {**catalog, 'revision': catalog['revision'] + 1, 'records': records}
            write(target, catalog)
        return {'repository': identity, 'revision': catalog['revision'],
                'records': [r for r in records if _owned(r, identity)],
                'changes': deepcopy(proposal['changes']), 'reused': duplicate_reuse + reused}


def remove(checkout, ids, revision):
    return commit(checkout, {'repository': repository(checkout), 'revision': revision,
                             'changes': {'remove': ids}})


def matches(item, path):
    """Root-anchored component matching; ** spans zero or more directories."""
    kind, pattern = item['type'], item['path']
    if kind == 'file':
        return path == pattern
    if kind == 'directory':
        return pattern == '.' or path == pattern or path.startswith(pattern + '/')
    parts = path.split('/')
    positions = {0}
    for component in pattern.split('/'):
        if component == '**':
            positions = set(range(min(positions), len(parts) + 1))
        else:
            positions = {index + 1 for index in positions
                         if index < len(parts) and fnmatch.fnmatchcase(parts[index], component)}
        if not positions:
            return False
    return len(parts) in positions


def task_paths(values):
    """A rename contributes both endpoints, even when either no longer exists."""
    if not isinstance(values, list):
        raise ValueError('Task paths must be a list')
    result = set()
    for value in values:
        if isinstance(value, str):
            result.add(relative_path(value))
        elif isinstance(value, dict) and set(value) == {'old_path', 'new_path'}:
            result.update(relative_path(value[key]) for key in ('old_path', 'new_path'))
        else:
            raise ValueError('Task paths require strings or old_path/new_path rename pairs')
    return sorted(result)


def resolve(checkout, mode, paths):
    if mode not in MODES:
        raise ValueError('Unknown preference application mode')
    paths = task_paths(paths)
    context = list_records(checkout)
    selected = []
    for item in context['records']:
        if mode not in item['applies_to']:
            continue
        scope = item['scope']
        matched = [path for path in paths
                   if (not scope['include'] or any(matches(s, path) for s in scope['include']))
                   and not any(matches(s, path) for s in scope['exclude'])]
        if matched or (not paths and not scope['include']):
            selected.append({'record': item, 'paths': matched,
                             'requires_condition': scope['condition'] is not None})
    # Deliberately omit catalog revision: unrelated edits must not invalidate work.
    return {'repository': context['repository'], 'mode': mode, 'paths': paths, 'records': selected}


def handoff(checkout, mode, paths, condition_context, *, previous=None, decisions=()):
    """Build task inputs, asking only for new or changed conditional decisions."""
    selection = resolve(checkout, mode, paths)
    previous = previous or {}
    if not isinstance(previous, dict) or not isinstance(decisions, (list, tuple)):
        raise ValueError('Handoff requires a previous handoff object and decision list')
    old_selection = previous.get('selection', {})
    if not isinstance(old_selection, dict):
        raise ValueError('Malformed previous preference selection')
    old_records = {item['record']['id']: item for item in old_selection.get('records', [])}
    old_decisions = {item['id']: item for item in previous.get('decisions', [])}
    provided = {item['id']: item for item in decisions}
    if len(provided) != len(decisions):
        raise ValueError('Duplicate preference condition decision')
    conditional = {item['record']['id']: item for item in selection['records'] if item['requires_condition']}
    if provided.keys() - conditional.keys():
        raise ValueError('Decision does not identify a current conditional preference')
    retained, pending = [], []
    same_context = (old_selection.get('repository') == selection['repository']
                    and old_selection.get('mode') == selection['mode']
                    and previous.get('condition_context') == condition_context)
    for preference_id, item in conditional.items():
        decision = provided.get(preference_id)
        if decision is None and same_context and old_records.get(preference_id) == item:
            decision = old_decisions.get(preference_id)
        if decision is None:
            pending.append(item)
            continue
        if (not isinstance(decision, dict) or set(decision) != {'id', 'applicable', 'paths', 'reason'}
                or type(decision['applicable']) is not bool
                or not isinstance(decision['paths'], list)
                or any(not isinstance(path, str) for path in decision['paths'])
                or len(set(decision['paths'])) != len(decision['paths'])
                or set(decision['paths']) - set(item['paths'])
                or (not decision['applicable'] and decision['paths'])
                or (decision['applicable'] and item['paths'] and not decision['paths'])
                or not isinstance(decision['reason'], str) or not decision['reason'].strip()):
            raise ValueError('Condition decision requires applicability, scoped paths and a reason')
        retained.append(deepcopy(decision))
    value = {'selection': selection, 'condition_context': deepcopy(condition_context), 'decisions': retained}
    return {'complete': not pending, 'handoff': value, 'pending': pending}


def render(value):
    """Render persisted records as readable Markdown, without semantic rewriting."""
    if isinstance(value, dict) and 'record' in value:
        values = [value['record']]
    elif isinstance(value, dict) and 'records' in value:
        values = [v['record'] if isinstance(v, dict) and 'record' in v else v for v in value['records']]
    else:
        raise ValueError('Render expects a show, list, commit or resolve result')
    sections = []
    for item in values:
        item = record(item)
        scope = item['scope']
        owner = 'user' if item['owner']['type'] == 'user' else item['owner']['repository']
        lines = ['### ' + item['id'], '', '- Owner: ' + owner,
                 '- Applies to: ' + ', '.join(item['applies_to'])]
        for key in ('include', 'exclude'):
            selectors = '; '.join(s['type'] + ': `' + s['path'].replace('`', '&#96;') + '`'
                                  for s in scope[key])
            lines.append('- ' + key.capitalize() + ': ' + (selectors or ('prompt-only' if key == 'include' else 'none')))
        lines += ['- Condition: ' + (scope['condition'] or 'none'), '', '**Instruction**', '', item['prompt']]
        sections.append('\n'.join(lines))
    return '\n\n'.join(sections) + '\n' if sections else 'No saved preferences.\n'
