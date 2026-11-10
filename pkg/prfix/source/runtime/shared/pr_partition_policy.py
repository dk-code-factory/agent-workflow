"""Persistent, user-authored partition preferences for one canonical base PR."""
import copy
import json
from pathlib import Path
import re
from urllib.parse import urlparse

from shared.ci_exceptions import data_root, fingerprint
from shared.private_state import mutate


DEFAULT = {'enabled': True, 'instructions': None}
ACTIONS = ('enable', 'disable', 'instructions', 'clear-instructions', 'reset')


def identity(pr):
    parsed = urlparse(pr or '')
    match = re.fullmatch(r'/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([1-9][0-9]*)/?', parsed.path)
    if (parsed.scheme != 'https' or not parsed.hostname or parsed.netloc != parsed.hostname
            or not re.fullmatch(r'[A-Za-z0-9.-]+', parsed.hostname) or parsed.query or parsed.fragment
            or not match or any(part in ('.', '..') for part in match.groups()[:2])):
        raise ValueError('PR partition policy requires a full HTTPS --pr URL')
    host, owner, repo, number = parsed.hostname.lower(), match[1].lower(), match[2].lower(), match[3]
    return f'https://{host}/{owner}/{repo}/pull/{number}', Path(host, owner, repo, 'pr-' + number + '.json')


def path(pr):
    _, relative = identity(pr)
    return data_root() / 'pr-partition' / relative


def text(value, name):
    if not isinstance(value, str) or not value.strip() or '\x00' in value:
        raise ValueError(name + ' must be nonempty text without NUL bytes')
    return value


def initial(pr):
    return {'schema_version': 1, 'pr_url': identity(pr)[0], 'history': [], 'declines': []}


def transition(previous, action, instructions=None):
    result = dict(previous)
    if action in ('enable', 'disable'):
        result['enabled'] = action == 'enable'
    elif action == 'instructions':
        result.update(enabled=True, instructions=text(instructions, 'partition instructions'))
    elif action == 'clear-instructions':
        result['instructions'] = None
    elif action == 'reset':
        result = dict(DEFAULT)
    else:
        raise ValueError('unknown PR partition policy action')
    return result


def validate(value, pr):
    canonical, _ = identity(pr)
    if (not isinstance(value, dict) or set(value) != {'schema_version', 'pr_url', 'history', 'declines'}
            or type(value['schema_version']) is not int or value['schema_version'] != 1
            or value['pr_url'] != canonical or not isinstance(value['history'], list)
            or not isinstance(value['declines'], list)):
        raise ValueError('malformed PR partition policy; restore its retained decision record')
    previous, policy = None, dict(DEFAULT)
    for number, entry in enumerate(value['history'], 1):
        if (not isinstance(entry, dict) or set(entry) != {
                'revision', 'action', 'policy', 'reason', 'source', 'previous', 'id'}
                or type(entry['revision']) is not int or entry['revision'] != number
                or entry['action'] not in ACTIONS or entry['previous'] != previous):
            raise ValueError('invalid PR partition policy history')
        text(entry['reason'], 'decision reason')
        text(entry['source'], 'decision source')
        desired = entry['policy']
        if not isinstance(desired, dict) or set(desired) != set(DEFAULT) or type(desired['enabled']) is not bool:
            raise ValueError('invalid PR partition preference')
        expected = transition(policy, entry['action'], desired['instructions'])
        if desired != expected or entry['id'] != fingerprint({k: v for k, v in entry.items() if k != 'id'}):
            raise ValueError('PR partition decision changed')
        policy, previous = desired, entry['id']
    seen = set()
    for entry in value['declines']:
        if (not isinstance(entry, dict) or not {'behavior', 'action_id', 'reason', 'source', 'id'} <= set(entry)
                or set(entry) - {'behavior', 'action_id', 'reason', 'source', 'proposal', 'id'}
                or entry['behavior'] not in ('evacuate_unrelated', 'split_related')):
            raise ValueError('invalid recorded partition decline')
        for field in ('action_id', 'reason', 'source'):
            text(entry[field], field)
        if 'proposal' in entry:
            proposal(entry['proposal'])
        if entry['id'] != fingerprint({k: v for k, v in entry.items() if k != 'id'}) or entry['id'] in seen:
            raise ValueError('changed or duplicate partition decline')
        seen.add(entry['id'])
    return value


def load(pr):
    canonical, _ = identity(pr)
    target = path(canonical)
    value = validate(json.loads(target.read_text()), canonical) if target.exists() else initial(canonical)
    decision = value['history'][-1] if value['history'] else None
    policy = decision['policy'] if decision else DEFAULT
    origin = next((entry for entry in reversed(value['history'])
                   if entry['action'] == 'instructions' and entry['policy']['instructions'] == policy['instructions']), None)
    # Action-specific declines are audit evidence, not a new governing preference.
    # Recording one must not stale the in-flight approved candidate set.
    return {**copy.deepcopy(policy), 'pr_url': canonical, 'configured': decision is not None,
            'source': 'pr' if decision else 'default', 'file': str(target), 'decision': decision,
            'instructions_source': ({key: origin[key] for key in ('revision', 'id', 'reason', 'source')}
                                    if policy['instructions'] is not None and origin else None),
            'history': value['history'], 'declines': value['declines'],
            'fingerprint': fingerprint({'pr_url': canonical, 'policy': policy,
                                        'decision': decision['id'] if decision else None})}


def binding(pr):
    value = load(pr)
    return {key: value[key] for key in ('enabled', 'instructions', 'fingerprint')}


def configure(pr, action, *, reason, source=None, instructions=None):
    canonical, _ = identity(pr)
    text(reason, 'decision reason')
    source = text(source if source is not None else (instructions if action == 'instructions' else reason), 'decision source')
    # Validate before creating directories or acquiring a persistent mutation lock.
    transition(DEFAULT, action, instructions)
    def update(value):
        validate(value, canonical)
        previous = value['history'][-1] if value['history'] else None
        policy = transition(previous['policy'] if previous else DEFAULT, action, instructions)
        if previous and all(previous[key] == item for key, item in {
                'action': action, 'policy': policy, 'reason': reason, 'source': source}.items()):
            return  # Idempotent replay of the same explicit user decision.
        entry = {'revision': len(value['history']) + 1, 'action': action, 'policy': policy,
                 'reason': reason, 'source': source, 'previous': previous['id'] if previous else None}
        entry['id'] = fingerprint(entry)
        value['history'].append(entry)
    mutate(path(canonical), initial(canonical), update)
    return load(canonical)


def proposal(value):
    if not isinstance(value, dict) or set(value) != {'state', 'snapshot_key', 'head', 'tree'}:
        raise ValueError('invalid partition decline proposal reference')
    for field, item in value.items():
        text(item, field)
    if not Path(value['state']).is_absolute():
        raise ValueError('partition decline proposal state must be absolute')
    return value


def decline(pr, behavior, action_id, *, reason, source, context=None):
    canonical, _ = identity(pr)
    if behavior not in ('evacuate_unrelated', 'split_related'):
        raise ValueError('unknown partition behavior')
    entry = {'behavior': behavior, 'action_id': text(action_id, 'action_id'),
             'reason': text(reason, 'decision reason'), 'source': text(source, 'decision source')}
    if context is not None:
        entry['proposal'] = proposal(context)
    entry['id'] = fingerprint(entry)
    def update(value):
        validate(value, canonical)
        if entry not in value['declines']:
            value['declines'].append(entry)
    mutate(path(canonical), initial(canonical), update)
    return load(canonical)


def add_parser(sub):
    parser = sub.add_parser('pr-partition', help='Record per-PR partition enablement and human instructions')
    commands = parser.add_subparsers(dest='action', required=True)
    commands.add_parser('show')
    for action in ACTIONS:
        command = commands.add_parser(action)
        command.add_argument('--reason', required=action != 'instructions',
                             default='Use the recorded conditions to decide whether to partition.' if action == 'instructions' else None)
        source = command.add_mutually_exclusive_group()
        source.add_argument('--source', help='Exact human instruction or discussion reference')
        source.add_argument('--source-file', type=Path, help='File containing the exact human instruction')
        if action == 'instructions':
            instruction = command.add_mutually_exclusive_group(required=True)
            instruction.add_argument('--text')
            instruction.add_argument('--file', type=Path)
    return parser


def dispatch(args, pr):
    identity(pr)  # No repository or GitHub lookup is needed for a PR-specific choice.
    if args.action == 'show':
        return load(pr)
    source = args.source_file.read_text() if args.source_file else args.source
    instructions = (args.file.read_text() if args.file else args.text) if args.action == 'instructions' else None
    return configure(pr, args.action, reason=args.reason, source=source, instructions=instructions)
